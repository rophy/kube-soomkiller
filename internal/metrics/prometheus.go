package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rophy/kube-soomkiller/internal/cgroup"
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

// PodInfo contains basic pod identification info
type PodInfo struct {
	UID       string
	Namespace string
	Name      string
}

// PodLookup is an interface for looking up pod info by UID
type PodLookup interface {
	GetPodByUID(uid string) *PodInfo
}

// SwapMetricsCollector exposes per-pod swap metrics on-demand
type SwapMetricsCollector struct {
	scanner            *cgroup.Scanner
	podLookup          PodLookup
	swapBytesDesc      *prometheus.Desc
	swapPercentDesc    *prometheus.Desc
	memoryMaxDesc      *prometheus.Desc
	candidateCountDesc *prometheus.Desc
}

// NewSwapMetricsCollector creates a collector for per-pod swap metrics
func NewSwapMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup) *SwapMetricsCollector {
	return &SwapMetricsCollector{
		scanner:   scanner,
		podLookup: podLookup,
		swapBytesDesc: prometheus.NewDesc(
			namespace+"_pod_swap_bytes",
			"Current swap usage in bytes per pod",
			[]string{"namespace", "pod"}, nil,
		),
		swapPercentDesc: prometheus.NewDesc(
			namespace+"_pod_swap_percent",
			"Max swap usage percentage across containers in pod",
			[]string{"namespace", "pod"}, nil,
		),
		memoryMaxDesc: prometheus.NewDesc(
			namespace+"_pod_memory_max_bytes",
			"Memory limit in bytes per pod",
			[]string{"namespace", "pod"}, nil,
		),
		candidateCountDesc: prometheus.NewDesc(
			namespace+"_candidate_pods_count",
			"Number of pods currently using swap",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector
func (c *SwapMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.swapBytesDesc
	ch <- c.swapPercentDesc
	ch <- c.memoryMaxDesc
	ch <- c.candidateCountDesc
}

// Collect implements prometheus.Collector - scans cgroups on each scrape
func (c *SwapMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	result, err := c.scanner.FindPodCgroups()
	if err != nil {
		return
	}

	// Track pods by UID to aggregate container metrics
	type podMetrics struct {
		swapBytes   int64
		memoryMax   int64
		swapPercent float64
	}
	pods := make(map[string]*podMetrics)

	for _, cgroupPath := range result.Cgroups {
		// Only burstable pods use swap in LimitedSwap mode
		if !cgroup.IsBurstable(cgroupPath) {
			continue
		}

		uid := cgroup.ExtractPodUID(cgroupPath)
		if uid == "" {
			continue
		}

		containerMetrics, err := c.scanner.GetContainerMetrics(cgroupPath)
		if err != nil || containerMetrics.SwapCurrent == 0 {
			continue
		}

		// Calculate swap percentage for this container
		var swapPercent float64
		if containerMetrics.MemoryMax > 0 {
			swapPercent = float64(containerMetrics.SwapCurrent) / float64(containerMetrics.MemoryMax) * 100
		}

		if existing, ok := pods[uid]; ok {
			existing.swapBytes += containerMetrics.SwapCurrent
			existing.memoryMax += containerMetrics.MemoryMax
			if swapPercent > existing.swapPercent {
				existing.swapPercent = swapPercent
			}
		} else {
			pods[uid] = &podMetrics{
				swapBytes:   containerMetrics.SwapCurrent,
				memoryMax:   containerMetrics.MemoryMax,
				swapPercent: swapPercent,
			}
		}
	}

	// Emit metrics for pods we can identify
	candidateCount := 0
	for uid, pm := range pods {
		podInfo := c.podLookup.GetPodByUID(uid)
		if podInfo == nil {
			continue
		}

		candidateCount++
		ch <- prometheus.MustNewConstMetric(c.swapBytesDesc, prometheus.GaugeValue,
			float64(pm.swapBytes), podInfo.Namespace, podInfo.Name)
		ch <- prometheus.MustNewConstMetric(c.swapPercentDesc, prometheus.GaugeValue,
			pm.swapPercent, podInfo.Namespace, podInfo.Name)
		ch <- prometheus.MustNewConstMetric(c.memoryMaxDesc, prometheus.GaugeValue,
			float64(pm.memoryMax), podInfo.Namespace, podInfo.Name)
	}

	ch <- prometheus.MustNewConstMetric(c.candidateCountDesc, prometheus.GaugeValue, float64(candidateCount))
}

// RegisterSwapMetricsCollector registers the per-pod swap metrics collector
func RegisterSwapMetricsCollector(scanner *cgroup.Scanner, podLookup PodLookup) {
	prometheus.MustRegister(NewSwapMetricsCollector(scanner, podLookup))
}
