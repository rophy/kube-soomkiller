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

// Metrics holds all the prometheus metrics with node label
type Metrics struct {
	nodeName string

	// Pod termination metrics
	PodsKilledTotal   prometheus.Counter
	LastKillTimestamp prometheus.Gauge

	// Configuration metrics
	ConfigSwapThresholdPercent prometheus.Gauge
	ConfigDryRun               prometheus.Gauge
}

// NewMetrics creates metrics with the node label
func NewMetrics(nodeName string) *Metrics {
	nodeLabel := prometheus.Labels{"node": nodeName}

	return &Metrics{
		nodeName: nodeName,
		PodsKilledTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "pods_killed_total",
			Help:        "Total number of pods killed due to swap pressure",
			ConstLabels: nodeLabel,
		}),
		LastKillTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "last_kill_timestamp_seconds",
			Help:        "Unix timestamp of the last pod kill",
			ConstLabels: nodeLabel,
		}),
		ConfigSwapThresholdPercent: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "config_swap_threshold_percent",
			Help:        "Configured swap threshold as percentage of memory limit",
			ConstLabels: nodeLabel,
		}),
		ConfigDryRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "config_dry_run",
			Help:        "1 if dry-run mode is enabled, 0 otherwise",
			ConstLabels: nodeLabel,
		}),
	}
}

// Register registers all metrics with prometheus
func (m *Metrics) Register() {
	prometheus.MustRegister(
		m.PodsKilledTotal,
		m.LastKillTimestamp,
		m.ConfigSwapThresholdPercent,
		m.ConfigDryRun,
	)
}

// SwapIOCollector exposes node-level swap I/O counters from /proc/vmstat
type SwapIOCollector struct {
	scanner     *cgroup.Scanner
	nodeName    string
	pswpInDesc  *prometheus.Desc
	pswpOutDesc *prometheus.Desc
}

// NewSwapIOCollector creates a collector that exposes swap I/O counters
func NewSwapIOCollector(scanner *cgroup.Scanner, nodeName string) *SwapIOCollector {
	nodeLabel := prometheus.Labels{"node": nodeName}

	return &SwapIOCollector{
		scanner:  scanner,
		nodeName: nodeName,
		pswpInDesc: prometheus.NewDesc(
			namespace+"_node_swap_in_pages_total",
			"Total pages swapped in (from /proc/vmstat pswpin)",
			nil, nodeLabel,
		),
		pswpOutDesc: prometheus.NewDesc(
			namespace+"_node_swap_out_pages_total",
			"Total pages swapped out (from /proc/vmstat pswpout)",
			nil, nodeLabel,
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
func RegisterSwapIOCollector(scanner *cgroup.Scanner, nodeName string) {
	prometheus.MustRegister(NewSwapIOCollector(scanner, nodeName))
}

// PodLookup is an interface for looking up pods by UID
type PodLookup interface {
	GetPodByUID(uid string) *corev1.Pod
}

// ContainerMetricsCollector exposes per-container metrics on-demand
type ContainerMetricsCollector struct {
	scanner   *cgroup.Scanner
	podLookup PodLookup
	nodeName  string

	swapBytesDesc     *prometheus.Desc
	swapMaxDesc       *prometheus.Desc
	memoryCurrentDesc *prometheus.Desc
	memoryMaxDesc     *prometheus.Desc
}

// NewContainerMetricsCollector creates a collector for per-container metrics
func NewContainerMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup, nodeName string) *ContainerMetricsCollector {
	labels := []string{"namespace", "pod", "container"}
	nodeLabel := prometheus.Labels{"node": nodeName}

	return &ContainerMetricsCollector{
		scanner:   scanner,
		podLookup: podLookup,
		nodeName:  nodeName,
		swapBytesDesc: prometheus.NewDesc(
			namespace+"_container_swap_bytes",
			"Current swap usage in bytes per container",
			labels, nodeLabel,
		),
		swapMaxDesc: prometheus.NewDesc(
			namespace+"_container_swap_max_bytes",
			"Swap limit in bytes per container",
			labels, nodeLabel,
		),
		memoryCurrentDesc: prometheus.NewDesc(
			namespace+"_container_memory_current_bytes",
			"Current memory usage in bytes per container",
			labels, nodeLabel,
		),
		memoryMaxDesc: prometheus.NewDesc(
			namespace+"_container_memory_max_bytes",
			"Memory limit in bytes per container",
			labels, nodeLabel,
		),
	}
}

// Describe implements prometheus.Collector
func (c *ContainerMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.swapBytesDesc
	ch <- c.swapMaxDesc
	ch <- c.memoryCurrentDesc
	ch <- c.memoryMaxDesc
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
		ch <- prometheus.MustNewConstMetric(c.swapMaxDesc, prometheus.GaugeValue,
			float64(metrics.SwapMax), labels...)
		ch <- prometheus.MustNewConstMetric(c.memoryCurrentDesc, prometheus.GaugeValue,
			float64(metrics.MemoryCurrent), labels...)
		ch <- prometheus.MustNewConstMetric(c.memoryMaxDesc, prometheus.GaugeValue,
			float64(metrics.MemoryMax), labels...)
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
func RegisterContainerMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup, nodeName string) {
	prometheus.MustRegister(NewContainerMetricsCollector(scanner, podLookup, nodeName))
}
