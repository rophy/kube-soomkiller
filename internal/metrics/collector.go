package metrics

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// PSI represents Pressure Stall Information for a cgroup
type PSI struct {
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	SomeTotal  uint64
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
	FullTotal  uint64
}

// SwapIOStats represents node-level swap I/O counters from /proc/vmstat
type SwapIOStats struct {
	PswpIn  uint64 // pages swapped in (cumulative)
	PswpOut uint64 // pages swapped out (cumulative)
}

// ContainerMetrics contains memory-related metrics for a container
type ContainerMetrics struct {
	CgroupPath    string
	SwapCurrent   int64 // bytes
	MemoryCurrent int64 // bytes
	MemoryMax     int64 // bytes (memory.max limit)
	PSI           PSI
}

// Collector gathers metrics from cgroups and /proc/vmstat
type Collector struct {
	cgroupRoot  string
	vmstatPath  string
}

// NewCollector creates a new metrics collector
func NewCollector(cgroupRoot string) *Collector {
	return &Collector{
		cgroupRoot: cgroupRoot,
		vmstatPath: "/proc/vmstat",
	}
}

// ValidateEnvironment checks that the system meets requirements:
// - cgroup v2 (unified hierarchy)
// - systemd cgroup driver (kubepods.slice layout)
func (c *Collector) ValidateEnvironment() error {
	// Check for cgroup v2: look for cgroup.controllers file
	cgroupControllers := filepath.Join(c.cgroupRoot, "cgroup.controllers")
	if _, err := os.Stat(cgroupControllers); os.IsNotExist(err) {
		return fmt.Errorf("cgroup v2 not detected: %s not found (cgroup v1 is not supported)", cgroupControllers)
	}

	// Check for systemd cgroup driver: look for kubepods.slice directory
	kubepodsSlice := filepath.Join(c.cgroupRoot, "kubepods.slice")
	if _, err := os.Stat(kubepodsSlice); os.IsNotExist(err) {
		return fmt.Errorf("systemd cgroup driver not detected: %s not found (cgroupfs driver is not supported)", kubepodsSlice)
	}

	// Check for swap support: look for memory.swap.max in kubepods.slice
	swapMax := filepath.Join(kubepodsSlice, "memory.swap.max")
	if _, err := os.Stat(swapMax); os.IsNotExist(err) {
		return fmt.Errorf("swap not enabled: %s not found", swapMax)
	}

	return nil
}

// CgroupsResult contains the results of cgroup discovery
type CgroupsResult struct {
	// Recognized cgroup paths matching known container runtimes
	Cgroups []string
	// Unrecognized .scope directories that don't match known patterns
	Unrecognized []string
}

// FindPodCgroups finds all container cgroup paths under kubepods.slice
// Supports both containerd (cri-containerd-) and CRI-O (crio-) runtimes
// Layout: kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice/<runtime>-<id>.scope
func (c *Collector) FindPodCgroups() (*CgroupsResult, error) {
	result := &CgroupsResult{}

	kubepodsPath := filepath.Join(c.cgroupRoot, "kubepods.slice")
	if _, err := os.Stat(kubepodsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubepods.slice not found at %s", kubepodsPath)
	}

	// Walk through kubepods hierarchy to find container cgroups
	err := filepath.Walk(kubepodsPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}

		if !info.IsDir() {
			return nil
		}

		name := info.Name()
		if !strings.HasSuffix(name, ".scope") {
			return nil
		}

		relPath, _ := filepath.Rel(c.cgroupRoot, path)

		// Match container cgroup directories:
		// - containerd: cri-containerd-<id>.scope
		// - CRI-O: crio-<id>.scope
		if strings.HasPrefix(name, "cri-containerd-") || strings.HasPrefix(name, "crio-") {
			result.Cgroups = append(result.Cgroups, relPath)
		} else {
			result.Unrecognized = append(result.Unrecognized, relPath)
		}

		return nil
	})

	return result, err
}

// GetSwapIOStats retrieves swap I/O counters from /proc/vmstat
func (c *Collector) GetSwapIOStats() (*SwapIOStats, error) {
	file, err := os.Open(c.vmstatPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", c.vmstatPath, err)
	}
	defer file.Close()

	stats := &SwapIOStats{}
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		switch fields[0] {
		case "pswpin":
			val, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				klog.Warningf("Failed to parse pswpin value %q: %v", fields[1], err)
			} else {
				stats.PswpIn = val
			}
		case "pswpout":
			val, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				klog.Warningf("Failed to parse pswpout value %q: %v", fields[1], err)
			} else {
				stats.PswpOut = val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", c.vmstatPath, err)
	}

	return stats, nil
}

// GetContainerMetrics retrieves metrics for a container given its cgroup path
func (c *Collector) GetContainerMetrics(cgroupPath string) (*ContainerMetrics, error) {
	fullPath := filepath.Join(c.cgroupRoot, cgroupPath)

	metrics := &ContainerMetrics{
		CgroupPath: cgroupPath,
	}

	// Read memory.swap.current
	swapCurrent, err := readInt64File(filepath.Join(fullPath, "memory.swap.current"))
	if err != nil {
		return nil, fmt.Errorf("failed to read memory.swap.current: %w", err)
	}
	metrics.SwapCurrent = swapCurrent

	// Read memory.current
	memoryCurrent, err := readInt64File(filepath.Join(fullPath, "memory.current"))
	if err != nil {
		return nil, fmt.Errorf("failed to read memory.current: %w", err)
	}
	metrics.MemoryCurrent = memoryCurrent

	// Read memory.max
	memoryMax, err := readMemoryMax(filepath.Join(fullPath, "memory.max"))
	if err != nil {
		return nil, fmt.Errorf("failed to read memory.max: %w", err)
	}
	metrics.MemoryMax = memoryMax

	// Read memory.pressure (PSI)
	psi, err := c.readPSI(filepath.Join(fullPath, "memory.pressure"))
	if err != nil {
		return nil, fmt.Errorf("failed to read memory.pressure: %w", err)
	}
	metrics.PSI = *psi

	return metrics, nil
}

func (c *Collector) readPSI(path string) (*PSI, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	psi := &PSI{}
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		// Parse: some avg10=17.42 avg60=3.24 avg300=0.68 total=2649745
		// Parse: full avg10=13.37 avg60=2.41 avg300=0.50 total=2098080

		parts := strings.Fields(line)
		if len(parts) < 5 {
			continue
		}

		var avg10, avg60, avg300 float64
		var total uint64

		for _, part := range parts[1:] {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "avg10":
				avg10, _ = strconv.ParseFloat(kv[1], 64)
			case "avg60":
				avg60, _ = strconv.ParseFloat(kv[1], 64)
			case "avg300":
				avg300, _ = strconv.ParseFloat(kv[1], 64)
			case "total":
				total, _ = strconv.ParseUint(kv[1], 10, 64)
			}
		}

		switch parts[0] {
		case "some":
			psi.SomeAvg10 = avg10
			psi.SomeAvg60 = avg60
			psi.SomeAvg300 = avg300
			psi.SomeTotal = total
		case "full":
			psi.FullAvg10 = avg10
			psi.FullAvg60 = avg60
			psi.FullAvg300 = avg300
			psi.FullTotal = total
		}
	}

	return psi, scanner.Err()
}

func readInt64File(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// readMemoryMax reads memory.max which can be a number or "max" (unlimited)
func readMemoryMax(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	content := strings.TrimSpace(string(data))
	if content == "max" {
		// Return a very large value for unlimited
		return 1 << 62, nil // ~4 exabytes
	}
	return strconv.ParseInt(content, 10, 64)
}
