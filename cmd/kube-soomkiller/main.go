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
		tcQueueThreshold  int
		psiThreshold      float64
		cgroupRoot        string
		tcDevice          string
		dryRun            bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node to monitor")
	flag.DurationVar(&pollInterval, "poll-interval", 5*time.Second, "How often to check metrics")
	flag.IntVar(&tcQueueThreshold, "tc-queue-threshold", 1000, "tc queue depth threshold to trigger action")
	flag.Float64Var(&psiThreshold, "psi-threshold", 50.0, "PSI full avg10 threshold to consider pod for termination")
	flag.StringVar(&cgroupRoot, "cgroup-root", "/sys/fs/cgroup", "Path to cgroup v2 root")
	flag.StringVar(&tcDevice, "tc-device", "lo", "Network device for tc stats")
	flag.BoolVar(&dryRun, "dry-run", false, "Log actions without executing")

	klog.InitFlags(nil)
	flag.Parse()

	if nodeName == "" {
		klog.Fatal("--node-name or NODE_NAME environment variable is required")
	}

	klog.Infof("Starting kube-soomkiller on node %s", nodeName)
	klog.Infof("Config: poll-interval=%s, tc-queue-threshold=%d, psi-threshold=%.1f, dry-run=%v",
		pollInterval, tcQueueThreshold, psiThreshold, dryRun)

	// Create Kubernetes client
	k8sClient, err := createK8sClient(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create metrics collector
	metricsCollector := metrics.NewCollector(cgroupRoot, tcDevice)

	// Create controller
	ctrl := controller.New(controller.Config{
		NodeName:         nodeName,
		PollInterval:     pollInterval,
		TCQueueThreshold: tcQueueThreshold,
		PSIThreshold:     psiThreshold,
		DryRun:           dryRun,
		K8sClient:        k8sClient,
		Metrics:          metricsCollector,
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
