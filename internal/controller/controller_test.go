package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rophy/kube-soomkiller/internal/cgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

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

// Helper to create a pod with specific QoS class and UID
func createPodWithUID(name, namespace, nodeName string, uid types.UID, qosClass corev1.PodQOSClass) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       uid,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			QOSClass: qosClass,
		},
	}
}

func TestTerminatePod_DryRun(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		createPodWithUID("test-pod", "default", "test-node", "pod-uid-123", corev1.PodQOSBurstable),
	)

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
	fakeClient := fake.NewSimpleClientset(
		createPodWithUID("test-pod", "default", "test-node", "pod-uid-123", corev1.PodQOSBurstable),
	)

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

func TestScanCgroupsForSwap_QoSFiltering(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cgroups for different QoS classes
	// Pod UIDs formatted as cgroup uses underscores
	burstablePodUID := "aaaa1111_2222_3333_4444_555566667777"
	guaranteedPodUID := "bbbb1111_2222_3333_4444_555566667777"
	besteffortPodUID := "cccc1111_2222_3333_4444_555566667777"

	// Burstable - should be included
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+burstablePodUID+".slice/cri-containerd-abc.scope", 100<<20, 512<<20)
	// Guaranteed - should be filtered out (guaranteed pods don't use swap in LimitedSwap)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-pod"+guaranteedPodUID+".slice/cri-containerd-def.scope", 100<<20, 512<<20)
	// BestEffort - should be filtered out (besteffort pods don't use swap in LimitedSwap)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod"+besteffortPodUID+".slice/cri-containerd-ghi.scope", 100<<20, 512<<20)

	scanner := cgroup.NewScanner(tmpDir)
	c := &Controller{
		config: Config{
			CgroupScanner: scanner,
		},
	}

	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		t.Fatalf("scanCgroupsForSwap() error = %v", err)
	}

	// Only burstable pod should be a candidate
	if len(candidates) != 1 {
		t.Errorf("scanCgroupsForSwap() returned %d candidates, want 1", len(candidates))
		for _, c := range candidates {
			t.Logf("  candidate: uid=%s pct=%.2f%%", c.UID, c.SwapPercent)
		}
		return
	}

	// Check the UID is the burstable one (with dashes restored)
	expectedUID := "aaaa1111-2222-3333-4444-555566667777"
	if candidates[0].UID != expectedUID {
		t.Errorf("candidate UID = %s, want %s", candidates[0].UID, expectedUID)
	}
}

func TestScanCgroupsForSwap_SwapZeroFiltering(t *testing.T) {
	tmpDir := t.TempDir()

	withSwapUID := "aaaa1111_2222_3333_4444_555566667777"
	noSwapUID := "bbbb1111_2222_3333_4444_555566667777"

	// Pod with swap
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+withSwapUID+".slice/cri-containerd-abc.scope", 100<<20, 512<<20)
	// Pod without swap (swap=0)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+noSwapUID+".slice/cri-containerd-def.scope", 0, 512<<20)

	scanner := cgroup.NewScanner(tmpDir)
	c := &Controller{
		config: Config{
			CgroupScanner: scanner,
		},
	}

	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		t.Fatalf("scanCgroupsForSwap() error = %v", err)
	}

	// Only pod with swap > 0 should be a candidate
	if len(candidates) != 1 {
		t.Errorf("scanCgroupsForSwap() returned %d candidates, want 1", len(candidates))
		return
	}

	expectedUID := "aaaa1111-2222-3333-4444-555566667777"
	if candidates[0].UID != expectedUID {
		t.Errorf("candidate UID = %s, want %s", candidates[0].UID, expectedUID)
	}
}

func TestScanCgroupsForSwap_SwapPercentCalculation(t *testing.T) {
	tmpDir := t.TempDir()

	podUID := "aaaa1111_2222_3333_4444_555566667777"

	// Create cgroup: 50MB swap, 512MB memory limit = ~9.77% swap usage
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+podUID+".slice/cri-containerd-abc.scope", 50<<20, 512<<20)

	scanner := cgroup.NewScanner(tmpDir)
	c := &Controller{
		config: Config{
			CgroupScanner: scanner,
		},
	}

	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		t.Fatalf("scanCgroupsForSwap() error = %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("scanCgroupsForSwap() returned %d candidates, want 1", len(candidates))
	}

	cand := candidates[0]
	expectedPercent := float64(50<<20) / float64(512<<20) * 100 // ~9.77%
	if cand.SwapPercent < 9.7 || cand.SwapPercent > 9.8 {
		t.Errorf("candidate SwapPercent = %.2f, want ~%.2f", cand.SwapPercent, expectedPercent)
	}
}

func TestScanCgroupsForSwap_MultipleContainersInPod(t *testing.T) {
	tmpDir := t.TempDir()

	podUID := "aaaa1111_2222_3333_4444_555566667777"

	// Two containers in the same pod (same pod UID, different container IDs)
	// Container 1: 50MB swap / 256MB limit = ~19.5%
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+podUID+".slice/cri-containerd-abc.scope", 50<<20, 256<<20)
	// Container 2: 100MB swap / 512MB limit = ~19.5%
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+podUID+".slice/cri-containerd-def.scope", 100<<20, 512<<20)

	scanner := cgroup.NewScanner(tmpDir)
	c := &Controller{
		config: Config{
			CgroupScanner: scanner,
		},
	}

	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		t.Fatalf("scanCgroupsForSwap() error = %v", err)
	}

	// Should return one candidate (same pod)
	if len(candidates) != 1 {
		t.Fatalf("scanCgroupsForSwap() returned %d candidates, want 1", len(candidates))
	}

	cand := candidates[0]
	// SwapPercent should be the MAX of the two containers (~19.5% for both)
	if cand.SwapPercent < 19.0 || cand.SwapPercent > 20.0 {
		t.Errorf("candidate SwapPercent = %.2f, want ~19.5%% (max)", cand.SwapPercent)
	}
}

func TestScanCgroupsForSwap_CRIORuntime(t *testing.T) {
	tmpDir := t.TempDir()

	podUID := "aaaa1111_2222_3333_4444_555566667777"

	// Create cgroup with CRI-O format
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+podUID+".slice/crio-abc.scope", 100<<20, 512<<20)

	scanner := cgroup.NewScanner(tmpDir)
	c := &Controller{
		config: Config{
			CgroupScanner: scanner,
		},
	}

	candidates, err := c.scanCgroupsForSwap()
	if err != nil {
		t.Fatalf("scanCgroupsForSwap() error = %v", err)
	}

	// Should find the CRI-O cgroup
	if len(candidates) != 1 {
		t.Fatalf("scanCgroupsForSwap() returned %d candidates, want 1", len(candidates))
	}

	expectedUID := "aaaa1111-2222-3333-4444-555566667777"
	if candidates[0].UID != expectedUID {
		t.Errorf("candidate UID = %s, want %s", candidates[0].UID, expectedUID)
	}
}

// Note: Full integration tests with informer are done via e2e tests.
// The fake.NewSimpleClientset doesn't provide a proper RESTClient for informers.
// Tests here focus on unit testing individual components.
