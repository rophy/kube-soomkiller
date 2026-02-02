package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetContainerMetrics(t *testing.T) {
	tmpDir := t.TempDir()

	// Create cgroup directory with metric files
	cgroupPath := "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Write metric files
	files := map[string]string{
		"memory.swap.current": "104857600", // 100MB
		"memory.current":      "268435456", // 256MB
		"memory.max":          "536870912", // 512MB
		"memory.pressure": `some avg10=5.50 avg60=2.30 avg300=1.10 total=123456
full avg10=3.25 avg60=1.50 avg300=0.80 total=654321`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	scanner := NewScanner(tmpDir)
	metrics, err := scanner.GetContainerMetrics(cgroupPath)
	if err != nil {
		t.Fatalf("GetContainerMetrics() error = %v", err)
	}

	// Verify swap
	if metrics.SwapCurrent != 104857600 {
		t.Errorf("SwapCurrent = %d, want 104857600", metrics.SwapCurrent)
	}

	// Verify memory
	if metrics.MemoryCurrent != 268435456 {
		t.Errorf("MemoryCurrent = %d, want 268435456", metrics.MemoryCurrent)
	}

	// Verify memory max
	if metrics.MemoryMax != 536870912 {
		t.Errorf("MemoryMax = %d, want 536870912", metrics.MemoryMax)
	}

	// Verify PSI
	if metrics.PSI.SomeAvg10 != 5.50 {
		t.Errorf("PSI.SomeAvg10 = %f, want 5.50", metrics.PSI.SomeAvg10)
	}
	if metrics.PSI.FullAvg10 != 3.25 {
		t.Errorf("PSI.FullAvg10 = %f, want 3.25", metrics.PSI.FullAvg10)
	}
	if metrics.PSI.FullTotal != 654321 {
		t.Errorf("PSI.FullTotal = %d, want 654321", metrics.PSI.FullTotal)
	}
}

func TestGetContainerMetrics_ZeroSwap(t *testing.T) {
	tmpDir := t.TempDir()

	cgroupPath := "kubepods.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	files := map[string]string{
		"memory.swap.current": "0",
		"memory.current":      "134217728",
		"memory.max":          "268435456", // 256MB
		"memory.pressure": `some avg10=0.00 avg60=0.00 avg300=0.00 total=0
full avg10=0.00 avg60=0.00 avg300=0.00 total=0`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	scanner := NewScanner(tmpDir)
	metrics, err := scanner.GetContainerMetrics(cgroupPath)
	if err != nil {
		t.Fatalf("GetContainerMetrics() error = %v", err)
	}

	if metrics.SwapCurrent != 0 {
		t.Errorf("SwapCurrent = %d, want 0", metrics.SwapCurrent)
	}
}

func TestGetContainerMetrics_UnlimitedMemory(t *testing.T) {
	tmpDir := t.TempDir()

	cgroupPath := "kubepods.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	files := map[string]string{
		"memory.swap.current": "0",
		"memory.current":      "134217728",
		"memory.max":          "max", // unlimited
		"memory.pressure": `some avg10=0.00 avg60=0.00 avg300=0.00 total=0
full avg10=0.00 avg60=0.00 avg300=0.00 total=0`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	scanner := NewScanner(tmpDir)
	metrics, err := scanner.GetContainerMetrics(cgroupPath)
	if err != nil {
		t.Fatalf("GetContainerMetrics() error = %v", err)
	}

	// memory.max = "max" should return 1<<62 (~4 exabytes)
	expected := int64(1 << 62)
	if metrics.MemoryMax != expected {
		t.Errorf("MemoryMax = %d, want %d (1<<62)", metrics.MemoryMax, expected)
	}
}

func TestGetContainerMetrics_MissingFiles(t *testing.T) {
	tmpDir := t.TempDir()

	cgroupPath := "kubepods.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Don't create any metric files

	scanner := NewScanner(tmpDir)
	_, err := scanner.GetContainerMetrics(cgroupPath)
	if err == nil {
		t.Error("GetContainerMetrics() expected error when metric files missing")
	}
}

func TestGetSwapIOStats(t *testing.T) {
	tmpDir := t.TempDir()
	vmstatPath := filepath.Join(tmpDir, "vmstat")

	content := `nr_free_pages 12345
pswpin 1000
pswpout 2000
pgfault 999999
`
	if err := os.WriteFile(vmstatPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	scanner := &Scanner{
		cgroupRoot: tmpDir,
		vmstatPath: vmstatPath,
	}

	stats, err := scanner.GetSwapIOStats()
	if err != nil {
		t.Fatalf("GetSwapIOStats() error = %v", err)
	}

	if stats.PswpIn != 1000 {
		t.Errorf("PswpIn = %d, want 1000", stats.PswpIn)
	}
	if stats.PswpOut != 2000 {
		t.Errorf("PswpOut = %d, want 2000", stats.PswpOut)
	}
}

func TestGetSwapIOStats_NoSwap(t *testing.T) {
	tmpDir := t.TempDir()
	vmstatPath := filepath.Join(tmpDir, "vmstat")

	// vmstat without pswpin/pswpout (system without swap)
	content := `nr_free_pages 12345
pgfault 999999
`
	if err := os.WriteFile(vmstatPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	scanner := &Scanner{
		cgroupRoot: tmpDir,
		vmstatPath: vmstatPath,
	}

	stats, err := scanner.GetSwapIOStats()
	if err != nil {
		t.Fatalf("GetSwapIOStats() error = %v", err)
	}

	if stats.PswpIn != 0 || stats.PswpOut != 0 {
		t.Errorf("Expected zero swap stats, got PswpIn=%d PswpOut=%d", stats.PswpIn, stats.PswpOut)
	}
}

func TestValidateEnvironment(t *testing.T) {
	t.Run("valid environment", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create cgroup v2 indicator
		if err := os.WriteFile(filepath.Join(tmpDir, "cgroup.controllers"), []byte("memory cpu"), 0644); err != nil {
			t.Fatalf("Failed to create cgroup.controllers: %v", err)
		}

		// Create kubepods.slice with memory.swap.max
		kubepodsPath := filepath.Join(tmpDir, "kubepods.slice")
		if err := os.MkdirAll(kubepodsPath, 0755); err != nil {
			t.Fatalf("Failed to create kubepods.slice: %v", err)
		}
		if err := os.WriteFile(filepath.Join(kubepodsPath, "memory.swap.max"), []byte("max"), 0644); err != nil {
			t.Fatalf("Failed to create memory.swap.max: %v", err)
		}

		scanner := NewScanner(tmpDir)
		if err := scanner.ValidateEnvironment(); err != nil {
			t.Errorf("ValidateEnvironment() unexpected error: %v", err)
		}
	})

	t.Run("missing cgroup v2", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Don't create cgroup.controllers

		scanner := NewScanner(tmpDir)
		err := scanner.ValidateEnvironment()
		if err == nil {
			t.Error("ValidateEnvironment() expected error for missing cgroup v2")
		}
	})

	t.Run("missing kubepods.slice", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create cgroup.controllers but not kubepods.slice
		if err := os.WriteFile(filepath.Join(tmpDir, "cgroup.controllers"), []byte("memory cpu"), 0644); err != nil {
			t.Fatalf("Failed to create cgroup.controllers: %v", err)
		}

		scanner := NewScanner(tmpDir)
		err := scanner.ValidateEnvironment()
		if err == nil {
			t.Error("ValidateEnvironment() expected error for missing kubepods.slice")
		}
	})

	t.Run("missing swap support", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create cgroup.controllers and kubepods.slice but not memory.swap.max
		if err := os.WriteFile(filepath.Join(tmpDir, "cgroup.controllers"), []byte("memory cpu"), 0644); err != nil {
			t.Fatalf("Failed to create cgroup.controllers: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(tmpDir, "kubepods.slice"), 0755); err != nil {
			t.Fatalf("Failed to create kubepods.slice: %v", err)
		}

		scanner := NewScanner(tmpDir)
		err := scanner.ValidateEnvironment()
		if err == nil {
			t.Error("ValidateEnvironment() expected error for missing swap support")
		}
	})
}

func TestFindPodCgroups(t *testing.T) {
	t.Run("finds containerd and crio cgroups", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create kubepods.slice with mixed runtime cgroups
		paths := []string{
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc123.scope",
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod456.slice/crio-def456.scope",
			"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod789.slice/cri-containerd-ghi789.scope",
		}

		for _, p := range paths {
			fullPath := filepath.Join(tmpDir, p)
			if err := os.MkdirAll(fullPath, 0755); err != nil {
				t.Fatalf("Failed to create test directory: %v", err)
			}
		}

		scanner := NewScanner(tmpDir)
		result, err := scanner.FindPodCgroups()
		if err != nil {
			t.Fatalf("FindPodCgroups() error = %v", err)
		}

		if len(result.Cgroups) != 3 {
			t.Errorf("FindPodCgroups() returned %d cgroups, want 3", len(result.Cgroups))
		}
		if len(result.Unrecognized) != 0 {
			t.Errorf("FindPodCgroups() returned %d unrecognized, want 0", len(result.Unrecognized))
		}
	})

	t.Run("tracks unrecognized scope directories", func(t *testing.T) {
		tmpDir := t.TempDir()

		paths := []string{
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc123.scope",
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/init.scope",          // unrecognized .scope
			"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod456.slice/docker-def456.scope", // unrecognized .scope
			"kubepods.slice/kubepods-burstable.slice/some-other-dir",                                      // not a .scope, ignored
			"kubepods.slice/system.slice",                                                                 // not a .scope dir, ignored
		}

		for _, p := range paths {
			fullPath := filepath.Join(tmpDir, p)
			if err := os.MkdirAll(fullPath, 0755); err != nil {
				t.Fatalf("Failed to create test directory: %v", err)
			}
		}

		scanner := NewScanner(tmpDir)
		result, err := scanner.FindPodCgroups()
		if err != nil {
			t.Fatalf("FindPodCgroups() error = %v", err)
		}

		if len(result.Cgroups) != 1 {
			t.Errorf("FindPodCgroups() returned %d cgroups, want 1", len(result.Cgroups))
		}
		if len(result.Unrecognized) != 2 {
			t.Errorf("FindPodCgroups() returned %d unrecognized, want 2: %v", len(result.Unrecognized), result.Unrecognized)
		}
	})

	t.Run("error when kubepods.slice missing", func(t *testing.T) {
		tmpDir := t.TempDir()
		// Don't create kubepods.slice

		scanner := NewScanner(tmpDir)
		_, err := scanner.FindPodCgroups()
		if err == nil {
			t.Error("FindPodCgroups() expected error when kubepods.slice missing")
		}
	})
}

func TestExtractPodUID(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "burstable pod",
			path:     "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc.scope",
			expected: "12345678-1234-1234-1234-123456789abc",
		},
		{
			name:     "besteffort pod",
			path:     "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podabc_def_123.slice/cri-containerd-xyz.scope",
			expected: "abc-def-123",
		},
		{
			name:     "no pod marker",
			path:     "kubepods.slice/kubepods-burstable.slice/cri-containerd-abc.scope",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractPodUID(tt.path)
			if result != tt.expected {
				t.Errorf("ExtractPodUID(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

func TestExtractQoS(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "burstable",
			path:     "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc.scope",
			expected: "burstable",
		},
		{
			name:     "besteffort",
			path:     "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod123.slice/cri-containerd-abc.scope",
			expected: "besteffort",
		},
		{
			name:     "guaranteed",
			path:     "kubepods.slice/kubepods-pod123.slice/cri-containerd-abc.scope",
			expected: "guaranteed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractQoS(tt.path)
			if result != tt.expected {
				t.Errorf("ExtractQoS(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

func TestIsBurstable(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{
			name:     "burstable",
			path:     "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc.scope",
			expected: true,
		},
		{
			name:     "besteffort",
			path:     "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod123.slice/cri-containerd-abc.scope",
			expected: false,
		},
		{
			name:     "guaranteed",
			path:     "kubepods.slice/kubepods-pod123.slice/cri-containerd-abc.scope",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsBurstable(tt.path)
			if result != tt.expected {
				t.Errorf("IsBurstable(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}
