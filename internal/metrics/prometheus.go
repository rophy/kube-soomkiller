package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "soomkiller"
)

var (
	// Node-level metrics
	SwapIORate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "swap_io_rate_pages_per_second",
		Help:      "Current swap I/O rate in pages per second (pswpin + pswpout)",
	})

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

	// Pod candidate metrics
	CandidatePodsCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "candidate_pods_count",
		Help:      "Number of pods currently using swap (termination candidates)",
	})

	// Per-pod metrics (with labels)
	PodSwapBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pod_swap_bytes",
		Help:      "Swap usage in bytes per pod",
	}, []string{"namespace", "pod"})

	PodSwapPercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pod_swap_percent",
		Help:      "Swap usage as percentage of memory limit per pod",
	}, []string{"namespace", "pod"})

	PodMemoryMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pod_memory_max_bytes",
		Help:      "Memory limit (memory.max) in bytes per pod",
	}, []string{"namespace", "pod"})

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
		// Node-level
		SwapIORate,

		// Pod termination
		PodsKilledTotal,
		LastKillTimestamp,
		CandidatePodsCount,

		// Per-pod
		PodSwapBytes,
		PodSwapPercent,
		PodMemoryMax,

		// Config
		ConfigSwapThresholdPercent,
		ConfigDryRun,
	)
}

// ResetPodMetrics clears all per-pod metrics (call before updating)
func ResetPodMetrics() {
	PodSwapBytes.Reset()
	PodSwapPercent.Reset()
	PodMemoryMax.Reset()
}
