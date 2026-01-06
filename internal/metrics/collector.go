package metrics

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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

// TCStats represents traffic control queue statistics
type TCStats struct {
	Backlog     int64  // bytes in queue
	BacklogPkts int64  // packets in queue
	Dropped     int64  // dropped packets
	Overlimits  int64  // rate-limited packets
	Sent        int64  // bytes sent
	SentPkts    int64  // packets sent
}

// PodMetrics contains memory-related metrics for a pod
type PodMetrics struct {
	CgroupPath   string
	SwapCurrent  int64   // bytes
	MemoryCurrent int64  // bytes
	PSI          PSI
}

// Collector gathers metrics from cgroups and tc
type Collector struct {
	cgroupRoot string
	tcDevice   string
}

// NewCollector creates a new metrics collector
func NewCollector(cgroupRoot, tcDevice string) *Collector {
	return &Collector{
		cgroupRoot: cgroupRoot,
		tcDevice:   tcDevice,
	}
}

// GetTCStats retrieves tc queue statistics for the configured device
func (c *Collector) GetTCStats() (*TCStats, error) {
	cmd := exec.Command("tc", "-s", "qdisc", "show", "dev", c.tcDevice)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run tc: %w", err)
	}

	return parseTCStats(string(output))
}

func parseTCStats(output string) (*TCStats, error) {
	stats := &TCStats{}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Parse: Sent 210325976 bytes 134175 pkt (dropped 0, overlimits 5678 requeues 0)
		if strings.HasPrefix(line, "Sent") {
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				stats.Sent, _ = strconv.ParseInt(parts[1], 10, 64)
				stats.SentPkts, _ = strconv.ParseInt(parts[3], 10, 64)
			}
			// Parse dropped and overlimits from parentheses
			if idx := strings.Index(line, "dropped"); idx != -1 {
				var dropped, overlimits int64
				fmt.Sscanf(line[idx:], "dropped %d, overlimits %d", &dropped, &overlimits)
				stats.Dropped = dropped
				stats.Overlimits = overlimits
			}
		}

		// Parse: backlog 12345b 89p requeues 0
		if strings.HasPrefix(line, "backlog") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				// Parse "12345b" -> 12345
				bytesStr := strings.TrimSuffix(parts[1], "b")
				stats.Backlog, _ = strconv.ParseInt(bytesStr, 10, 64)
				// Parse "89p" -> 89
				pktsStr := strings.TrimSuffix(parts[2], "p")
				stats.BacklogPkts, _ = strconv.ParseInt(pktsStr, 10, 64)
			}
		}
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

// FindPodCgroups finds all pod cgroup paths on the node
func (c *Collector) FindPodCgroups() ([]string, error) {
	var cgroups []string

	kubepodPath := filepath.Join(c.cgroupRoot, "kubepods.slice")
	if _, err := os.Stat(kubepodPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubepods.slice not found at %s", kubepodPath)
	}

	// Walk through kubepods hierarchy to find container cgroups
	err := filepath.Walk(kubepodPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Look for cri-containerd-* directories (container cgroups)
		if info.IsDir() && strings.HasPrefix(info.Name(), "cri-containerd-") {
			relPath, _ := filepath.Rel(c.cgroupRoot, path)
			cgroups = append(cgroups, relPath)
		}

		return nil
	})

	return cgroups, err
}

func readInt64File(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
