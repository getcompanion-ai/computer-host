package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
)

// Config contains the minimum host-local settings required to boot a machine.
type Config struct {
	RootDir               string
	FirecrackerBinaryPath string
	JailerBinaryPath      string
	KernelImagePath       string
	RootFSPath            string
}

// Load loads and validates the firecracker-host configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		RootDir:               strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_ROOT_DIR")),
		FirecrackerBinaryPath: strings.TrimSpace(os.Getenv("FIRECRACKER_BINARY_PATH")),
		JailerBinaryPath:      strings.TrimSpace(os.Getenv("JAILER_BINARY_PATH")),
		KernelImagePath:       strings.TrimSpace(os.Getenv("FIRECRACKER_GUEST_KERNEL_PATH")),
		RootFSPath:            strings.TrimSpace(os.Getenv("FIRECRACKER_GUEST_ROOTFS_PATH")),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports whether the host configuration is usable.
func (c Config) Validate() error {
	if c.RootDir == "" {
		return fmt.Errorf("FIRECRACKER_HOST_ROOT_DIR is required")
	}
	if c.FirecrackerBinaryPath == "" {
		return fmt.Errorf("FIRECRACKER_BINARY_PATH is required")
	}
	if c.JailerBinaryPath == "" {
		return fmt.Errorf("JAILER_BINARY_PATH is required")
	}
	if c.KernelImagePath == "" {
		return fmt.Errorf("FIRECRACKER_GUEST_KERNEL_PATH is required")
	}
	if c.RootFSPath == "" {
		return fmt.Errorf("FIRECRACKER_GUEST_ROOTFS_PATH is required")
	}
	return nil
}

// FirecrackerRuntimeConfig converts the host config into the runtime wrapper's concrete runtime config.
func (c Config) FirecrackerRuntimeConfig() firecracker.RuntimeConfig {
	return firecracker.RuntimeConfig{
		RootDir:               c.RootDir,
		FirecrackerBinaryPath: c.FirecrackerBinaryPath,
		JailerBinaryPath:      c.JailerBinaryPath,
	}
}
