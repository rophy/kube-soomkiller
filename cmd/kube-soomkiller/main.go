package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rophy/kube-soomkiller/internal/cgroup"
	"github.com/rophy/kube-soomkiller/internal/controller"
	"github.com/rophy/kube-soomkiller/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

var version = "dev"

func main() {
	var (
		kubeconfig            string
		nodeName              string
		pollInterval          time.Duration
		swapThresholdPercent  float64
		cgroupRoot            string
		dryRun                bool
		metricsAddr           string
		protectedNamespaces   string
		showVersion           bool
	)

	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node to monitor")
	flag.DurationVar(&pollInterval, "poll-interval", 1*time.Second, "How often to sample /proc/vmstat (minimum 1s)")
	flag.Float64Var(&swapThresholdPercent, "swap-threshold-percent", 1.0, "Kill pods with swap usage > this % of memory limit")
	flag.StringVar(&cgroupRoot, "cgroup-root", "/sys/fs/cgroup", "Path to cgroup v2 root")
	flag.BoolVar(&dryRun, "dry-run", getEnvBool("DRY_RUN", true), "Log actions without executing")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Address to serve Prometheus metrics on")
	flag.StringVar(&protectedNamespaces, "protected-namespaces", "kube-system", "Comma-separated list of namespaces to never kill pods from")

	klog.InitFlags(nil)
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// Validate required parameters
	if nodeName == "" {
		klog.Fatal("--node-name or NODE_NAME environment variable is required")
	}

	// Validate configuration parameters
	if pollInterval < time.Second {
		klog.Fatalf("--poll-interval must be at least 1s, got %s", pollInterval)
	}
	if swapThresholdPercent < 0 {
		klog.Fatalf("--swap-threshold-percent must be >= 0, got %f", swapThresholdPercent)
	}

	klog.InfoS("Starting kube-soomkiller", "node", nodeName, "version", version)
	klog.InfoS("Configuration loaded", "pollInterval", pollInterval, "swapThresholdPercent", swapThresholdPercent, "dryRun", dryRun)

	// Create cgroup scanner
	cgroupScanner := cgroup.NewScanner(cgroupRoot)

	// Validate environment (cgroup v2, systemd, swap enabled)
	if err := cgroupScanner.ValidateEnvironment(); err != nil {
		klog.Fatalf("Environment validation failed: %v", err)
	}
	klog.InfoS("Environment validated", "cgroupVersion", "v2", "cgroupDriver", "systemd", "swapEnabled", true)

	// Register Prometheus metrics
	metrics.RegisterMetrics()
	metrics.RegisterSwapIOCollector(cgroupScanner)

	// Set config metrics
	metrics.ConfigSwapThresholdPercent.Set(swapThresholdPercent)
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
		klog.InfoS("Metrics server started", "addr", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			klog.ErrorS(err, "Metrics server failed")
		}
	}()

	// Create Kubernetes client
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Parse protected namespaces
	var protectedNSList []string
	if protectedNamespaces != "" {
		for _, ns := range strings.Split(protectedNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" {
				protectedNSList = append(protectedNSList, ns)
			}
		}
	}

	// Create event recorder for emitting Kubernetes events
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: k8sClient.CoreV1().Events(""),
	})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: "kube-soomkiller",
	})

	// Create node-scoped pod informer
	podInformer := controller.NewPodInformer(k8sClient, nodeName, 30*time.Second)

	// Create PodLookup adapter for metrics collector
	podLookup := &podLookupAdapter{informer: podInformer}

	// Register per-pod swap metrics collector
	metrics.RegisterSwapMetricsCollector(cgroupScanner, podLookup)

	// Create controller
	ctrl := controller.New(controller.Config{
		NodeName:             nodeName,
		PollInterval:         pollInterval,
		SwapThresholdPercent: swapThresholdPercent,
		DryRun:               dryRun,
		ProtectedNamespaces:  protectedNSList,
		K8sClient:            k8sClient,
		CgroupScanner:        cgroupScanner,
		EventRecorder:        eventRecorder,
		PodInformer:          podInformer,
	})

	// Handle shutdown gracefully
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		klog.InfoS("Received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Start pod informer in background
	go podInformer.Run(ctx.Done())

	// Wait for informer cache to sync before starting controller
	klog.InfoS("Waiting for pod informer cache to sync")
	if !podInformer.WaitForCacheSync(ctx.Done()) {
		klog.Fatal("Failed to sync pod informer cache")
	}
	klog.InfoS("Pod informer cache synced")

	// Run controller
	if err := ctrl.Run(ctx); err != nil {
		klog.Fatalf("Controller error: %v", err)
	}

	klog.InfoS("Controller stopped")
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

// podLookupAdapter adapts PodInformer to the metrics.PodLookup interface
type podLookupAdapter struct {
	informer *controller.PodInformer
}

func (a *podLookupAdapter) GetPodByUID(uid string) *metrics.PodInfo {
	pod := a.informer.GetPodByUID(uid)
	if pod == nil {
		return nil
	}
	return &metrics.PodInfo{
		UID:       string(pod.UID),
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}
}
