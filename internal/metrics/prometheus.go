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

	SwapIOThresholdExceeded = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "swap_io_threshold_exceeded",
		Help:      "1 if swap I/O rate exceeds threshold, 0 otherwise",
	})

	SwapIOThresholdExceededDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "swap_io_threshold_exceeded_duration_seconds",
		Help:      "How long the swap I/O threshold has been exceeded",
	})

	CooldownRemaining = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cooldown_remaining_seconds",
		Help:      "Seconds remaining in cooldown period after killing a pod",
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

	PodPSIFullAvg10 = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pod_psi_full_avg10",
		Help:      "PSI full avg10 value per pod",
	}, []string{"namespace", "pod"})

	// Configuration metrics (for visibility)
	ConfigSwapIOThreshold = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_swap_io_threshold_pages_per_second",
		Help:      "Configured swap I/O threshold in pages per second",
	})

	ConfigSustainedDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_sustained_duration_seconds",
		Help:      "Configured sustained duration before taking action",
	})

	ConfigCooldownPeriod = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "config_cooldown_period_seconds",
		Help:      "Configured cooldown period after killing a pod",
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
		SwapIOThresholdExceeded,
		SwapIOThresholdExceededDuration,
		CooldownRemaining,

		// Pod termination
		PodsKilledTotal,
		LastKillTimestamp,
		CandidatePodsCount,

		// Per-pod
		PodSwapBytes,
		PodPSIFullAvg10,

		// Config
		ConfigSwapIOThreshold,
		ConfigSustainedDuration,
		ConfigCooldownPeriod,
		ConfigDryRun,
	)
}

// ResetPodMetrics clears all per-pod metrics (call before updating)
func ResetPodMetrics() {
	PodSwapBytes.Reset()
	PodPSIFullAvg10.Reset()
}
