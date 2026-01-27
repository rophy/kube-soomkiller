package controller

import (
	"testing"
)

func TestSelectVictim(t *testing.T) {
	tests := []struct {
		name       string
		candidates []PodCandidate
		wantPod    string
		wantNS     string
	}{
		{
			name: "single candidate",
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
			name: "equal PSI selects first in original order",
			candidates: []PodCandidate{
				{Namespace: "ns-a", Name: "pod-a", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
				{Namespace: "ns-b", Name: "pod-b", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
				{Namespace: "ns-c", Name: "pod-c", PSIFullAvg10: 5.0, SwapBytes: 100 << 20},
			},
			wantNS:  "ns-a",
			wantPod: "pod-a",
		},
		{
			name: "zero PSI values",
			candidates: []PodCandidate{
				{Namespace: "default", Name: "zero-psi-1", PSIFullAvg10: 0.0, SwapBytes: 100 << 20},
				{Namespace: "default", Name: "zero-psi-2", PSIFullAvg10: 0.0, SwapBytes: 200 << 20},
			},
			wantNS:  "default",
			wantPod: "zero-psi-1",
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
			name: "large candidate list",
			candidates: func() []PodCandidate {
				candidates := make([]PodCandidate, 100)
				for i := 0; i < 100; i++ {
					candidates[i] = PodCandidate{
						Namespace:    "default",
						Name:         "pod-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
						PSIFullAvg10: float64(i),
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
	var firstResult PodCandidate
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

func TestExtractContainerID(t *testing.T) {
	tests := []struct {
		name       string
		cgroupPath string
		want       string
	}{
		{
			name:       "standard containerd path",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc123def456789012345678901234567890123456789012345678901234.scope",
			want:       "abc123def456",
		},
		{
			name:       "short container ID",
			cgroupPath: "kubepods.slice/cri-containerd-abc123.scope",
			want:       "abc123",
		},
		{
			name:       "exactly 12 char ID",
			cgroupPath: "kubepods.slice/cri-containerd-123456789012.scope",
			want:       "123456789012",
		},
		{
			name:       "non-containerd cgroup",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/init.scope",
			want:       "",
		},
		{
			name:       "sandbox path (should still extract)",
			cgroupPath: "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-sandbox123456789.scope",
			want:       "sandbox12345",
		},
		{
			name:       "empty path",
			cgroupPath: "",
			want:       "",
		},
		{
			name:       "no scope suffix",
			cgroupPath: "kubepods.slice/cri-containerd-abc123",
			want:       "",
		},
		{
			name:       "crio path (not containerd)",
			cgroupPath: "kubepods.slice/crio-abc123def456.scope",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContainerID(tt.cgroupPath)
			if got != tt.want {
				t.Errorf("extractContainerID(%q) = %q, want %q", tt.cgroupPath, got, tt.want)
			}
		})
	}
}
