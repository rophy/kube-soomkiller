package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rophy/kube-soomkiller/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestExtractContainerIDFromCgroup(t *testing.T) {
	tests := []struct {
		name       string
		cgroupPath string
		want       string
	}{
		{
			name:       "containerd full path",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc123def456789012345678901234567890123456789012345678901234.scope",
			want:       "abc123def456",
		},
		{
			name:       "crio full path",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/crio-abc123def456789012345678901234567890123456789012345678901234.scope",
			want:       "abc123def456",
		},
		{
			name:       "containerd short path",
			cgroupPath: "cri-containerd-abc123def456.scope",
			want:       "abc123def456",
		},
		{
			name:       "crio short path",
			cgroupPath: "crio-abc123def456.scope",
			want:       "abc123def456",
		},
		{
			name:       "short container ID",
			cgroupPath: "cri-containerd-abc123.scope",
			want:       "abc123",
		},
		{
			name:       "non-container cgroup",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/init.scope",
			want:       "",
		},
		{
			name:       "no scope suffix",
			cgroupPath: "cri-containerd-abc123",
			want:       "",
		},
		{
			name:       "empty path",
			cgroupPath: "",
			want:       "",
		},
		{
			name:       "unknown runtime prefix",
			cgroupPath: "docker-abc123def456.scope",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContainerIDFromCgroup(tt.cgroupPath)
			if got != tt.want {
				t.Errorf("extractContainerIDFromCgroup(%q) = %q, want %q", tt.cgroupPath, got, tt.want)
			}
		})
	}
}

func TestExtractContainerIDFromStatus(t *testing.T) {
	tests := []struct {
		name        string
		containerID string
		want        string
	}{
		{
			name:        "containerd format",
			containerID: "containerd://abc123def456789012345678901234567890123456789012345678901234",
			want:        "abc123def456789012345678901234567890123456789012345678901234",
		},
		{
			name:        "cri-o format",
			containerID: "cri-o://abc123def456789012345678901234567890123456789012345678901234",
			want:        "abc123def456789012345678901234567890123456789012345678901234",
		},
		{
			name:        "docker format",
			containerID: "docker://abc123def456",
			want:        "abc123def456",
		},
		{
			name:        "empty string",
			containerID: "",
			want:        "",
		},
		{
			name:        "no separator",
			containerID: "abc123",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContainerIDFromStatus(tt.containerID)
			if got != tt.want {
				t.Errorf("extractContainerIDFromStatus(%q) = %q, want %q", tt.containerID, got, tt.want)
			}
		})
	}
}

// Helper to create a fake cgroup with metrics
func createFakeCgroup(t *testing.T, cgroupRoot, cgroupPath string, swapBytes, memoryMax int64) {
	t.Helper()
	fullPath := filepath.Join(cgroupRoot, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create cgroup dir: %v", err)
	}

	files := map[string]string{
		"memory.swap.current": fmt.Sprintf("%d", swapBytes),
		"memory.current":      "268435456",
		"memory.max":          fmt.Sprintf("%d", memoryMax),
		"memory.pressure": `some avg10=1.00 avg60=1.00 avg300=1.00 total=1000
full avg10=1.00 avg60=1.00 avg300=1.00 total=1000`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write metric file: %v", err)
		}
	}
}

// Helper to create a fake client that honors spec.nodeName field selector
func createFakeClientWithNodeFilter(pods ...*corev1.Pod) *fake.Clientset {
	fakeClient := fake.NewSimpleClientset()

	// Add all pods to the fake client
	for _, pod := range pods {
		fakeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	}

	// Add reactor to filter pods by nodeName field selector
	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		listAction := action.(k8stesting.ListAction)
		fieldSelector := listAction.GetListRestrictions().Fields

		// Check for spec.nodeName selector
		nodeNameReq, found := fieldSelector.RequiresExactMatch("spec.nodeName")
		if !found {
			return false, nil, nil // Let default handler process
		}

		// Filter pods by node name
		var filtered []corev1.Pod
		for _, pod := range pods {
			if pod.Spec.NodeName == nodeNameReq {
				filtered = append(filtered, *pod)
			}
		}

		return true, &corev1.PodList{Items: filtered}, nil
	})

	return fakeClient
}

// Helper to create a pod with specific QoS class
func createPod(name, namespace, nodeName string, qosClass corev1.PodQOSClass, containerID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			QOSClass: qosClass,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "main",
					ContainerID: "containerd://" + containerID,
				},
			},
		},
	}
}

// Helper to create a pod with multiple containers
func createPodWithMultipleContainers(name, namespace, nodeName string, qosClass corev1.PodQOSClass, containerIDs []string) *corev1.Pod {
	var statuses []corev1.ContainerStatus
	for i, id := range containerIDs {
		statuses = append(statuses, corev1.ContainerStatus{
			Name:        fmt.Sprintf("container-%d", i),
			ContainerID: "containerd://" + id,
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			QOSClass:          qosClass,
			ContainerStatuses: statuses,
		},
	}
}

// Helper to create a pod with CRI-O runtime
func createPodCRIO(name, namespace, nodeName string, qosClass corev1.PodQOSClass, containerID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			QOSClass: qosClass,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "main",
					ContainerID: "cri-o://" + containerID,
				},
			},
		},
	}
}

func TestFindCandidates_QoSFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Container IDs (first 12 chars used for matching)
	burstableContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	guaranteedContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"
	bestEffortContainerID := "ccc111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups with realistic kubelet path structure
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+burstableContainerID+".scope", 100<<20, 512<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-guaranteed.slice/kubepods-guaranteed-podbbb.slice/cri-containerd-"+guaranteedContainerID+".scope", 100<<20, 512<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podccc.slice/cri-containerd-"+bestEffortContainerID+".scope", 100<<20, 512<<20)

	// Create fake K8s client with pods of different QoS classes
	fakeClient := fake.NewSimpleClientset(
		createPod("burstable-pod", "default", nodeName, corev1.PodQOSBurstable, burstableContainerID),
		createPod("guaranteed-pod", "default", nodeName, corev1.PodQOSGuaranteed, guaranteedContainerID),
		createPod("besteffort-pod", "default", nodeName, corev1.PodQOSBestEffort, bestEffortContainerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Only Burstable pod should be a candidate
	if len(candidates) != 1 {
		t.Errorf("findCandidates() returned %d candidates, want 1", len(candidates))
		for _, c := range candidates {
			t.Logf("  candidate: %s/%s", c.Namespace, c.Name)
		}
		return
	}

	if candidates[0].Name != "burstable-pod" {
		t.Errorf("findCandidates() candidate = %s, want burstable-pod", candidates[0].Name)
	}
}

func TestFindCandidates_SwapZeroFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Container IDs
	withSwapContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	noSwapContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups - one with swap, one without
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+withSwapContainerID+".scope", 100<<20, 512<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice/cri-containerd-"+noSwapContainerID+".scope", 0, 512<<20) // swap=0

	// Create fake K8s client - both pods are Burstable
	fakeClient := fake.NewSimpleClientset(
		createPod("with-swap-pod", "default", nodeName, corev1.PodQOSBurstable, withSwapContainerID),
		createPod("no-swap-pod", "default", nodeName, corev1.PodQOSBurstable, noSwapContainerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Only pod with swap > 0 should be a candidate
	if len(candidates) != 1 {
		t.Errorf("findCandidates() returned %d candidates, want 1", len(candidates))
		for _, c := range candidates {
			t.Logf("  candidate: %s/%s swap=%d", c.Namespace, c.Name, c.SwapBytes)
		}
		return
	}

	if candidates[0].Name != "with-swap-pod" {
		t.Errorf("findCandidates() candidate = %s, want with-swap-pod", candidates[0].Name)
	}
}

func TestFindCandidates_SwapPercentCalculation(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	containerID := "aaa111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroup: 50MB swap, 512MB memory limit = ~9.77% swap usage
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+containerID+".scope", 50<<20, 512<<20)

	fakeClient := fake.NewSimpleClientset(
		createPod("test-pod", "default", nodeName, corev1.PodQOSBurstable, containerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("findCandidates() returned %d candidates, want 1", len(candidates))
	}

	cand := candidates[0]
	expectedPercent := float64(50<<20) / float64(512<<20) * 100 // ~9.77%
	if cand.SwapPercent < 9.7 || cand.SwapPercent > 9.8 {
		t.Errorf("candidate SwapPercent = %.2f, want ~%.2f", cand.SwapPercent, expectedPercent)
	}

	if cand.MemoryMax != 512<<20 {
		t.Errorf("candidate MemoryMax = %d, want %d", cand.MemoryMax, 512<<20)
	}
}

func TestFindCandidates_NoCandidates(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Create empty kubepods.slice directory (no container cgroups)
	kubepodsPath := filepath.Join(tmpDir, "kubepods.slice")
	if err := os.MkdirAll(kubepodsPath, 0755); err != nil {
		t.Fatalf("Failed to create kubepods.slice: %v", err)
	}

	// Create fake K8s client with a Burstable pod but no matching cgroup
	containerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	fakeClient := fake.NewSimpleClientset(
		createPod("orphan-pod", "default", nodeName, corev1.PodQOSBurstable, containerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	if len(candidates) != 0 {
		t.Errorf("findCandidates() returned %d candidates, want 0", len(candidates))
	}
}

func TestFindCandidates_PodOnDifferentNode(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	containerID := "aaa111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroup with swap usage
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+containerID+".scope", 100<<20, 512<<20)

	// Create fake K8s client with pod on DIFFERENT node
	fakeClient := createFakeClientWithNodeFilter(
		createPod("remote-pod", "default", "other-node", corev1.PodQOSBurstable, containerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName, // Controller is on test-node
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Pod on different node should not be a candidate
	if len(candidates) != 0 {
		t.Errorf("findCandidates() returned %d candidates, want 0 (pod on different node)", len(candidates))
	}
}

func TestFindCandidates_MultipleContainersInPod(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Two containers in the same pod
	container1ID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	container2ID := "bbb111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups for both containers with different swap values
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+container1ID+".scope", 50<<20, 256<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+container2ID+".scope", 100<<20, 512<<20)

	// Create fake K8s client with pod having two containers
	fakeClient := fake.NewSimpleClientset(
		createPodWithMultipleContainers("multi-container-pod", "default", nodeName, corev1.PodQOSBurstable, []string{container1ID, container2ID}),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Should return one candidate with aggregated metrics
	if len(candidates) != 1 {
		t.Fatalf("findCandidates() returned %d candidates, want 1", len(candidates))
	}

	cand := candidates[0]
	if cand.Name != "multi-container-pod" {
		t.Errorf("candidate name = %s, want multi-container-pod", cand.Name)
	}

	// Swap should be aggregated (50MB + 100MB = 150MB)
	expectedSwap := int64(150 << 20)
	if cand.SwapBytes != expectedSwap {
		t.Errorf("candidate SwapBytes = %d, want %d (aggregated)", cand.SwapBytes, expectedSwap)
	}

	// MemoryMax should be the max (512MB)
	if cand.MemoryMax != 512<<20 {
		t.Errorf("candidate MemoryMax = %d, want %d (max)", cand.MemoryMax, 512<<20)
	}
}

func TestFindCandidates_CRIORuntime(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	containerID := "aaa111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroup with CRI-O format
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/crio-"+containerID+".scope", 100<<20, 512<<20)

	// Create pod with CRI-O container ID format
	fakeClient := fake.NewSimpleClientset(
		createPodCRIO("crio-pod", "default", nodeName, corev1.PodQOSBurstable, containerID),
	)

	c := &Controller{
		config: Config{
			NodeName:  nodeName,
			K8sClient: fakeClient,
			Metrics:   metrics.NewCollector(tmpDir),
		},
	}

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Should find the CRI-O pod as a candidate
	if len(candidates) != 1 {
		t.Fatalf("findCandidates() returned %d candidates, want 1", len(candidates))
	}

	if candidates[0].Name != "crio-pod" {
		t.Errorf("candidate name = %s, want crio-pod", candidates[0].Name)
	}
}

func TestTerminatePod_DryRun(t *testing.T) {
	pod := createPod("test-pod", "default", "test-node", corev1.PodQOSBurstable, "abc123")
	fakeClient := fake.NewSimpleClientset(pod)

	c := &Controller{
		config: Config{
			DryRun:    true, // dry-run enabled
			K8sClient: fakeClient,
		},
	}

	err := c.terminatePod(context.Background(), PodCandidate{
		Namespace: "default",
		Name:      "test-pod",
	})

	if err != nil {
		t.Fatalf("terminatePod() unexpected error: %v", err)
	}

	// Pod should still exist (not deleted in dry-run mode)
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Errorf("pod was deleted in dry-run mode, should have been preserved")
	}
}

func TestTerminatePod_ActualDelete(t *testing.T) {
	pod := createPod("test-pod", "default", "test-node", corev1.PodQOSBurstable, "abc123")
	fakeClient := fake.NewSimpleClientset(pod)

	c := &Controller{
		config: Config{
			DryRun:    false, // dry-run disabled
			K8sClient: fakeClient,
		},
	}

	// Verify pod exists before deletion
	_, err := fakeClient.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod should exist before deletion: %v", err)
	}

	err = c.terminatePod(context.Background(), PodCandidate{
		Namespace: "default",
		Name:      "test-pod",
	})

	if err != nil {
		t.Fatalf("terminatePod() unexpected error: %v", err)
	}

	// Pod should be deleted
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err == nil {
		t.Errorf("pod still exists after terminatePod(), should have been deleted")
	}
}

func TestTerminatePod_NonExistent(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	c := &Controller{
		config: Config{
			DryRun:    false,
			K8sClient: fakeClient,
		},
	}

	err := c.terminatePod(context.Background(), PodCandidate{
		Namespace: "default",
		Name:      "nonexistent-pod",
	})

	if err == nil {
		t.Errorf("terminatePod() should return error for non-existent pod")
	}
}

func TestFindCandidates_ProtectedNamespaces(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	defaultContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	kubeSystemContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"
	monitoringContainerID := "ccc111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups for all pods
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+defaultContainerID+".scope", 100<<20, 512<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice/cri-containerd-"+kubeSystemContainerID+".scope", 200<<20, 512<<20)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podccc.slice/cri-containerd-"+monitoringContainerID+".scope", 150<<20, 512<<20)

	// Create pods in different namespaces
	fakeClient := fake.NewSimpleClientset(
		createPod("user-pod", "default", nodeName, corev1.PodQOSBurstable, defaultContainerID),
		createPod("coredns", "kube-system", nodeName, corev1.PodQOSBurstable, kubeSystemContainerID),
		createPod("prometheus", "monitoring", nodeName, corev1.PodQOSBurstable, monitoringContainerID),
	)

	c := New(Config{
		NodeName:            nodeName,
		K8sClient:           fakeClient,
		Metrics:             metrics.NewCollector(tmpDir),
		ProtectedNamespaces: []string{"kube-system", "monitoring"},
	})

	candidates, err := c.findCandidates(context.Background())
	if err != nil {
		t.Fatalf("findCandidates() error = %v", err)
	}

	// Only the pod in "default" namespace should be a candidate
	if len(candidates) != 1 {
		t.Fatalf("findCandidates() returned %d candidates, want 1", len(candidates))
	}

	if candidates[0].Name != "user-pod" {
		t.Errorf("findCandidates() candidate = %s, want user-pod", candidates[0].Name)
	}

	if candidates[0].Namespace != "default" {
		t.Errorf("findCandidates() candidate namespace = %s, want default", candidates[0].Namespace)
	}
}

func TestNewController_ProtectedNamespacesMap(t *testing.T) {
	c := New(Config{
		ProtectedNamespaces: []string{"kube-system", "monitoring", "default"},
	})

	if !c.protectedNamespaces["kube-system"] {
		t.Error("kube-system should be in protected namespaces")
	}
	if !c.protectedNamespaces["monitoring"] {
		t.Error("monitoring should be in protected namespaces")
	}
	if !c.protectedNamespaces["default"] {
		t.Error("default should be in protected namespaces")
	}
	if c.protectedNamespaces["other"] {
		t.Error("other should not be in protected namespaces")
	}
}

func TestFindAndKillOverThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Create pods with different swap percentages
	lowSwapContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	highSwapContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"

	// low-swap: 5MB swap / 512MB limit = ~1%
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-"+lowSwapContainerID+".scope", 5<<20, 512<<20)
	// high-swap: 100MB swap / 512MB limit = ~19.5%
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice/cri-containerd-"+highSwapContainerID+".scope", 100<<20, 512<<20)

	fakeClient := fake.NewSimpleClientset(
		createPod("low-swap-pod", "default", nodeName, corev1.PodQOSBurstable, lowSwapContainerID),
		createPod("high-swap-pod", "default", nodeName, corev1.PodQOSBurstable, highSwapContainerID),
	)

	c := &Controller{
		config: Config{
			NodeName:             nodeName,
			SwapThresholdPercent: 10.0, // 10% threshold
			DryRun:               false,
			K8sClient:            fakeClient,
			Metrics:              metrics.NewCollector(tmpDir),
		},
		protectedNamespaces: make(map[string]bool),
	}

	err := c.findAndKillOverThreshold(context.Background())
	if err != nil {
		t.Fatalf("findAndKillOverThreshold() error = %v", err)
	}

	// high-swap-pod should be deleted (19.5% > 10%)
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "high-swap-pod", metav1.GetOptions{})
	if err == nil {
		t.Error("high-swap-pod should have been deleted (over threshold)")
	}

	// low-swap-pod should still exist (~1% < 10%)
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "low-swap-pod", metav1.GetOptions{})
	if err != nil {
		t.Error("low-swap-pod should still exist (under threshold)")
	}
}
