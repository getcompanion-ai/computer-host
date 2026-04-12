package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type vmConfig struct {
	BootSource        vmBootSource     `json:"boot-source"`
	Drives            []vmDrive        `json:"drives"`
	MachineConfig     vmMachineConfig  `json:"machine-config"`
	NetworkInterfaces []vmNetworkIface `json:"network-interfaces"`
	Vsock             *vmVsock         `json:"vsock,omitempty"`
	Logger            *vmLogger        `json:"logger,omitempty"`
	MMDSConfig        *vmMMDSConfig    `json:"mmds-config,omitempty"`
	Entropy           *vmEntropy       `json:"entropy,omitempty"`
	Serial            *vmSerial        `json:"serial,omitempty"`
}

type vmBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type vmDrive struct {
	DriveID      string         `json:"drive_id"`
	PathOnHost   string         `json:"path_on_host"`
	IsRootDevice bool           `json:"is_root_device"`
	IsReadOnly   bool           `json:"is_read_only"`
	CacheType    DriveCacheType `json:"cache_type,omitempty"`
	IOEngine     DriveIOEngine  `json:"io_engine,omitempty"`
}

type vmMachineConfig struct {
	VcpuCount  int64 `json:"vcpu_count"`
	MemSizeMib int64 `json:"mem_size_mib"`
}

type vmNetworkIface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type vmVsock struct {
	GuestCID int64  `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type vmLogger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level,omitempty"`
	ShowLevel     bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool   `json:"show_log_origin,omitempty"`
}

type vmMMDSConfig struct {
	Version           MMDSVersion `json:"version,omitempty"`
	NetworkInterfaces []string    `json:"network_interfaces"`
	IPv4Address       string      `json:"ipv4_address,omitempty"`
}

type vmEntropy struct{}

type vmSerial struct {
	SerialOutPath string `json:"serial_out_path,omitempty"`
}

func writeConfigFile(chrootRootDir string, spec MachineSpec, paths machinePaths, network NetworkAllocation) (string, error) {
	cfg := vmConfig{
		BootSource: vmBootSource{
			KernelImagePath: spec.KernelImagePath,
			BootArgs:        spec.KernelArgs,
		},
		MachineConfig: vmMachineConfig{
			VcpuCount:  spec.VCPUs,
			MemSizeMib: spec.MemoryMiB,
		},
		Logger: &vmLogger{
			LogPath:       paths.JailedFirecrackerLogPath,
			Level:         defaultFirecrackerLogLevel,
			ShowLevel:     true,
			ShowLogOrigin: true,
		},
		Entropy: &vmEntropy{},
		Serial:  &vmSerial{SerialOutPath: paths.JailedSerialLogPath},
	}

	root := spec.rootDrive()
	cfg.Drives = append(cfg.Drives, vmDrive{
		DriveID:      root.ID,
		PathOnHost:   root.Path,
		IsRootDevice: true,
		IsReadOnly:   root.ReadOnly,
		CacheType:    root.CacheType,
		IOEngine:     root.IOEngine,
	})
	for _, drive := range spec.Drives {
		cfg.Drives = append(cfg.Drives, vmDrive{
			DriveID:      drive.ID,
			PathOnHost:   drive.Path,
			IsRootDevice: false,
			IsReadOnly:   drive.ReadOnly,
			CacheType:    drive.CacheType,
			IOEngine:     drive.IOEngine,
		})
	}

	cfg.NetworkInterfaces = append(cfg.NetworkInterfaces, vmNetworkIface{
		IfaceID:     network.InterfaceID,
		HostDevName: network.TapName,
		GuestMAC:    network.GuestMAC,
	})

	if spec.MMDS != nil {
		cfg.MMDSConfig = &vmMMDSConfig{
			Version:           spec.MMDS.Version,
			NetworkInterfaces: spec.MMDS.NetworkInterfaces,
			IPv4Address:       spec.MMDS.IPv4Address,
		}
	}

	if spec.Vsock != nil {
		cfg.Vsock = &vmVsock{
			GuestCID: int64(spec.Vsock.CID),
			UDSPath:  spec.Vsock.Path,
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal vm config: %w", err)
	}

	configPath := filepath.Join(chrootRootDir, "vm_config.json")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write vm config: %w", err)
	}

	return "/vm_config.json", nil
}
