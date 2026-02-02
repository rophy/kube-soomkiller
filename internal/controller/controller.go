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
	PodInformer          *PodInformer         // node-scoped pod cache
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
	UID         string  // Pod UID from cgroup path
	Namespace   string  // Populated from informer cache
	Name        string  // Populated from informer cache
	SwapPercent float64 // Max swap percentage across all containers
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
		klog.V(3).Infof("Swap I/O rate=%.1f pages/sec", swapIORate)
		c.lastStatusLogTime = now
	}

	// Log when swap I/O is detected
	if swapIORate > 0 {
		klog.V(1).Infof("Swap I/O detected: %.1f pages/sec", swapIORate)
	}

	// Always scan for pods over threshold (even without active swap I/O,
	// a pod may already be using swap from a previous burst)
	return c.findAndKillOverThreshold(ctx)
}

func (c *Controller) findAndKillOverThreshold(ctx context.Context) error {
	// Phase 1: Scan cgroups for swap usage (NO API CALL)
	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		return err
	}

	// Update candidate count metric
	metrics.CandidatePodsCount.Set(float64(len(candidates)))

	if len(candidates) == 0 {
		klog.V(3).Info("No pods using swap")
		// Reset per-pod metrics when no candidates
		metrics.ResetPodMetrics()
		return nil
	}

	// Filter to only pods over threshold
	var overThreshold []PodCandidate
	for _, cand := range candidates {
		if cand.SwapPercent > c.config.SwapThresholdPercent {
			overThreshold = append(overThreshold, cand)
		}
	}

	if len(overThreshold) == 0 {
		// Log details of candidates at V(3) for debugging
		for _, cand := range candidates {
			klog.V(3).Infof("Candidate: uid=%s pct=%.2f%% threshold=%.2f%%",
				cand.UID, cand.SwapPercent, c.config.SwapThresholdPercent)
		}
		klog.V(3).Infof("Found %d pods using swap, none over threshold", len(candidates))
		// Reset per-pod metrics when no pods over threshold
		metrics.ResetPodMetrics()
		return nil
	}

	// Phase 2: Resolve pod names from informer cache (no API call)
	klog.V(3).Infof("Found %d pods using swap, %d over threshold", len(candidates), len(overThreshold))

	// Resolve and filter candidates using informer cache
	var resolved []PodCandidate
	for _, cand := range overThreshold {
		pod := c.config.PodInformer.GetPodByUID(cand.UID)
		if pod == nil {
			klog.V(3).Infof("Pod UID %s not found in cache, may have been deleted", cand.UID)
			continue
		}

		// Skip pods already terminating
		if pod.DeletionTimestamp != nil {
			klog.V(3).Infof("Skipping pod %s/%s: already terminating", pod.Namespace, pod.Name)
			continue
		}

		// Skip protected namespaces
		if c.protectedNamespaces[pod.Namespace] {
			klog.V(3).Infof("Skipping pod %s/%s: namespace is protected", pod.Namespace, pod.Name)
			continue
		}

		cand.Namespace = pod.Namespace
		cand.Name = pod.Name
		resolved = append(resolved, cand)
	}

	// Update per-pod metrics now that we have names
	metrics.ResetPodMetrics()
	for _, cand := range resolved {
		metrics.PodSwapPercent.WithLabelValues(cand.Namespace, cand.Name).Set(cand.SwapPercent)
	}

	if len(resolved) == 0 {
		klog.V(3).Info("No killable pods after filtering")
		return nil
	}

	// Log all resolved candidates
	klog.Infof("Found %d pods over threshold:", len(resolved))
	for _, cand := range resolved {
		klog.Infof("  %s/%s: swap=%.1f%%", cand.Namespace, cand.Name, cand.SwapPercent)
	}

	// Kill pods over threshold (sorted by swap percent descending)
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].SwapPercent > resolved[j].SwapPercent
	})

	var killed int
	for _, cand := range resolved {
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

// scanCgroupsForSwap scans cgroups for pods using swap without calling the API.
// It filters by QoS class (burstable only) and returns candidates with swap usage.
func (c *Controller) scanCgroupsForSwap() ([]PodCandidate, error) {
	// Find all container cgroups via filesystem walk
	cgroupsResult, err := c.config.Metrics.FindPodCgroups()
	if err != nil {
		klog.Warningf("Failed to find pod cgroups: %v", err)
		return nil, nil
	}

	// Track processed pods by UID to avoid duplicates (multiple containers per pod)
	processedPods := make(map[string]*PodCandidate)

	for _, cgroupPath := range cgroupsResult.Cgroups {
		// Filter by QoS: only Burstable pods get swap in LimitedSwap mode
		qos := extractQoSFromCgroup(cgroupPath)
		if qos != "burstable" {
			klog.V(4).Infof("Skipping cgroup %s: QoS is %s (not burstable)", cgroupPath, qos)
			continue
		}

		// Extract pod UID from cgroup path
		uid := extractPodUIDFromCgroup(cgroupPath)
		if uid == "" {
			klog.Warningf("Could not extract pod UID from cgroup %s", cgroupPath)
			continue
		}

		containerMetrics, err := c.config.Metrics.GetContainerMetrics(cgroupPath)
		if err != nil {
			klog.Warningf("Failed to get metrics for %s: %v", cgroupPath, err)
			continue
		}

		// Skip if not using swap
		if containerMetrics.SwapCurrent == 0 {
			continue
		}

		// Calculate swap percentage for THIS container
		var swapPercent float64
		if containerMetrics.MemoryMax > 0 {
			swapPercent = float64(containerMetrics.SwapCurrent) / float64(containerMetrics.MemoryMax) * 100
		}

		if existing, ok := processedPods[uid]; ok {
			// Pod already seen - take max swap percentage
			// If ANY container exceeds threshold, the pod should be killed
			if swapPercent > existing.SwapPercent {
				existing.SwapPercent = swapPercent
			}
		} else {
			processedPods[uid] = &PodCandidate{
				UID:         uid,
				SwapPercent: swapPercent,
			}
		}
	}

	// Convert map to slice
	var candidates []PodCandidate
	for _, cand := range processedPods {
		candidates = append(candidates, *cand)
	}

	return candidates, nil
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

// extractPodUIDFromCgroup extracts the pod UID from cgroup path
// Input: kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice/...
// or: kubepods.slice/kubepods-pod<UID>.slice/... (for Guaranteed)
// Returns UID with dashes (e.g., "b47ed05b-d1f1-4318-a7ea-f4c6015264b6")
func extractPodUIDFromCgroup(cgroupPath string) string {
	// Look for "pod" prefix in path components
	parts := strings.Split(cgroupPath, "/")
	for _, part := range parts {
		// Match patterns like "kubepods-burstable-pod<UID>.slice" or "kubepods-pod<UID>.slice"
		if !strings.HasSuffix(part, ".slice") {
			continue
		}
		part = strings.TrimSuffix(part, ".slice")

		// Find "pod" marker
		podIdx := strings.LastIndex(part, "-pod")
		if podIdx == -1 {
			continue
		}

		// Extract UID after "-pod"
		uid := part[podIdx+4:] // skip "-pod"
		if uid == "" {
			continue
		}

		// Convert underscores to dashes (cgroup uses underscores)
		uid = strings.ReplaceAll(uid, "_", "-")
		return uid
	}
	return ""
}

// extractQoSFromCgroup extracts the QoS class from cgroup path
// Returns "burstable", "besteffort", or "guaranteed"
func extractQoSFromCgroup(cgroupPath string) string {
	if strings.Contains(cgroupPath, "kubepods-burstable") {
		return "burstable"
	}
	if strings.Contains(cgroupPath, "kubepods-besteffort") {
		return "besteffort"
	}
	// Guaranteed pods are directly under kubepods.slice without QoS subdirectory
	if strings.Contains(cgroupPath, "kubepods.slice") {
		return "guaranteed"
	}
	return ""
}

func (c *Controller) terminatePod(ctx context.Context, cand PodCandidate) error {
	if c.config.DryRun {
		klog.Infof("[DRY-RUN] Would delete pod %s/%s", cand.Namespace, cand.Name)
		return nil
	}

	klog.Warningf("Deleting pod %s/%s to relieve swap pressure", cand.Namespace, cand.Name)

	// Emit Kubernetes event before deleting (if event recorder is configured)
	if c.config.EventRecorder != nil {
		// Get the pod object from informer cache to attach the event to
		pod := c.config.PodInformer.GetPodByUID(cand.UID)
		if pod != nil {
			c.config.EventRecorder.Eventf(pod, corev1.EventTypeWarning, "Soomkilled",
				"Pod %s deleted by kube-soomkiller on node %s: swap usage %.1f%%",
				cand.Name, c.config.NodeName, cand.SwapPercent)
		} else {
			klog.Warningf("Could not get pod %s/%s from cache for event", cand.Namespace, cand.Name)
		}
	}

	err := c.config.K8sClient.CoreV1().Pods(cand.Namespace).Delete(ctx, cand.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod %s/%s: %w", cand.Namespace, cand.Name, err)
	}

	klog.Infof("Successfully deleted pod %s/%s", cand.Namespace, cand.Name)
	return nil
}
