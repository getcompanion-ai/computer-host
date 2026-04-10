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
	RootDrive       DriveSpec
	KernelArgs      string
	Drives          []DriveSpec
	MMDS            *MMDSSpec
	Vsock           *VsockSpec
}

// DriveSpec describes an additional guest block device.
type DriveSpec struct {
	ID        string
	Path      string
	ReadOnly  bool
	CacheType DriveCacheType
	IOEngine  DriveIOEngine
}

type DriveCacheType string

const (
	DriveCacheTypeUnsafe    DriveCacheType = "Unsafe"
	DriveCacheTypeWriteback DriveCacheType = "Writeback"
)

type DriveIOEngine string

const (
	DriveIOEngineSync  DriveIOEngine = "Sync"
	DriveIOEngineAsync DriveIOEngine = "Async"
)

type MMDSVersion string

const (
	MMDSVersionV1 MMDSVersion = "V1"
	MMDSVersionV2 MMDSVersion = "V2"
)

// MMDSSpec describes the MMDS network configuration and initial payload.
type MMDSSpec struct {
	NetworkInterfaces []string
	Version           MMDSVersion
	IPv4Address       string
	IMDSCompat        bool
	Data              any
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
	if filepath.Base(strings.TrimSpace(string(s.ID))) != strings.TrimSpace(string(s.ID)) {
		return fmt.Errorf("machine id %q must not contain path separators", s.ID)
	}
	if err := s.rootDrive().Validate(); err != nil {
		return fmt.Errorf("root drive: %w", err)
	}
	for i, drive := range s.Drives {
		if err := drive.Validate(); err != nil {
			return fmt.Errorf("drive %d: %w", i, err)
		}
	}
	if s.MMDS != nil {
		if err := s.MMDS.Validate(); err != nil {
			return fmt.Errorf("mmds: %w", err)
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
	if strings.TrimSpace(d.Path) == "" {
		return fmt.Errorf("drive path is required")
	}
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("drive id is required")
	}
	switch d.CacheType {
	case "", DriveCacheTypeUnsafe, DriveCacheTypeWriteback:
	default:
		return fmt.Errorf("unsupported drive cache type %q", d.CacheType)
	}
	switch d.IOEngine {
	case "", DriveIOEngineSync, DriveIOEngineAsync:
	default:
		return fmt.Errorf("unsupported drive io engine %q", d.IOEngine)
	}
	return nil
}

// Validate reports whether the MMDS configuration is usable.
func (m MMDSSpec) Validate() error {
	if len(m.NetworkInterfaces) == 0 {
		return fmt.Errorf("mmds network interfaces are required")
	}
	switch m.Version {
	case "", MMDSVersionV1, MMDSVersionV2:
	default:
		return fmt.Errorf("unsupported mmds version %q", m.Version)
	}
	for i, iface := range m.NetworkInterfaces {
		if strings.TrimSpace(iface) == "" {
			return fmt.Errorf("mmds network_interfaces[%d] is required", i)
		}
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

func (s MachineSpec) rootDrive() DriveSpec {
	root := s.RootDrive
	if strings.TrimSpace(root.ID) == "" {
		root.ID = defaultRootDriveID
	}
	if strings.TrimSpace(root.Path) == "" {
		root.Path = s.RootFSPath
	}
	return root
}
