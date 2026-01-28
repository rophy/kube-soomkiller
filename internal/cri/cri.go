package cri

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Client provides access to container runtime via crictl
type Client struct {
	crictl string // path to crictl binary
}

// NewClient creates a new CRI client
func NewClient(crictl string) *Client {
	if crictl == "" {
		crictl = "crictl"
	}
	return &Client{crictl: crictl}
}

// ContainerInfo contains relevant info from crictl inspect
type ContainerInfo struct {
	CgroupPath    string
	SystemdCgroup bool
}

// GetContainerInfo returns container info including cgroup path
func (c *Client) GetContainerInfo(containerID string) (*ContainerInfo, error) {
	cmd := exec.Command(c.crictl, "inspect", containerID)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("crictl inspect failed: %w", err)
	}

	// Parse JSON output
	var result struct {
		Info struct {
			RuntimeSpec struct {
				Linux struct {
					CgroupsPath string `json:"cgroupsPath"`
				} `json:"linux"`
			} `json:"runtimeSpec"`
			Config struct {
				Linux struct {
					CgroupsPath string `json:"cgroup_parent"`
				} `json:"linux"`
			} `json:"config"`
		} `json:"info"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse crictl output: %w", err)
	}

	cgroupPath := result.Info.RuntimeSpec.Linux.CgroupsPath
	if cgroupPath == "" {
		return nil, fmt.Errorf("cgroup path not found in crictl output")
	}

	// Check if it's systemd cgroup format (contains colons)
	systemdCgroup := strings.Contains(cgroupPath, ":")

	return &ContainerInfo{
		CgroupPath:    cgroupPath,
		SystemdCgroup: systemdCgroup,
	}, nil
}

// ConvertSystemdCgroupPath converts systemd cgroup notation to filesystem path
// Input format: "kubepods-burstable-pod<uid>.slice:cri-containerd:<container-id>"
// Output format: "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice/cri-containerd-<container-id>.scope"
func ConvertSystemdCgroupPath(cgroupRoot, systemdPath string) (string, error) {
	// Parse: "<pod-slice>:<driver>:<container-id>"
	parts := strings.SplitN(systemdPath, ":", 3)
	if len(parts) != 3 {
		// Not systemd format, might be a direct path
		return fmt.Sprintf("%s/%s", cgroupRoot, systemdPath), nil
	}

	podSlice := parts[0]   // kubepods-burstable-pod<uid>.slice
	driver := parts[1]     // cri-containerd or crio
	containerID := parts[2] // full container ID

	// Extract QoS class from pod slice name
	// "kubepods-besteffort-pod..." → "kubepods-besteffort.slice"
	// "kubepods-burstable-pod..."  → "kubepods-burstable.slice"
	// "kubepods-pod..."            → "" (guaranteed, directly under kubepods.slice)
	var qosSlice string
	if strings.HasPrefix(podSlice, "kubepods-besteffort-") {
		qosSlice = "kubepods-besteffort.slice"
	} else if strings.HasPrefix(podSlice, "kubepods-burstable-") {
		qosSlice = "kubepods-burstable.slice"
	}
	// Guaranteed pods have no QoS slice (but they don't get swap anyway)

	// Build filesystem path
	var fullPath string
	if qosSlice != "" {
		fullPath = fmt.Sprintf("%s/kubepods.slice/%s/%s/%s-%s.scope",
			cgroupRoot, qosSlice, podSlice, driver, containerID)
	} else {
		fullPath = fmt.Sprintf("%s/kubepods.slice/%s/%s-%s.scope",
			cgroupRoot, podSlice, driver, containerID)
	}

	return fullPath, nil
}
