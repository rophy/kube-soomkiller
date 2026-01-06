package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		psiThreshold      float64
		cgroupRoot        string
		dryRun            bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node to monitor")
	flag.DurationVar(&pollInterval, "poll-interval", 1*time.Second, "How often to sample /proc/vmstat")
	flag.IntVar(&swapIOThreshold, "swap-io-threshold", 1000, "Swap I/O rate (pages/sec) to trigger action")
	flag.DurationVar(&sustainedDuration, "sustained-duration", 10*time.Second, "How long threshold must be exceeded before action")
	flag.DurationVar(&cooldownPeriod, "cooldown-period", 30*time.Second, "Wait time after killing a pod")
	flag.Float64Var(&psiThreshold, "psi-threshold", 50.0, "Minimum PSI full avg10 for pod selection")
	flag.StringVar(&cgroupRoot, "cgroup-root", "/sys/fs/cgroup", "Path to cgroup v2 root")
	flag.BoolVar(&dryRun, "dry-run", true, "Log actions without executing")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		klog.Fatal("--node-name or NODE_NAME environment variable is required")
	}

	klog.Infof("Starting kube-soomkiller on node %s", nodeName)
	klog.Infof("Config: poll-interval=%s, swap-io-threshold=%d pages/sec, sustained-duration=%s, cooldown-period=%s, psi-threshold=%.1f, dry-run=%v",
		pollInterval, swapIOThreshold, sustainedDuration, cooldownPeriod, psiThreshold, dryRun)

	// Create Kubernetes client
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create metrics collector
	metricsCollector := metrics.NewCollector(cgroupRoot)

	// Create controller
	ctrl := controller.New(controller.Config{
		NodeName:          nodeName,
		PollInterval:      pollInterval,
		SwapIOThreshold:   swapIOThreshold,
		SustainedDuration: sustainedDuration,
		CooldownPeriod:    cooldownPeriod,
		PSIThreshold:      psiThreshold,
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
