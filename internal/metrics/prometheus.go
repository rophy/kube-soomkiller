package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rophy/kube-soomkiller/internal/cgroup"
	corev1 "k8s.io/api/core/v1"
)

const (
	namespace = "soomkiller"
)

var (
	// Pod termination metrics
	PodsKilledTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "pods_killed_total",
		Help:      "Total number of pods killed due to swap pressure",
	})

	LastKillTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "last_kill_timestamp_seconds",
		Help:      "Unix timestamp of the last pod kill",
	})

	// Configuration metrics (for visibility)
	ConfigSwapThresholdPercent = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_swap_threshold_percent",
		Help:      "Configured swap threshold as percentage of memory limit",
	})

	ConfigDryRun = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_dry_run",
		Help:      "1 if dry-run mode is enabled, 0 otherwise",
	})
)

// RegisterMetrics registers all Prometheus metrics
func RegisterMetrics() {
	prometheus.MustRegister(
		// Pod termination
		PodsKilledTotal,
		LastKillTimestamp,

		// Config
		ConfigSwapThresholdPercent,
		ConfigDryRun,
	)
}

// SwapIOCollector exposes node-level swap I/O counters from /proc/vmstat
type SwapIOCollector struct {
	scanner     *cgroup.Scanner
	pswpInDesc  *prometheus.Desc
	pswpOutDesc *prometheus.Desc
}

// NewSwapIOCollector creates a collector that exposes swap I/O counters
func NewSwapIOCollector(scanner *cgroup.Scanner) *SwapIOCollector {
	return &SwapIOCollector{
		scanner: scanner,
		pswpInDesc: prometheus.NewDesc(
			namespace+"_node_swap_in_pages_total",
			"Total pages swapped in (from /proc/vmstat pswpin)",
			nil, nil,
		),
		pswpOutDesc: prometheus.NewDesc(
			namespace+"_node_swap_out_pages_total",
			"Total pages swapped out (from /proc/vmstat pswpout)",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector
func (c *SwapIOCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pswpInDesc
	ch <- c.pswpOutDesc
}

// Collect implements prometheus.Collector
func (c *SwapIOCollector) Collect(ch chan<- prometheus.Metric) {
	stats, err := c.scanner.GetSwapIOStats()
	if err != nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(c.pswpInDesc, prometheus.CounterValue, float64(stats.PswpIn))
	ch <- prometheus.MustNewConstMetric(c.pswpOutDesc, prometheus.CounterValue, float64(stats.PswpOut))
}

// RegisterSwapIOCollector registers the swap I/O collector
func RegisterSwapIOCollector(scanner *cgroup.Scanner) {
	prometheus.MustRegister(NewSwapIOCollector(scanner))
}

// PodLookup is an interface for looking up pods by UID
type PodLookup interface {
	GetPodByUID(uid string) *corev1.Pod
}

// ContainerMetricsCollector exposes per-container metrics on-demand
type ContainerMetricsCollector struct {
	scanner   *cgroup.Scanner
	podLookup PodLookup

	swapBytesDesc      *prometheus.Desc
	memoryMaxDesc      *prometheus.Desc
	psiSomeAvg10Desc   *prometheus.Desc
	psiFullAvg10Desc   *prometheus.Desc
}

// NewContainerMetricsCollector creates a collector for per-container metrics
func NewContainerMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup) *ContainerMetricsCollector {
	labels := []string{"namespace", "pod", "container"}

	return &ContainerMetricsCollector{
		scanner:   scanner,
		podLookup: podLookup,
		swapBytesDesc: prometheus.NewDesc(
			namespace+"_container_swap_bytes",
			"Current swap usage in bytes per container",
			labels, nil,
		),
		memoryMaxDesc: prometheus.NewDesc(
			namespace+"_container_memory_max_bytes",
			"Memory limit in bytes per container",
			labels, nil,
		),
		psiSomeAvg10Desc: prometheus.NewDesc(
			namespace+"_container_memory_psi_some_avg10",
			"Percentage of time at least one task was stalled on memory (10s average)",
			labels, nil,
		),
		psiFullAvg10Desc: prometheus.NewDesc(
			namespace+"_container_memory_psi_full_avg10",
			"Percentage of time all tasks were stalled on memory (10s average)",
			labels, nil,
		),
	}
}

// Describe implements prometheus.Collector
func (c *ContainerMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.swapBytesDesc
	ch <- c.memoryMaxDesc
	ch <- c.psiSomeAvg10Desc
	ch <- c.psiFullAvg10Desc
}

// Collect implements prometheus.Collector - scans cgroups on each scrape
func (c *ContainerMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	result, err := c.scanner.FindPodCgroups()
	if err != nil {
		return
	}

	for _, cgroupPath := range result.Cgroups {
		// Only burstable pods use swap in LimitedSwap mode
		if !cgroup.IsBurstable(cgroupPath) {
			continue
		}

		// Extract pod UID and container ID from cgroup path
		podUID := cgroup.ExtractPodUID(cgroupPath)
		containerID := cgroup.ExtractContainerID(cgroupPath)
		if podUID == "" || containerID == "" {
			continue
		}

		// Look up pod to get namespace, pod name, and container name
		pod := c.podLookup.GetPodByUID(podUID)
		if pod == nil {
			continue
		}

		// Find container name by matching container ID
		containerName := findContainerName(pod, containerID)
		if containerName == "" {
			continue
		}

		// Get container metrics from cgroup
		metrics, err := c.scanner.GetContainerMetrics(cgroupPath)
		if err != nil {
			continue
		}

		// Emit metrics
		labels := []string{pod.Namespace, pod.Name, containerName}

		ch <- prometheus.MustNewConstMetric(c.swapBytesDesc, prometheus.GaugeValue,
			float64(metrics.SwapCurrent), labels...)
		ch <- prometheus.MustNewConstMetric(c.memoryMaxDesc, prometheus.GaugeValue,
			float64(metrics.MemoryMax), labels...)
		ch <- prometheus.MustNewConstMetric(c.psiSomeAvg10Desc, prometheus.GaugeValue,
			metrics.PSI.SomeAvg10, labels...)
		ch <- prometheus.MustNewConstMetric(c.psiFullAvg10Desc, prometheus.GaugeValue,
			metrics.PSI.FullAvg10, labels...)
	}
}

// findContainerName finds the container name by matching container ID in pod status
func findContainerName(pod *corev1.Pod, containerID string) string {
	// Check regular containers
	for _, cs := range pod.Status.ContainerStatuses {
		if matchContainerID(cs.ContainerID, containerID) {
			return cs.Name
		}
	}

	// Check init containers
	for _, cs := range pod.Status.InitContainerStatuses {
		if matchContainerID(cs.ContainerID, containerID) {
			return cs.Name
		}
	}

	return ""
}

// matchContainerID checks if the container status ID matches the cgroup container ID
// Container status ID format: "containerd://abc123..." or "cri-o://abc123..."
// Cgroup container ID format: "abc123..."
func matchContainerID(statusID, cgroupID string) bool {
	// Remove runtime prefix (e.g., "containerd://", "cri-o://")
	if idx := strings.Index(statusID, "://"); idx != -1 {
		statusID = statusID[idx+3:]
	}

	// Container IDs should match (may be truncated in cgroup)
	return strings.HasPrefix(statusID, cgroupID) || strings.HasPrefix(cgroupID, statusID)
}

// RegisterContainerMetricsCollector registers the per-container metrics collector
func RegisterContainerMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup) {
	prometheus.MustRegister(NewContainerMetricsCollector(scanner, podLookup))
}
