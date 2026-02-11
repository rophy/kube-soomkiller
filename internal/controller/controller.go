package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rophy/kube-soomkiller/internal/cgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// Config holds controller configuration
type Config struct {
	NodeName                  string
	PollInterval              time.Duration
	MemoryThresholdPercent    float64 // Kill pods with memory.current > this % of memory.max
	SwapThresholdPercent      float64 // Kill pods with swap.current > this % of memory.max
	FileCacheThresholdPercent float64 // Kill pods with file cache < this % of memory.max
	DryRun                    bool
	ProtectedNamespaces       []string // namespaces to never kill pods from
	K8sClient                 kubernetes.Interface
	CgroupScanner             *cgroup.Scanner
	EventRecorder             record.EventRecorder // optional, for emitting Kubernetes events
	PodInformer               *PodInformer         // node-scoped pod cache
}

// Controller monitors swap pressure and terminates pods when necessary
type Controller struct {
	config Config

	// Protected namespaces (precomputed as map for O(1) lookup)
	protectedNamespaces map[string]bool
}

// PodCandidate represents a pod that may be terminated
type PodCandidate struct {
	UID              string  // Pod UID from cgroup path
	Namespace        string  // Populated from informer cache
	Name             string  // Populated from informer cache
	MemoryPercent    float64 // memory.current / memory.max * 100
	SwapPercent      float64 // swap.current / memory.max * 100
	FileCachePercent float64 // file_cache / memory.max * 100
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
	klog.InfoS("Controller started", "pollInterval", c.config.PollInterval)
	klog.InfoS("Configured thresholds",
		"memoryThresholdPercent", c.config.MemoryThresholdPercent,
		"swapThresholdPercent", c.config.SwapThresholdPercent,
		"fileCacheThresholdPercent", c.config.FileCacheThresholdPercent)
	if len(c.config.ProtectedNamespaces) > 0 {
		klog.InfoS("Protected namespaces configured", "namespaces", c.config.ProtectedNamespaces)
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
				klog.ErrorS(err, "Reconcile failed")
			}
		}
	}
}

// checkCgroupsAtStartup scans cgroups once at startup to detect configuration issues early
func (c *Controller) checkCgroupsAtStartup() {
	result, err := c.config.CgroupScanner.FindPodCgroups()
	if err != nil {
		klog.InfoS("Startup cgroup check failed", "err", err)
		return
	}

	klog.InfoS("Startup cgroup check completed", "containerCgroups", len(result.Cgroups))

	if len(result.Unrecognized) > 0 {
		// Show up to 3 examples to avoid log spam
		examples := result.Unrecognized
		if len(examples) > 3 {
			examples = examples[:3]
		}
		klog.InfoS("Found unrecognized cgroup patterns", "count", len(result.Unrecognized), "examples", examples)
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	return c.findAndKillOverThreshold(ctx)
}

func (c *Controller) findAndKillOverThreshold(ctx context.Context) error {
	// Phase 1: Scan cgroups for kill candidates (NO API CALL)
	// Candidates already satisfy all three conditions (memory, swap, file cache)
	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		return err
	}

	if len(candidates) == 0 {
		klog.V(3).InfoS("No pods meet kill criteria")
		return nil
	}

	// Phase 2: Resolve pod names from informer cache (no API call)
	klog.V(3).InfoS("Found kill candidates", "count", len(candidates))

	var resolved []PodCandidate
	for _, cand := range candidates {
		pod := c.config.PodInformer.GetPodByUID(cand.UID)
		if pod == nil {
			klog.V(3).InfoS("Pod not found in cache", "uid", cand.UID)
			continue
		}

		// Skip pods already terminating
		if pod.DeletionTimestamp != nil {
			klog.V(3).InfoS("Skipped pod, already terminating", "pod", klog.KRef(pod.Namespace, pod.Name))
			continue
		}

		// Skip protected namespaces
		if c.protectedNamespaces[pod.Namespace] {
			klog.V(3).InfoS("Skipped pod, namespace protected", "pod", klog.KRef(pod.Namespace, pod.Name))
			continue
		}

		cand.Namespace = pod.Namespace
		cand.Name = pod.Name
		resolved = append(resolved, cand)
	}

	if len(resolved) == 0 {
		klog.V(3).InfoS("No killable pods after filtering")
		return nil
	}

	// Log all resolved candidates
	klog.V(2).InfoS("Found pods over threshold", "count", len(resolved))
	for _, cand := range resolved {
		klog.V(2).InfoS("Pod over threshold", "pod", klog.KRef(cand.Namespace, cand.Name),
			"memoryPercent", cand.MemoryPercent, "swapPercent", cand.SwapPercent, "fileCachePercent", cand.FileCachePercent)
	}

	// Kill pods over threshold (sorted by swap percent descending)
	sort.Slice(resolved, func(i, j int) bool {
		return resolved[i].SwapPercent > resolved[j].SwapPercent
	})

	var killed int
	for _, cand := range resolved {
		if err := c.terminatePod(ctx, cand); err != nil {
			klog.ErrorS(err, "Failed to delete pod", "pod", klog.KRef(cand.Namespace, cand.Name))
			continue
		}
		killed++
	}

	if killed > 0 {
		klog.InfoS("Deleted pods over threshold", "count", killed)
	}

	return nil
}

// scanCgroupsForSwap scans cgroups for pods that meet all kill criteria.
// A container is a kill candidate when ALL conditions are true:
//   - memory.current / memory.max > MemoryThresholdPercent
//   - swap.current / memory.max > SwapThresholdPercent
//   - file_cache / memory.max < FileCacheThresholdPercent
func (c *Controller) scanCgroupsForSwap() ([]PodCandidate, error) {
	// Find all container cgroups via filesystem walk
	cgroupsResult, err := c.config.CgroupScanner.FindPodCgroups()
	if err != nil {
		klog.ErrorS(err, "Failed to find pod cgroups")
		return nil, nil
	}

	// Track processed pods by UID to avoid duplicates (multiple containers per pod)
	processedPods := make(map[string]*PodCandidate)

	for _, cgroupPath := range cgroupsResult.Cgroups {
		// Filter by QoS: only Burstable pods get swap in LimitedSwap mode
		qos := cgroup.ExtractQoS(cgroupPath)
		if qos != "burstable" {
			klog.V(4).InfoS("Skipped cgroup, QoS not burstable", "cgroupPath", cgroupPath, "qos", qos)
			continue
		}

		// Extract pod UID from cgroup path
		uid := cgroup.ExtractPodUID(cgroupPath)
		if uid == "" {
			klog.Warning("Could not extract pod UID from cgroup", "cgroupPath", cgroupPath)
			continue
		}

		containerMetrics, err := c.config.CgroupScanner.GetContainerMetrics(cgroupPath)
		if err != nil {
			klog.Warning("Failed to get metrics for cgroup", "cgroupPath", cgroupPath, "err", err)
			continue
		}

		// Calculate percentages (all relative to memory.max)
		if containerMetrics.MemoryMax <= 0 {
			continue
		}
		memoryMax := float64(containerMetrics.MemoryMax)
		memoryPercent := float64(containerMetrics.MemoryCurrent) / memoryMax * 100
		swapPercent := float64(containerMetrics.SwapCurrent) / memoryMax * 100
		fileCachePercent := float64(containerMetrics.FileCache) / memoryMax * 100

		// All three conditions must be true for a kill candidate
		isCandidate := memoryPercent > c.config.MemoryThresholdPercent &&
			swapPercent > c.config.SwapThresholdPercent &&
			fileCachePercent < c.config.FileCacheThresholdPercent

		if !isCandidate {
			continue
		}

		if existing, ok := processedPods[uid]; ok {
			// Pod already seen - keep the worst-case metrics (highest swap)
			if swapPercent > existing.SwapPercent {
				existing.MemoryPercent = memoryPercent
				existing.SwapPercent = swapPercent
				existing.FileCachePercent = fileCachePercent
			}
		} else {
			processedPods[uid] = &PodCandidate{
				UID:              uid,
				MemoryPercent:    memoryPercent,
				SwapPercent:      swapPercent,
				FileCachePercent: fileCachePercent,
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

func (c *Controller) terminatePod(ctx context.Context, cand PodCandidate) error {
	if c.config.DryRun {
		klog.InfoS("Would delete pod (dry-run)", "pod", klog.KRef(cand.Namespace, cand.Name),
			"memoryPercent", cand.MemoryPercent, "swapPercent", cand.SwapPercent, "fileCachePercent", cand.FileCachePercent)
		return nil
	}

	// Emit Kubernetes event before deleting (if event recorder is configured)
	if c.config.EventRecorder != nil {
		// Get the pod object from informer cache to attach the event to
		pod := c.config.PodInformer.GetPodByUID(cand.UID)
		if pod != nil {
			c.config.EventRecorder.Eventf(pod, corev1.EventTypeWarning, "Soomkilled",
				"Pod %s deleted by kube-soomkiller on node %s: memory %.1f%%, swap %.1f%%, file cache %.1f%%",
				cand.Name, c.config.NodeName, cand.MemoryPercent, cand.SwapPercent, cand.FileCachePercent)
		} else {
			klog.V(3).InfoS("Could not get pod from cache for event", "pod", klog.KRef(cand.Namespace, cand.Name))
		}
	}

	err := c.config.K8sClient.CoreV1().Pods(cand.Namespace).Delete(ctx, cand.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod %s/%s: %w", cand.Namespace, cand.Name, err)
	}

	klog.InfoS("Deleted pod", "pod", klog.KRef(cand.Namespace, cand.Name),
		"memoryPercent", cand.MemoryPercent, "swapPercent", cand.SwapPercent, "fileCachePercent", cand.FileCachePercent,
		"reason", "soomkill thresholds exceeded")
	return nil
}
