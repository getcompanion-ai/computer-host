package firecracker

import (
	"fmt"
	"path/filepath"
	"strings"
)

// MachineID uniquely identifies a single firecracler microVM
type MachineID string

// MachineSpec describes the minimum machine inputs required to boot a guest.
type MachineSpec struct {
	ID              MachineID
	VCPUs           int64
	MemoryMiB       int64
	KernelImagePath string
	RootFSPath      string
	KernelArgs      string
	Drives          []DriveSpec
	Vsock           *VsockSpec
}

// DriveSpec describes an additional guest block device.
type DriveSpec struct {
	ID       string
	Path     string
	ReadOnly bool
}

// VsockSpec describes a single host-guest vsock device.
type VsockSpec struct {
	ID   string
	CID  uint32
	Path string
}

// Validate reports whether the machine specification is usable for boot.
func (s MachineSpec) Validate() error {
	if strings.TrimSpace(string(s.ID)) == "" {
		return fmt.Errorf("machine id is required")
	}
	if s.VCPUs < 1 {
		return fmt.Errorf("machine vcpus must be at least 1")
	}
	if s.MemoryMiB < 1 {
		return fmt.Errorf("machine memory must be at least 1 MiB")
	}
	if strings.TrimSpace(s.KernelImagePath) == "" {
		return fmt.Errorf("machine kernel image path is required")
	}
	if strings.TrimSpace(s.RootFSPath) == "" {
		return fmt.Errorf("machine rootfs path is required")
	}
	if filepath.Base(strings.TrimSpace(string(s.ID))) != strings.TrimSpace(string(s.ID)) {
		return fmt.Errorf("machine id %q must not contain path separators", s.ID)
	}
	for i, drive := range s.Drives {
		if err := drive.Validate(); err != nil {
			return fmt.Errorf("drive %d: %w", i, err)
		}
	}
	if s.Vsock != nil {
		if err := s.Vsock.Validate(); err != nil {
			return fmt.Errorf("vsock: %w", err)
		}
	}
	return nil
}

// Validate reports whether the drive specification is usable.
func (d DriveSpec) Validate() error {
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("drive id is required")
	}
	if strings.TrimSpace(d.Path) == "" {
		return fmt.Errorf("drive path is required")
	}
	return nil
}

// Validate reports whether the vsock specification is usable.
func (v VsockSpec) Validate() error {
	if strings.TrimSpace(v.ID) == "" {
		return fmt.Errorf("vsock id is required")
	}
	if v.CID == 0 {
		return fmt.Errorf("vsock cid must be non zero")
	}
	if strings.TrimSpace(v.Path) == "" {
		return fmt.Errorf("vsock path is required")
	}
	return nil
}
