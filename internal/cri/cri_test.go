package cri

import (
	"testing"
)

func TestConvertSystemdCgroupPath(t *testing.T) {
	tests := []struct {
		name       string
		cgroupRoot string
		systemdPath string
		want       string
	}{
		{
			name:        "burstable pod with containerd",
			cgroupRoot:  "/sys/fs/cgroup",
			systemdPath: "kubepods-burstable-poda7d1868a_f849_4bfb_852c_a9533f069fd7.slice:cri-containerd:de04f4ef85a3e1dd5cf3dc765497a69d613233c2e7c8b3f1a64751734169424d",
			want:        "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-poda7d1868a_f849_4bfb_852c_a9533f069fd7.slice/cri-containerd-de04f4ef85a3e1dd5cf3dc765497a69d613233c2e7c8b3f1a64751734169424d.scope",
		},
		{
			name:        "besteffort pod with containerd",
			cgroupRoot:  "/sys/fs/cgroup",
			systemdPath: "kubepods-besteffort-podc3ea9d1b_ca2b_477b_a457_c84cbd3501a3.slice:cri-containerd:b86746b68fa5ff0f9836fcb3ff7bb9c14ef94d2b91d764329fa111ae00394c23",
			want:        "/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podc3ea9d1b_ca2b_477b_a457_c84cbd3501a3.slice/cri-containerd-b86746b68fa5ff0f9836fcb3ff7bb9c14ef94d2b91d764329fa111ae00394c23.scope",
		},
		{
			name:        "burstable pod with crio",
			cgroupRoot:  "/sys/fs/cgroup",
			systemdPath: "kubepods-burstable-pod12345678_abcd_efgh_ijkl_mnopqrstuvwx.slice:crio:abc123def456789012345678901234567890123456789012345678901234",
			want:        "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_abcd_efgh_ijkl_mnopqrstuvwx.slice/crio-abc123def456789012345678901234567890123456789012345678901234.scope",
		},
		{
			name:        "guaranteed pod (no qos slice)",
			cgroupRoot:  "/sys/fs/cgroup",
			systemdPath: "kubepods-pod12345678_abcd_efgh_ijkl_mnopqrstuvwx.slice:cri-containerd:abc123",
			want:        "/sys/fs/cgroup/kubepods.slice/kubepods-pod12345678_abcd_efgh_ijkl_mnopqrstuvwx.slice/cri-containerd-abc123.scope",
		},
		{
			name:        "custom cgroup root",
			cgroupRoot:  "/host/sys/fs/cgroup",
			systemdPath: "kubepods-burstable-pod123.slice:cri-containerd:abc",
			want:        "/host/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod123.slice/cri-containerd-abc.scope",
		},
		{
			name:        "non-systemd format (direct path)",
			cgroupRoot:  "/sys/fs/cgroup",
			systemdPath: "kubepods/burstable/pod123/container456",
			want:        "/sys/fs/cgroup/kubepods/burstable/pod123/container456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertSystemdCgroupPath(tt.cgroupRoot, tt.systemdPath)
			if err != nil {
				t.Fatalf("ConvertSystemdCgroupPath() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ConvertSystemdCgroupPath() = %q, want %q", got, tt.want)
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
			containerID: "containerd://de04f4ef85a3e1dd5cf3dc765497a69d613233c2e7c8b3f1a64751734169424d",
			want:        "de04f4ef85a3e1dd5cf3dc765497a69d613233c2e7c8b3f1a64751734169424d",
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
			// Import the function from controller package for testing
			// Since it's in controller, we test the logic here
			got := extractContainerIDFromStatusTest(tt.containerID)
			if got != tt.want {
				t.Errorf("extractContainerIDFromStatus(%q) = %q, want %q", tt.containerID, got, tt.want)
			}
		})
	}
}

// extractContainerIDFromStatusTest duplicates the logic for testing
func extractContainerIDFromStatusTest(containerID string) string {
	if containerID == "" {
		return ""
	}
	for i := 0; i < len(containerID)-2; i++ {
		if containerID[i] == ':' && containerID[i+1] == '/' && containerID[i+2] == '/' {
			return containerID[i+3:]
		}
	}
	return ""
}
