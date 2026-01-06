package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rophy/kube-soomkiller/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Config holds controller configuration
type Config struct {
	NodeName         string
	PollInterval     time.Duration
	TCQueueThreshold int
	PSIThreshold     float64
	DryRun           bool
	K8sClient        *kubernetes.Clientset
	Metrics          *metrics.Collector
}

// Controller monitors swap pressure and terminates pods when necessary
type Controller struct {
	config Config
}

// PodCandidate represents a pod that may be terminated
type PodCandidate struct {
	Namespace   string
	Name        string
	CgroupPath  string
	SwapBytes   int64
	PSIFullAvg10 float64
}

// New creates a new controller
func New(config Config) *Controller {
	return &Controller{
		config: config,
	}
}

// Run starts the controller main loop
func (c *Controller) Run(ctx context.Context) error {
	klog.Infof("Controller started, polling every %s", c.config.PollInterval)

	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.reconcile(ctx); err != nil {
				klog.Errorf("Reconcile error: %v", err)
			}
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	// Get tc queue stats
	tcStats, err := c.config.Metrics.GetTCStats()
	if err != nil {
		return fmt.Errorf("failed to get tc stats: %w", err)
	}

	klog.V(2).Infof("tc stats: backlog=%d bytes (%d pkts), dropped=%d, overlimits=%d",
		tcStats.Backlog, tcStats.BacklogPkts, tcStats.Dropped, tcStats.Overlimits)

	// Check if tc queue threshold is exceeded
	if tcStats.BacklogPkts < int64(c.config.TCQueueThreshold) {
		klog.V(2).Infof("tc queue (%d pkts) below threshold (%d), no action needed",
			tcStats.BacklogPkts, c.config.TCQueueThreshold)
		return nil
	}

	klog.Warningf("tc queue threshold exceeded: %d pkts >= %d threshold",
		tcStats.BacklogPkts, c.config.TCQueueThreshold)

	// Find pods using swap
	candidates, err := c.findCandidates(ctx)
	if err != nil {
		return fmt.Errorf("failed to find candidates: %w", err)
	}

	if len(candidates) == 0 {
		klog.Warning("tc queue is full but no pods using swap found")
		return nil
	}

	// Select victim (highest PSI full avg10 among swap users)
	victim := c.selectVictim(candidates)

	klog.Warningf("Selected victim: %s/%s (swap=%d MB, PSI full avg10=%.2f)",
		victim.Namespace, victim.Name, victim.SwapBytes/1024/1024, victim.PSIFullAvg10)

	// Terminate the pod
	if err := c.terminatePod(ctx, victim); err != nil {
		return fmt.Errorf("failed to terminate pod: %w", err)
	}

	return nil
}

func (c *Controller) findCandidates(ctx context.Context) ([]PodCandidate, error) {
	var candidates []PodCandidate

	// Get all pods on this node
	pods, err := c.config.K8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", c.config.NodeName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Find pod cgroups
	cgroups, err := c.config.Metrics.FindPodCgroups()
	if err != nil {
		klog.Warningf("Failed to find pod cgroups: %v", err)
		return nil, nil
	}

	// Build a map of container ID to pod info
	containerToPod := make(map[string]struct {
		Namespace string
		Name      string
	})

	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID != "" {
				// ContainerID format: containerd://abc123...
				containerID := cs.ContainerID
				if idx := len("containerd://"); len(containerID) > idx {
					containerID = containerID[idx:]
				}
				// Use first 12 chars for matching
				if len(containerID) > 12 {
					containerID = containerID[:12]
				}
				containerToPod[containerID] = struct {
					Namespace string
					Name      string
				}{pod.Namespace, pod.Name}
			}
		}
	}

	// Check each cgroup for swap usage
	for _, cgroupPath := range cgroups {
		podMetrics, err := c.config.Metrics.GetPodMetrics(cgroupPath)
		if err != nil {
			klog.V(2).Infof("Failed to get metrics for %s: %v", cgroupPath, err)
			continue
		}

		// Skip if not using swap
		if podMetrics.SwapCurrent == 0 {
			continue
		}

		// Extract container ID from cgroup path
		// Format: kubepods.slice/kubepods-burstable.slice/.../cri-containerd-<id>.scope
		containerID := extractContainerID(cgroupPath)
		if containerID == "" {
			continue
		}

		podInfo, ok := containerToPod[containerID]
		if !ok {
			klog.V(2).Infof("Could not find pod for container %s", containerID)
			continue
		}

		candidates = append(candidates, PodCandidate{
			Namespace:    podInfo.Namespace,
			Name:         podInfo.Name,
			CgroupPath:   cgroupPath,
			SwapBytes:    podMetrics.SwapCurrent,
			PSIFullAvg10: podMetrics.PSI.FullAvg10,
		})
	}

	return candidates, nil
}

func extractContainerID(cgroupPath string) string {
	// Path: kubepods.slice/.../cri-containerd-<64-char-id>.scope
	const prefix = "cri-containerd-"
	const suffix = ".scope"

	idx := 0
	for i := len(cgroupPath) - 1; i >= 0; i-- {
		if cgroupPath[i] == '/' {
			idx = i + 1
			break
		}
	}

	name := cgroupPath[idx:]
	if len(name) > len(prefix)+len(suffix) &&
		name[:len(prefix)] == prefix &&
		name[len(name)-len(suffix):] == suffix {
		fullID := name[len(prefix) : len(name)-len(suffix)]
		if len(fullID) > 12 {
			return fullID[:12]
		}
		return fullID
	}

	return ""
}

func (c *Controller) selectVictim(candidates []PodCandidate) PodCandidate {
	// Sort by PSI full avg10 (descending)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].PSIFullAvg10 > candidates[j].PSIFullAvg10
	})

	// Log all candidates
	klog.Infof("Candidates for termination (%d total):", len(candidates))
	for i, cand := range candidates {
		klog.Infof("  %d. %s/%s: swap=%d MB, PSI=%.2f",
			i+1, cand.Namespace, cand.Name, cand.SwapBytes/1024/1024, cand.PSIFullAvg10)
	}

	return candidates[0]
}

func (c *Controller) terminatePod(ctx context.Context, pod PodCandidate) error {
	if c.config.DryRun {
		klog.Infof("[DRY-RUN] Would delete pod %s/%s", pod.Namespace, pod.Name)
		return nil
	}

	klog.Warningf("Deleting pod %s/%s to relieve swap pressure", pod.Namespace, pod.Name)

	err := c.config.K8sClient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	klog.Infof("Successfully deleted pod %s/%s", pod.Namespace, pod.Name)
	return nil
}
