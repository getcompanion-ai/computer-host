package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
)

const defaultSocketName = "firecracker-host.sock"

// Config contains the host-local daemon settings.
type Config struct {
	RootDir               string
	StatePath             string
	OperationsPath        string
	ArtifactsDir          string
	MachineDisksDir       string
	SnapshotsDir          string
	RuntimeDir            string
	SocketPath            string
	EgressInterface       string
	FirecrackerBinaryPath string
	JailerBinaryPath      string
}

// Load loads and validates the firecracker-host daemon configuration from the environment.
func Load() (Config, error) {
	rootDir := filepath.Clean(strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_ROOT_DIR")))
	cfg := Config{
		RootDir:               rootDir,
		StatePath:             filepath.Join(rootDir, "state", "state.json"),
		OperationsPath:        filepath.Join(rootDir, "state", "ops.json"),
		ArtifactsDir:          filepath.Join(rootDir, "artifacts"),
		MachineDisksDir:       filepath.Join(rootDir, "machine-disks"),
		SnapshotsDir:          filepath.Join(rootDir, "snapshots"),
		RuntimeDir:            filepath.Join(rootDir, "runtime"),
		SocketPath:            filepath.Join(rootDir, defaultSocketName),
		EgressInterface:       strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_EGRESS_INTERFACE")),
		FirecrackerBinaryPath: strings.TrimSpace(os.Getenv("FIRECRACKER_BINARY_PATH")),
		JailerBinaryPath:      strings.TrimSpace(os.Getenv("JAILER_BINARY_PATH")),
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
	if strings.TrimSpace(c.StatePath) == "" {
		return fmt.Errorf("state path is required")
	}
	if strings.TrimSpace(c.OperationsPath) == "" {
		return fmt.Errorf("operations path is required")
	}
	if strings.TrimSpace(c.ArtifactsDir) == "" {
		return fmt.Errorf("artifacts dir is required")
	}
	if strings.TrimSpace(c.MachineDisksDir) == "" {
		return fmt.Errorf("machine disks dir is required")
	}
	if strings.TrimSpace(c.SnapshotsDir) == "" {
		return fmt.Errorf("snapshots dir is required")
	}
	if strings.TrimSpace(c.RuntimeDir) == "" {
		return fmt.Errorf("runtime dir is required")
	}
	if strings.TrimSpace(c.SocketPath) == "" {
		return fmt.Errorf("socket path is required")
	}
	if strings.TrimSpace(c.EgressInterface) == "" {
		return fmt.Errorf("FIRECRACKER_HOST_EGRESS_INTERFACE is required")
	}
	return nil
}

// FirecrackerRuntimeConfig converts the daemon config into the Firecracker runtime config.
func (c Config) FirecrackerRuntimeConfig() firecracker.RuntimeConfig {
	return firecracker.RuntimeConfig{
		RootDir:               c.RuntimeDir,
		EgressInterface:       c.EgressInterface,
		FirecrackerBinaryPath: c.FirecrackerBinaryPath,
		JailerBinaryPath:      c.JailerBinaryPath,
	}
}
