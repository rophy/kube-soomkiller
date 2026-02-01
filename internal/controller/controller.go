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
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// Config holds controller configuration
type Config struct {
	NodeName             string
	PollInterval         time.Duration
	SwapThresholdPercent float64 // Kill pods with swap > this % of memory.max
	DryRun               bool
	ProtectedNamespaces  []string // namespaces to never kill pods from
	K8sClient            kubernetes.Interface
	Metrics              *metrics.Collector
	EventRecorder        record.EventRecorder // optional, for emitting Kubernetes events
}

// Controller monitors swap pressure and terminates pods when necessary
type Controller struct {
	config Config

	// State tracking
	lastSwapIO     *metrics.SwapIOStats
	lastSampleTime time.Time

	// Logging state (to reduce log frequency)
	lastStatusLogTime time.Time

	// Protected namespaces (precomputed as map for O(1) lookup)
	protectedNamespaces map[string]bool
}

// PodCandidate represents a pod that may be terminated
type PodCandidate struct {
	Namespace   string
	Name        string
	CgroupPath  string
	SwapBytes   int64
	MemoryMax   int64
	SwapPercent float64 // SwapBytes / MemoryMax * 100
}

// New creates a new controller
func New(config Config) *Controller {
	// Build protected namespaces map for O(1) lookup
	protectedNS := make(map[string]bool)
	for _, ns := range config.ProtectedNamespaces {
		protectedNS[ns] = true
	}

	return &Controller{
		config:              config,
		protectedNamespaces: protectedNS,
	}
}

// Run starts the controller main loop
func (c *Controller) Run(ctx context.Context) error {
	klog.Infof("Controller started, polling every %s", c.config.PollInterval)
	klog.Infof("Trigger: pod swap usage > %.1f%% of memory limit", c.config.SwapThresholdPercent)
	if len(c.config.ProtectedNamespaces) > 0 {
		klog.Infof("Protected namespaces (will not kill pods from): %v", c.config.ProtectedNamespaces)
	}

	// Startup check: scan cgroups to detect configuration issues early
	c.checkCgroupsAtStartup()

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

// checkCgroupsAtStartup scans cgroups once at startup to detect configuration issues early
func (c *Controller) checkCgroupsAtStartup() {
	result, err := c.config.Metrics.FindPodCgroups()
	if err != nil {
		klog.Warningf("Startup cgroup check failed: %v", err)
		return
	}

	klog.Infof("Startup cgroup check: found %d container cgroups", len(result.Cgroups))

	if len(result.Unrecognized) > 0 {
		// Show up to 3 examples to avoid log spam
		examples := result.Unrecognized
		if len(examples) > 3 {
			examples = examples[:3]
		}
		klog.Warningf("Startup cgroup check: found %d unrecognized cgroup patterns (not cri-containerd-* or crio-*): %v",
			len(result.Unrecognized), examples)
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

	// Update Prometheus metrics
	metrics.SwapIORate.Set(swapIORate)

	// Log status periodically (every 30s)
	if now.Sub(c.lastStatusLogTime) >= 30*time.Second {
		klog.V(2).Infof("Swap I/O rate=%.1f pages/sec", swapIORate)
		c.lastStatusLogTime = now
	}

	// No swap activity = nothing to do
	if swapIORate == 0 {
		return nil
	}

	klog.V(1).Infof("Swap I/O detected: %.1f pages/sec, scanning pods", swapIORate)

	// Find and kill pods over threshold
	return c.findAndKillOverThreshold(ctx)
}

func (c *Controller) findAndKillOverThreshold(ctx context.Context) error {
	candidates, err := c.findCandidates(ctx)
	if err != nil {
		return err
	}

	// Update candidate count metric
	metrics.CandidatePodsCount.Set(float64(len(candidates)))

	// Update per-pod metrics
	metrics.ResetPodMetrics()
	for _, cand := range candidates {
		metrics.PodSwapBytes.WithLabelValues(cand.Namespace, cand.Name).Set(float64(cand.SwapBytes))
		metrics.PodSwapPercent.WithLabelValues(cand.Namespace, cand.Name).Set(cand.SwapPercent)
		metrics.PodMemoryMax.WithLabelValues(cand.Namespace, cand.Name).Set(float64(cand.MemoryMax))
	}

	if len(candidates) == 0 {
		klog.V(2).Info("No pods using swap")
		return nil
	}

	// Log all candidates
	klog.Infof("Found %d pods using swap:", len(candidates))
	for _, cand := range candidates {
		overThreshold := ""
		if cand.SwapPercent > c.config.SwapThresholdPercent {
			overThreshold = " [OVER THRESHOLD]"
		}
		klog.Infof("  %s/%s: swap=%.1f%% (%.1fMB / %.1fMB limit)%s",
			cand.Namespace, cand.Name, cand.SwapPercent,
			float64(cand.SwapBytes)/1024/1024,
			float64(cand.MemoryMax)/1024/1024,
			overThreshold)
	}

	// Kill pods over threshold (sorted by swap percent descending)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].SwapPercent > candidates[j].SwapPercent
	})

	var killed int
	for _, cand := range candidates {
		if cand.SwapPercent <= c.config.SwapThresholdPercent {
			break // Sorted, so remaining are also under threshold
		}

		klog.Warningf("Pod %s/%s over threshold: swap=%.1f%% > %.1f%%",
			cand.Namespace, cand.Name, cand.SwapPercent, c.config.SwapThresholdPercent)

		if err := c.terminatePod(ctx, cand); err != nil {
			klog.Errorf("Failed to kill pod %s/%s: %v",
				cand.Namespace, cand.Name, err)
			continue
		}
		killed++
	}

	if killed > 0 {
		klog.Infof("Killed %d pods over swap threshold", killed)
		metrics.PodsKilledTotal.Add(float64(killed))
		metrics.LastKillTimestamp.Set(float64(time.Now().Unix()))
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

	// Build a map of container ID (first 12 chars) to pod info for Burstable pods only
	type podInfo struct {
		Namespace string
		Name      string
	}
	containerToPod := make(map[string]podInfo)

	for _, pod := range pods.Items {
		// Skip pods in protected namespaces
		if c.protectedNamespaces[pod.Namespace] {
			klog.V(3).Infof("Skipping pod %s/%s: namespace is protected",
				pod.Namespace, pod.Name)
			continue
		}

		// Skip pods that are already being deleted
		if pod.DeletionTimestamp != nil {
			klog.V(3).Infof("Skipping pod %s/%s: already being deleted",
				pod.Namespace, pod.Name)
			continue
		}

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
	cgroupsResult, err := c.config.Metrics.FindPodCgroups()
	if err != nil {
		klog.Warningf("Failed to find pod cgroups: %v", err)
		return nil, nil
	}

	// Track processed pods to avoid duplicates (multiple containers per pod)
	processedPods := make(map[string]*PodCandidate)

	for _, cgroupPath := range cgroupsResult.Cgroups {
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

		// Calculate swap percentage
		var swapPercent float64
		if podMetrics.MemoryMax > 0 {
			swapPercent = float64(podMetrics.SwapCurrent) / float64(podMetrics.MemoryMax) * 100
		}

		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		if existing, ok := processedPods[podKey]; ok {
			// Aggregate metrics across containers in the same pod
			existing.SwapBytes += podMetrics.SwapCurrent
			// Take max memory.max across containers
			if podMetrics.MemoryMax > existing.MemoryMax {
				existing.MemoryMax = podMetrics.MemoryMax
			}
			// Recalculate percent with aggregated values
			if existing.MemoryMax > 0 {
				existing.SwapPercent = float64(existing.SwapBytes) / float64(existing.MemoryMax) * 100
			}
		} else {
			processedPods[podKey] = &PodCandidate{
				Namespace:   pod.Namespace,
				Name:        pod.Name,
				CgroupPath:  cgroupPath,
				SwapBytes:   podMetrics.SwapCurrent,
				MemoryMax:   podMetrics.MemoryMax,
				SwapPercent: swapPercent,
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

func (c *Controller) terminatePod(ctx context.Context, cand PodCandidate) error {
	if c.config.DryRun {
		klog.Infof("[DRY-RUN] Would delete pod %s/%s", cand.Namespace, cand.Name)
		return nil
	}

	klog.Warningf("Deleting pod %s/%s to relieve swap pressure", cand.Namespace, cand.Name)

	// Emit Kubernetes event before deleting (if event recorder is configured)
	if c.config.EventRecorder != nil {
		// Get the pod object to attach the event to
		pod, err := c.config.K8sClient.CoreV1().Pods(cand.Namespace).Get(ctx, cand.Name, metav1.GetOptions{})
		if err == nil {
			c.config.EventRecorder.Eventf(pod, corev1.EventTypeWarning, "Soomkilled",
				"Pod %s deleted by kube-soomkiller on node %s: swap usage %.1fMiB",
				cand.Name, c.config.NodeName, float64(cand.SwapBytes)/1024/1024)
		} else {
			klog.V(2).Infof("Could not get pod %s/%s for event: %v", cand.Namespace, cand.Name, err)
		}
	}

	err := c.config.K8sClient.CoreV1().Pods(cand.Namespace).Delete(ctx, cand.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod %s/%s: %w", cand.Namespace, cand.Name, err)
	}

	klog.Infof("Successfully deleted pod %s/%s", cand.Namespace, cand.Name)
	return nil
}
