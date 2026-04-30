package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AgentComputerAI/computer-host/internal/firecracker"
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
	DiskCloneMode         DiskCloneMode
	DriveIOEngine         firecracker.DriveIOEngine
	EnablePCI             bool
	SocketPath            string
	HTTPAddr              string
	EgressInterface       string
	ReconcileInterval     time.Duration
	FirecrackerBinaryPath string
	JailerBinaryPath      string
	GuestLoginCAPublicKey string
}

// DiskCloneMode controls how the daemon materializes writable machine disks.
type DiskCloneMode string

const (
	// DiskCloneModeReflink requires an O(1) copy-on-write clone and never falls back to a full copy.
	DiskCloneModeReflink DiskCloneMode = "reflink"
	// DiskCloneModeCopy performs a full sparse copy. Use only for local development or emergency fallback.
	DiskCloneModeCopy DiskCloneMode = "copy"
)

// Load loads and validates the firecracker-host daemon configuration from the environment.
func Load() (Config, error) {
	rootDir := filepath.Clean(strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_ROOT_DIR")))
	enablePCI, err := loadBool("FIRECRACKER_HOST_ENABLE_PCI")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		RootDir:               rootDir,
		StatePath:             filepath.Join(rootDir, "state", "state.json"),
		OperationsPath:        filepath.Join(rootDir, "state", "ops.json"),
		ArtifactsDir:          filepath.Join(rootDir, "artifacts"),
		MachineDisksDir:       filepath.Join(rootDir, "machine-disks"),
		SnapshotsDir:          filepath.Join(rootDir, "snapshots"),
		RuntimeDir:            filepath.Join(rootDir, "runtime"),
		DiskCloneMode:         loadDiskCloneMode(os.Getenv("FIRECRACKER_HOST_DISK_CLONE_MODE")),
		DriveIOEngine:         loadDriveIOEngine(os.Getenv("FIRECRACKER_HOST_DRIVE_IO_ENGINE")),
		EnablePCI:             enablePCI,
		SocketPath:            filepath.Join(rootDir, defaultSocketName),
		HTTPAddr:              strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_HTTP_ADDR")),
		EgressInterface:       strings.TrimSpace(os.Getenv("FIRECRACKER_HOST_EGRESS_INTERFACE")),
		FirecrackerBinaryPath: strings.TrimSpace(os.Getenv("FIRECRACKER_BINARY_PATH")),
		JailerBinaryPath:      strings.TrimSpace(os.Getenv("JAILER_BINARY_PATH")),
		GuestLoginCAPublicKey: strings.TrimSpace(os.Getenv("GUEST_LOGIN_CA_PUBLIC_KEY")),
	}
	cfg.ReconcileInterval, err = durationDefault("FIRECRACKER_HOST_RECONCILE_INTERVAL", 5*time.Second)
	if err != nil {
		return Config{}, err
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
	if err := c.DiskCloneMode.Validate(); err != nil {
		return err
	}
	if err := validateDriveIOEngine(c.DriveIOEngine); err != nil {
		return err
	}
	if strings.TrimSpace(c.SocketPath) == "" {
		return fmt.Errorf("socket path is required")
	}
	if strings.TrimSpace(c.EgressInterface) == "" {
		return fmt.Errorf("FIRECRACKER_HOST_EGRESS_INTERFACE is required")
	}
	if c.ReconcileInterval <= 0 {
		return fmt.Errorf("FIRECRACKER_HOST_RECONCILE_INTERVAL must be greater than zero")
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
		EnablePCI:             c.EnablePCI,
	}
}

func loadDiskCloneMode(raw string) DiskCloneMode {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DiskCloneModeReflink
	}
	return DiskCloneMode(value)
}

// Validate reports whether the clone mode is safe to use.
func (m DiskCloneMode) Validate() error {
	switch m {
	case DiskCloneModeReflink, DiskCloneModeCopy:
		return nil
	default:
		return fmt.Errorf("FIRECRACKER_HOST_DISK_CLONE_MODE must be %q or %q", DiskCloneModeReflink, DiskCloneModeCopy)
	}
}

func loadDriveIOEngine(raw string) firecracker.DriveIOEngine {
	value := strings.TrimSpace(raw)
	if value == "" {
		return firecracker.DriveIOEngineSync
	}
	return firecracker.DriveIOEngine(value)
}

func validateDriveIOEngine(engine firecracker.DriveIOEngine) error {
	switch engine {
	case firecracker.DriveIOEngineSync, firecracker.DriveIOEngineAsync:
		return nil
	default:
		return fmt.Errorf("FIRECRACKER_HOST_DRIVE_IO_ENGINE must be %q or %q", firecracker.DriveIOEngineSync, firecracker.DriveIOEngineAsync)
	}
}

func loadBool(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func durationDefault(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}
