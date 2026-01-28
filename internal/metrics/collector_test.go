package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetPodMetrics(t *testing.T) {
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
		"memory.pressure": `some avg10=5.50 avg60=2.30 avg300=1.10 total=123456
full avg10=3.25 avg60=1.50 avg300=0.80 total=654321`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	collector := NewCollector(tmpDir)
	metrics, err := collector.GetPodMetrics(cgroupPath)
	if err != nil {
		t.Fatalf("GetPodMetrics() error = %v", err)
	}

	// Verify swap
	if metrics.SwapCurrent != 104857600 {
		t.Errorf("SwapCurrent = %d, want 104857600", metrics.SwapCurrent)
	}

	// Verify memory
	if metrics.MemoryCurrent != 268435456 {
		t.Errorf("MemoryCurrent = %d, want 268435456", metrics.MemoryCurrent)
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

func TestGetPodMetrics_ZeroSwap(t *testing.T) {
	tmpDir := t.TempDir()

	cgroupPath := "kubepods.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	files := map[string]string{
		"memory.swap.current": "0",
		"memory.current":      "134217728",
		"memory.pressure": `some avg10=0.00 avg60=0.00 avg300=0.00 total=0
full avg10=0.00 avg60=0.00 avg300=0.00 total=0`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fullPath, name), []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
	}

	collector := NewCollector(tmpDir)
	metrics, err := collector.GetPodMetrics(cgroupPath)
	if err != nil {
		t.Fatalf("GetPodMetrics() error = %v", err)
	}

	if metrics.SwapCurrent != 0 {
		t.Errorf("SwapCurrent = %d, want 0", metrics.SwapCurrent)
	}
}

func TestGetPodMetrics_MissingFiles(t *testing.T) {
	tmpDir := t.TempDir()

	cgroupPath := "kubepods.slice/cri-containerd-abc123.scope"
	fullPath := filepath.Join(tmpDir, cgroupPath)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Don't create any metric files

	collector := NewCollector(tmpDir)
	_, err := collector.GetPodMetrics(cgroupPath)
	if err == nil {
		t.Error("GetPodMetrics() expected error when metric files missing")
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

	collector := &Collector{
		cgroupRoot: tmpDir,
		vmstatPath: vmstatPath,
	}

	stats, err := collector.GetSwapIOStats()
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

	collector := &Collector{
		cgroupRoot: tmpDir,
		vmstatPath: vmstatPath,
	}

	stats, err := collector.GetSwapIOStats()
	if err != nil {
		t.Fatalf("GetSwapIOStats() error = %v", err)
	}

	if stats.PswpIn != 0 || stats.PswpOut != 0 {
		t.Errorf("Expected zero swap stats, got PswpIn=%d PswpOut=%d", stats.PswpIn, stats.PswpOut)
	}
}
