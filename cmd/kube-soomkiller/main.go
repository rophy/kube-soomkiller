package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rophy/kube-soomkiller/internal/controller"
	"github.com/rophy/kube-soomkiller/internal/metrics"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func main() {
	var (
		kubeconfig        string
		nodeName          string
		pollInterval      time.Duration
		swapIOThreshold   int
		sustainedDuration time.Duration
		cooldownPeriod    time.Duration
		cgroupRoot        string
		dryRun            bool
		metricsAddr       string
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node to monitor")
	flag.DurationVar(&pollInterval, "poll-interval", 1*time.Second, "How often to sample /proc/vmstat")
	flag.IntVar(&swapIOThreshold, "swap-io-threshold", 1000, "Swap I/O rate (pages/sec) to trigger action")
	flag.DurationVar(&sustainedDuration, "sustained-duration", 10*time.Second, "How long threshold must be exceeded before action")
	flag.DurationVar(&cooldownPeriod, "cooldown-period", 30*time.Second, "Wait time after killing a pod")
	flag.StringVar(&cgroupRoot, "cgroup-root", "/sys/fs/cgroup", "Path to cgroup v2 root")
	flag.BoolVar(&dryRun, "dry-run", getEnvBool("DRY_RUN", true), "Log actions without executing")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Address to serve Prometheus metrics on")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		klog.Fatal("--node-name or NODE_NAME environment variable is required")
	}

	klog.Infof("Starting kube-soomkiller on node %s", nodeName)
	klog.Infof("Config: poll-interval=%s, swap-io-threshold=%d pages/sec, sustained-duration=%s, cooldown-period=%s, dry-run=%v",
		pollInterval, swapIOThreshold, sustainedDuration, cooldownPeriod, dryRun)

	// Register Prometheus metrics
	metrics.RegisterMetrics()

	// Set config metrics
	metrics.ConfigSwapIOThreshold.Set(float64(swapIOThreshold))
	metrics.ConfigSustainedDuration.Set(sustainedDuration.Seconds())
	metrics.ConfigCooldownPeriod.Set(cooldownPeriod.Seconds())
	if dryRun {
		metrics.ConfigDryRun.Set(1)
	} else {
		metrics.ConfigDryRun.Set(0)
	}

	// Start metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})
		klog.Infof("Starting metrics server on %s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			klog.Errorf("Metrics server error: %v", err)
		}
	}()

	// Create Kubernetes client
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create metrics collector
	metricsCollector := metrics.NewCollector(cgroupRoot)

	// Validate environment (cgroup v2, systemd, swap enabled)
	if err := metricsCollector.ValidateEnvironment(); err != nil {
		klog.Fatalf("Environment validation failed: %v", err)
	}
	klog.Info("Environment validated: cgroup v2, systemd cgroup driver, swap enabled")

	// Create controller
	ctrl := controller.New(controller.Config{
		NodeName:          nodeName,
		PollInterval:      pollInterval,
		SwapIOThreshold:   swapIOThreshold,
		SustainedDuration: sustainedDuration,
		CooldownPeriod:    cooldownPeriod,
		DryRun:            dryRun,
		K8sClient:         k8sClient,
		Metrics:           metricsCollector,
	})

	// Handle shutdown gracefully
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %s, shutting down", sig)
		cancel()
	}()

	// Run controller
	if err := ctrl.Run(ctx); err != nil {
		klog.Fatalf("Controller error: %v", err)
	}

	klog.Info("kube-soomkiller stopped")
}

func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		return val == "true" || val == "1"
	}
	return defaultVal
}
