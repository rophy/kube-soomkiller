package metrics

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// PodMetrics contains memory-related metrics for a pod
type PodMetrics struct {
	CgroupPath    string
	SwapCurrent   int64 // bytes
	MemoryCurrent int64 // bytes
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
			stats.PswpIn, _ = strconv.ParseUint(fields[1], 10, 64)
		case "pswpout":
			stats.PswpOut, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", c.vmstatPath, err)
	}

	return stats, nil
}

// GetPodMetrics retrieves metrics for a pod given its cgroup path
func (c *Collector) GetPodMetrics(cgroupPath string) (*PodMetrics, error) {
	fullPath := filepath.Join(c.cgroupRoot, cgroupPath)

	metrics := &PodMetrics{
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
