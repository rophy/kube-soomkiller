package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rophy/kube-soomkiller/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Config holds controller configuration
type Config struct {
	NodeName          string
	PollInterval      time.Duration
	SwapIOThreshold   int           // pages/sec to trigger action
	SustainedDuration time.Duration // how long threshold must be exceeded
	CooldownPeriod    time.Duration // wait time after killing a pod
	PSIThreshold      float64       // minimum PSI for pod selection
	DryRun            bool
	K8sClient         *kubernetes.Clientset
	Metrics           *metrics.Collector
}

// Controller monitors swap pressure and terminates pods when necessary
type Controller struct {
	config Config

	// State tracking
	lastSwapIO        *metrics.SwapIOStats
	lastSampleTime    time.Time
	thresholdExceeded time.Time // when threshold was first exceeded
	lastKillTime      time.Time // when we last killed a pod
}

// PodCandidate represents a pod that may be terminated
type PodCandidate struct {
	Namespace    string
	Name         string
	CgroupPath   string
	SwapBytes    int64
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
	klog.Infof("Trigger: swap I/O > %d pages/sec for %s",
		c.config.SwapIOThreshold, c.config.SustainedDuration)

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
	now := time.Now()

	// Get current swap I/O stats
	swapIO, err := c.config.Metrics.GetSwapIOStats()
	if err != nil {
		return fmt.Errorf("failed to get swap I/O stats: %w", err)
	}

	// Calculate swap I/O rate if we have a previous sample
	var swapIORate float64
	if c.lastSwapIO != nil {
		elapsed := now.Sub(c.lastSampleTime).Seconds()
		if elapsed > 0 {
			pswpInDelta := swapIO.PswpIn - c.lastSwapIO.PswpIn
			pswpOutDelta := swapIO.PswpOut - c.lastSwapIO.PswpOut
			swapIORate = float64(pswpInDelta+pswpOutDelta) / elapsed
		}
	}

	// Update last sample
	c.lastSwapIO = swapIO
	c.lastSampleTime = now

	klog.V(2).Infof("Swap I/O: pswpin=%d, pswpout=%d, rate=%.1f pages/sec",
		swapIO.PswpIn, swapIO.PswpOut, swapIORate)

	// Update Prometheus metrics
	metrics.SwapIORate.Set(swapIORate)

	// Check if in cooldown period
	if !c.lastKillTime.IsZero() && now.Sub(c.lastKillTime) < c.config.CooldownPeriod {
		remaining := c.config.CooldownPeriod - now.Sub(c.lastKillTime)
		metrics.CooldownRemaining.Set(remaining.Seconds())
		klog.V(2).Infof("In cooldown period, %s remaining", remaining.Round(time.Second))
		return nil
	}
	metrics.CooldownRemaining.Set(0)

	// Check if swap I/O rate exceeds threshold
	if swapIORate < float64(c.config.SwapIOThreshold) {
		// Reset threshold exceeded time
		c.thresholdExceeded = time.Time{}
		metrics.SwapIOThresholdExceeded.Set(0)
		metrics.SwapIOThresholdExceededDuration.Set(0)
		klog.V(2).Infof("Swap I/O rate (%.1f) below threshold (%d), no action needed",
			swapIORate, c.config.SwapIOThreshold)
		return nil
	}

	// Threshold exceeded
	metrics.SwapIOThresholdExceeded.Set(1)

	// Threshold exceeded - track when it started
	if c.thresholdExceeded.IsZero() {
		c.thresholdExceeded = now
		klog.Infof("Swap I/O threshold exceeded: %.1f pages/sec >= %d threshold",
			swapIORate, c.config.SwapIOThreshold)
	}

	// Check if sustained long enough
	sustainedFor := now.Sub(c.thresholdExceeded)
	metrics.SwapIOThresholdExceededDuration.Set(sustainedFor.Seconds())

	if sustainedFor < c.config.SustainedDuration {
		klog.Infof("Threshold exceeded for %s (need %s), waiting...",
			sustainedFor.Round(time.Second), c.config.SustainedDuration)
		return nil
	}

	klog.Warningf("Swap I/O threshold exceeded for %s: %.1f pages/sec >= %d threshold",
		sustainedFor.Round(time.Second), swapIORate, c.config.SwapIOThreshold)

	// Find pods using swap
	candidates, err := c.findCandidates(ctx)
	if err != nil {
		return fmt.Errorf("failed to find candidates: %w", err)
	}

	// Update candidate count metric
	metrics.CandidatePodsCount.Set(float64(len(candidates)))

	// Update per-pod metrics
	metrics.ResetPodMetrics()
	for _, cand := range candidates {
		metrics.PodSwapBytes.WithLabelValues(cand.Namespace, cand.Name).Set(float64(cand.SwapBytes))
		metrics.PodPSIFullAvg10.WithLabelValues(cand.Namespace, cand.Name).Set(cand.PSIFullAvg10)
	}

	if len(candidates) == 0 {
		klog.Warning("Swap I/O is high but no pods using swap found")
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

	// Update state after successful kill
	c.lastKillTime = now
	c.thresholdExceeded = time.Time{} // Reset to re-evaluate after cooldown

	// Update Prometheus metrics
	metrics.PodsKilledTotal.Inc()
	metrics.LastKillTimestamp.Set(float64(now.Unix()))
	metrics.SwapIOThresholdExceeded.Set(0)
	metrics.SwapIOThresholdExceededDuration.Set(0)

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

	// Build a map of container ID (first 12 chars) to pod info for Burstable pods only
	type podInfo struct {
		Namespace string
		Name      string
	}
	containerToPod := make(map[string]podInfo)

	for _, pod := range pods.Items {
		// Only consider Burstable pods - they're the only ones that get swap in LimitedSwap mode
		if pod.Status.QOSClass != corev1.PodQOSBurstable {
			klog.V(3).Infof("Skipping pod %s/%s: QoS class is %s (not Burstable)",
				pod.Namespace, pod.Name, pod.Status.QOSClass)
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID == "" {
				continue
			}
			// Extract container ID from "containerd://abc123..." or "cri-o://abc123..."
			containerID := extractContainerIDFromStatus(cs.ContainerID)
			if containerID == "" {
				continue
			}
			// Use first 12 chars for matching (standard short ID)
			if len(containerID) > 12 {
				containerID = containerID[:12]
			}
			containerToPod[containerID] = podInfo{
				Namespace: pod.Namespace,
				Name:      pod.Name,
			}
		}
	}

	// Find all container cgroups via filesystem walk
	cgroups, err := c.config.Metrics.FindPodCgroups()
	if err != nil {
		klog.Warningf("Failed to find pod cgroups: %v", err)
		return nil, nil
	}

	// Track processed pods to avoid duplicates (multiple containers per pod)
	processedPods := make(map[string]*PodCandidate)

	for _, cgroupPath := range cgroups {
		// Extract container ID from cgroup path
		containerID := extractContainerIDFromCgroup(cgroupPath)
		if containerID == "" {
			continue
		}

		pod, ok := containerToPod[containerID]
		if !ok {
			klog.V(3).Infof("Container %s not in Burstable pod list, skipping", containerID)
			continue
		}

		podMetrics, err := c.config.Metrics.GetPodMetrics(cgroupPath)
		if err != nil {
			klog.V(2).Infof("Failed to get metrics for %s: %v", cgroupPath, err)
			continue
		}

		// Skip if not using swap
		if podMetrics.SwapCurrent == 0 {
			continue
		}

		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		if existing, ok := processedPods[podKey]; ok {
			// Aggregate metrics across containers in the same pod
			existing.SwapBytes += podMetrics.SwapCurrent
			if podMetrics.PSI.FullAvg10 > existing.PSIFullAvg10 {
				existing.PSIFullAvg10 = podMetrics.PSI.FullAvg10
			}
		} else {
			processedPods[podKey] = &PodCandidate{
				Namespace:    pod.Namespace,
				Name:         pod.Name,
				CgroupPath:   cgroupPath,
				SwapBytes:    podMetrics.SwapCurrent,
				PSIFullAvg10: podMetrics.PSI.FullAvg10,
			}
		}
	}

	// Convert map to slice
	for _, cand := range processedPods {
		candidates = append(candidates, *cand)
	}

	return candidates, nil
}

// extractContainerIDFromStatus extracts container ID from Kubernetes container status
// Input format: "containerd://abc123..." or "cri-o://abc123..."
func extractContainerIDFromStatus(containerID string) string {
	idx := strings.Index(containerID, "://")
	if idx == -1 {
		return ""
	}
	return containerID[idx+3:]
}

// extractContainerIDFromCgroup extracts the first 12 chars of container ID from cgroup path
// Input: kubepods.slice/.../cri-containerd-<64-char-id>.scope or crio-<64-char-id>.scope
func extractContainerIDFromCgroup(cgroupPath string) string {
	// Find the last path component
	lastSlash := strings.LastIndex(cgroupPath, "/")
	name := cgroupPath
	if lastSlash != -1 {
		name = cgroupPath[lastSlash+1:]
	}

	// Remove .scope suffix
	if !strings.HasSuffix(name, ".scope") {
		return ""
	}
	name = strings.TrimSuffix(name, ".scope")

	// Extract container ID based on runtime prefix
	var fullID string
	if strings.HasPrefix(name, "cri-containerd-") {
		fullID = strings.TrimPrefix(name, "cri-containerd-")
	} else if strings.HasPrefix(name, "crio-") {
		fullID = strings.TrimPrefix(name, "crio-")
	} else {
		return ""
	}

	// Return first 12 chars for matching
	if len(fullID) > 12 {
		return fullID[:12]
	}
	return fullID
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
