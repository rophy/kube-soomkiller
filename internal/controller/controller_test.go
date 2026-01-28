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
	"k8s.io/client-go/kubernetes/fake"
)

func TestSelectVictim(t *testing.T) {
	tests := []struct {
		name       string
		candidates []PodCandidate
		wantPod    string
		wantNS     string
		wantNil    bool
	}{
		{
			name: "single candidate with PSI > 0",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "only-pod", PSIFullAvg10: 5.0, SwapBytes: 100 * 1024 * 1024},
			},
			wantNS:  "default",
			wantPod: "only-pod",
		},
		{
			name: "selects highest PSI",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "low-psi", PSIFullAvg10: 1.0, SwapBytes: 100 << 20},
				{Namespace: "default", Name: "high-psi", PSIFullAvg10: 10.0, SwapBytes: 50 << 20},
				{Namespace: "default", Name: "mid-psi", PSIFullAvg10: 5.0, SwapBytes: 200 << 20},
			},
			wantNS:  "default",
			wantPod: "high-psi",
		},
		{
			name: "PSI takes priority over swap size",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "big-swap-low-psi", PSIFullAvg10: 2.0, SwapBytes: 500 << 20},
				{Namespace: "default", Name: "small-swap-high-psi", PSIFullAvg10: 15.0, SwapBytes: 10 << 20},
			},
			wantNS:  "default",
			wantPod: "small-swap-high-psi",
		},
		{
			name: "equal PSI uses swap as tiebreaker",
			candidates: []PodCandidate{
				{Namespace: "ns-a", Name: "pod-a", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
				{Namespace: "ns-b", Name: "pod-b", PSIFullAvg10: 5.0, SwapBytes: 200 << 20},
				{Namespace: "ns-c", Name: "pod-c", PSIFullAvg10: 5.0, SwapBytes: 150 << 20},
			},
			wantNS:  "ns-b",
			wantPod: "pod-b", // highest swap when PSI is equal
		},
		{
			name: "all candidates have PSI = 0 returns nil",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "zero-psi-1", PSIFullAvg10: 0.0, SwapBytes: 100 << 20},
				{Namespace: "default", Name: "zero-psi-2", PSIFullAvg10: 0.0, SwapBytes: 200 << 20},
			},
			wantNil: true,
		},
		{
			name: "filters out PSI = 0 candidates",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "zero-psi", PSIFullAvg10: 0.0, SwapBytes: 500 << 20},
				{Namespace: "default", Name: "has-psi", PSIFullAvg10: 1.0, SwapBytes: 50 << 20},
			},
			wantNS:  "default",
			wantPod: "has-psi",
		},
		{
			name: "very small PSI differences",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "psi-low", PSIFullAvg10: 0.001, SwapBytes: 100 << 20},
				{Namespace: "default", Name: "psi-high", PSIFullAvg10: 0.002, SwapBytes: 100 << 20},
			},
			wantNS:  "default",
			wantPod: "psi-high",
		},
		{
			name: "mixed namespaces",
			candidates: []PodCandidate{
				{Namespace: "kube-system", Name: "system-pod", PSIFullAvg10: 3.0, SwapBytes: 50 << 20},
				{Namespace: "production", Name: "app-pod", PSIFullAvg10: 8.0, SwapBytes: 150 << 20},
				{Namespace: "staging", Name: "test-pod", PSIFullAvg10: 12.0, SwapBytes: 80 << 20},
			},
			wantNS:  "staging",
			wantPod: "test-pod",
		},
		{
			name: "large candidate list with mixed PSI",
			candidates: func() []PodCandidate {
				candidates := make([]PodCandidate, 100)
				for i := 0; i < 100; i++ {
					candidates[i] = PodCandidate{
						Namespace:    "default",
						Name:         "pod-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
						PSIFullAvg10: float64(i), // pod-a0 has PSI=0, filtered out
						SwapBytes:    int64(i << 20),
					}
				}
				// Add the highest PSI pod in the middle
				candidates[50] = PodCandidate{
					Namespace:    "default",
					Name:         "highest-psi-pod",
					PSIFullAvg10: 999.0,
					SwapBytes:    1 << 20,
				}
				return candidates
			}(),
			wantNS:  "default",
			wantPod: "highest-psi-pod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{}
			got := c.selectVictim(tt.candidates)
			if tt.wantNil {
				if got != nil {
					t.Errorf("selectVictim() = %s/%s, want nil", got.Namespace, got.Name)
				}
				return
			}
			if got == nil {
				t.Errorf("selectVictim() = nil, want %s/%s", tt.wantNS, tt.wantPod)
				return
			}
			if got.Namespace != tt.wantNS || got.Name != tt.wantPod {
				t.Errorf("selectVictim() = %s/%s, want %s/%s",
					got.Namespace, got.Name, tt.wantNS, tt.wantPod)
			}
		})
	}
}

func TestSelectVictimStability(t *testing.T) {
	// Test that repeated calls with same input produce same output
	candidates := []PodCandidate{
		{Namespace: "default", Name: "pod-a", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
		{Namespace: "default", Name: "pod-b", PSIFullAvg10: 10.0, SwapBytes: 200 << 20},
		{Namespace: "default", Name: "pod-c", PSIFullAvg10: 7.0, SwapBytes: 150 << 20},
	}

	c := &Controller{}

	// Run multiple times to verify deterministic behavior
	var firstResult *PodCandidate
	for i := 0; i < 100; i++ {
		// Make a copy to avoid sort affecting original
		candidatesCopy := make([]PodCandidate, len(candidates))
		copy(candidatesCopy, candidates)

		result := c.selectVictim(candidatesCopy)
		if i == 0 {
			firstResult = result
		} else {
			if result.Name != firstResult.Name || result.Namespace != firstResult.Namespace {
				t.Errorf("selectVictim() not stable: iteration %d got %s/%s, want %s/%s",
					i, result.Namespace, result.Name, firstResult.Namespace, firstResult.Name)
			}
		}
	}
}

func TestSelectVictimSwapTiebreaker(t *testing.T) {
	// When PSI values are equal, swap should be used as tiebreaker
	candidates := []PodCandidate{
		{Namespace: "default", Name: "small-swap", PSIFullAvg10: 5.0, SwapBytes: 50 << 20},
		{Namespace: "default", Name: "large-swap", PSIFullAvg10: 5.0, SwapBytes: 200 << 20},
		{Namespace: "default", Name: "medium-swap", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
	}

	c := &Controller{}

	result := c.selectVictim(candidates)
	if result == nil {
		t.Fatal("selectVictim() returned nil")
	}
	if result.Name != "large-swap" {
		t.Errorf("selectVictim() = %s, want large-swap (highest swap when PSI equal)", result.Name)
	}
}

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
func createFakeCgroup(t *testing.T, cgroupRoot, cgroupPath string, swapBytes int64, psiFullAvg10 float64) {
	t.Helper()
	fullPath := filepath.Join(cgroupRoot, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create cgroup dir: %v", err)
	}

	files := map[string]string{
		"memory.swap.current": fmt.Sprintf("%d", swapBytes),
		"memory.current":      "268435456",
		"memory.pressure": fmt.Sprintf(`some avg10=1.00 avg60=1.00 avg300=1.00 total=1000
full avg10=%.2f avg60=1.00 avg300=1.00 total=1000`, psiFullAvg10),
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write metric file: %v", err)
		}
	}
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

func TestCollectCandidates_QoSFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Create cgroups for each pod
	// Container IDs (first 12 chars used for matching)
	burstableContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	guaranteedContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"
	bestEffortContainerID := "ccc111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups with swap usage and PSI
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/cri-containerd-"+burstableContainerID+".scope", 100<<20, 5.0)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-guaranteed.slice/cri-containerd-"+guaranteedContainerID+".scope", 100<<20, 5.0)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-besteffort.slice/cri-containerd-"+bestEffortContainerID+".scope", 100<<20, 5.0)

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

func TestCollectCandidates_SwapZeroFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// Container IDs
	withSwapContainerID := "aaa111222333444555666777888999000111222333444555666777888999000111"
	noSwapContainerID := "bbb111222333444555666777888999000111222333444555666777888999000111"

	// Create cgroups - one with swap, one without
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/cri-containerd-"+withSwapContainerID+".scope", 100<<20, 5.0)
	createFakeCgroup(t, tmpDir, "kubepods.slice/kubepods-burstable.slice/cri-containerd-"+noSwapContainerID+".scope", 0, 5.0) // swap=0

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

func TestCollectCandidates_NoCandidates(t *testing.T) {
	tmpDir := t.TempDir()
	nodeName := "test-node"

	// No cgroups created - empty filesystem

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

func TestTerminatePod_DryRun(t *testing.T) {
	// Create fake client with a pod
	pod := createPod("test-pod", "default", "test-node", corev1.PodQOSBurstable, "abc123")
	fakeClient := fake.NewSimpleClientset(pod)

	c := &Controller{
		config: Config{
			DryRun:    true, // dry-run enabled
			K8sClient: fakeClient,
		},
	}

	// Call terminatePod
	err := c.terminatePod(context.Background(), PodCandidate{
		Namespace: "default",
		Name:      "test-pod",
	})

	// Should succeed without error
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
	// Create fake client with a pod
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

	// Call terminatePod
	err = c.terminatePod(context.Background(), PodCandidate{
		Namespace: "default",
		Name:      "test-pod",
	})

	// Should succeed without error
	if err != nil {
		t.Fatalf("terminatePod() unexpected error: %v", err)
	}

	// Pod should be deleted
	_, err = fakeClient.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
	if err == nil {
		t.Errorf("pod still exists after terminatePod(), should have been deleted")
	}
}
