package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
)

const (
	defaultGuestKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	defaultGuestMemoryMiB  = 512
	defaultGuestVCPUs      = 1
	defaultVSockCID        = 3
	defaultVSockID         = "vsock0"
)

// Config contains the minimum host-local settings required to boot a machine.
type Config struct {
	Runtime RuntimeConfig
	Machine MachineConfig
	VSock   VSockConfig
}

// RuntimeConfig contains host-local runtime settings.
type RuntimeConfig struct {
	RootDir               string
	FirecrackerBinaryPath string
	JailerBinaryPath      string
	JailerUID             int
	JailerGID             int
	NumaNode              int
	NetworkCIDR           string
}

// MachineConfig contains the default guest boot settings.
type MachineConfig struct {
	KernelImagePath string
	RootFSPath      string
	KernelArgs      string
	VCPUs           int64
	MemoryMiB       int64
}

// VSockConfig contains optional default vsock settings.
type VSockConfig struct {
	Enabled bool
	BaseDir string
	ID      string
	CID     uint32
}

// Load loads and validates the firecracker-host configuration from the
// environment.
func Load() (Config, error) {
	cfg := Config{
		Runtime: RuntimeConfig{
			RootDir:               strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_ROOT_DIR")),
			FirecrackerBinaryPath: strings.TrimSpace(os.Getenv("FIRECRACKER_BINARY_PATH")),
			JailerBinaryPath:      strings.TrimSpace(os.Getenv("JAILER_BINARY_PATH")),
			JailerUID:             os.Getuid(),
			JailerGID:             os.Getgid(),
			NetworkCIDR:           strings.TrimSpace(os.Getenv("FIRECRACKER_NETWORK_CIDR")),
		},
		Machine: MachineConfig{
			KernelImagePath: strings.TrimSpace(os.Getenv("FIRECRACKER_GUEST_KERNEL_PATH")),
			RootFSPath:      strings.TrimSpace(os.Getenv("FIRECRACKER_GUEST_ROOTFS_PATH")),
			KernelArgs:      defaultGuestKernelArgs,
			VCPUs:           defaultGuestVCPUs,
			MemoryMiB:       defaultGuestMemoryMiB,
		},
		VSock: VSockConfig{
			ID:  defaultVSockID,
			CID: defaultVSockCID,
		},
	}

	if value := strings.TrimSpace(os.Getenv("FIRECRACKER_GUEST_KERNEL_ARGS")); value != "" {
		cfg.Machine.KernelArgs = value
	}
	if value := strings.TrimSpace(os.Getenv("FIRECRACKER_BINARY_PATH")); value == "" {
		cfg.Runtime.FirecrackerBinaryPath = "firecracker"
	}
	if value := strings.TrimSpace(os.Getenv("JAILER_BINARY_PATH")); value == "" {
		cfg.Runtime.JailerBinaryPath = "jailer"
	}

	if value, ok, err := lookupIntEnv("FIRECRACKER_JAILER_UID"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Runtime.JailerUID = value
	}
	if value, ok, err := lookupIntEnv("FIRECRACKER_JAILER_GID"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Runtime.JailerGID = value
	}
	if value, ok, err := lookupIntEnv("FIRECRACKER_NUMA_NODE"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Runtime.NumaNode = value
	}
	if value, ok, err := lookupInt64Env("FIRECRACKER_GUEST_VCPUS"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Machine.VCPUs = value
	}
	if value, ok, err := lookupInt64Env("FIRECRACKER_GUEST_MEMORY_MIB"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Machine.MemoryMiB = value
	}
	if value, ok, err := lookupBoolEnv("FIRECRACKER_VSOCK_ENABLED"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.VSock.Enabled = value
	}
	if value := strings.TrimSpace(os.Getenv("FIRECRACKER_VSOCK_BASE_DIR")); value != "" {
		cfg.VSock.BaseDir = value
	}
	if value := strings.TrimSpace(os.Getenv("FIRECRACKER_VSOCK_ID")); value != "" {
		cfg.VSock.ID = value
	}
	if value, ok, err := lookupUint32Env("FIRECRACKER_VSOCK_CID"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.VSock.CID = value
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the host configuration is usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Runtime.RootDir) == "" {
		return fmt.Errorf("FIRECRACKER_HOST_ROOT_DIR is required")
	}
	if strings.TrimSpace(c.Machine.KernelImagePath) == "" {
		return fmt.Errorf("FIRECRACKER_GUEST_KERNEL_PATH is required")
	}
	if strings.TrimSpace(c.Machine.RootFSPath) == "" {
		return fmt.Errorf("FIRECRACKER_GUEST_ROOTFS_PATH is required")
	}
	if c.Machine.VCPUs < 1 {
		return fmt.Errorf("FIRECRACKER_GUEST_VCPUS must be at least 1")
	}
	if c.Machine.MemoryMiB < 1 {
		return fmt.Errorf("FIRECRACKER_GUEST_MEMORY_MIB must be at least 1")
	}
	if c.Runtime.NumaNode < 0 {
		return fmt.Errorf("FIRECRACKER_NUMA_NODE must be non-negative")
	}
	if c.VSock.Enabled {
		if strings.TrimSpace(c.VSock.BaseDir) == "" {
			return fmt.Errorf("FIRECRACKER_VSOCK_BASE_DIR is required when FIRECRACKER_VSOCK_ENABLED is true")
		}
		if c.VSock.CID == 0 {
			return fmt.Errorf("FIRECRACKER_VSOCK_CID must be non-zero when FIRECRACKER_VSOCK_ENABLED is true")
		}
		if strings.TrimSpace(c.VSock.ID) == "" {
			return fmt.Errorf("FIRECRACKER_VSOCK_ID is required when FIRECRACKER_VSOCK_ENABLED is true")
		}
	}
	return nil
}

// FirecrackerRuntimeConfig converts the host config into the runtime wrapper's
// concrete runtime config.
func (c Config) FirecrackerRuntimeConfig() firecracker.RuntimeConfig {
	return firecracker.RuntimeConfig{
		RootDir:               c.Runtime.RootDir,
		FirecrackerBinaryPath: c.Runtime.FirecrackerBinaryPath,
		JailerBinaryPath:      c.Runtime.JailerBinaryPath,
		JailerUID:             c.Runtime.JailerUID,
		JailerGID:             c.Runtime.JailerGID,
		NumaNode:              c.Runtime.NumaNode,
		NetworkCIDR:           c.Runtime.NetworkCIDR,
	}
}

func lookupBoolEnv(key string) (bool, bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, false, nil
	}

	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, false, fmt.Errorf("parse %s: %w", key, err)
	}
	return value, true, nil
}

func lookupIntEnv(key string) (int, bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}

	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", key, err)
	}
	return value, true, nil
}

func lookupInt64Env(key string) (int64, bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}

	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", key, err)
	}
	return value, true, nil
}

func lookupUint32Env(key string) (uint32, bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}

	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", key, err)
	}
	return uint32(value), true, nil
}
