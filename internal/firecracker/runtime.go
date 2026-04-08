package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrMachineNotFound = errors.New("machine not found")

var (
	stopGracePeriod  = 5 * time.Second
	stopPollInterval = 50 * time.Millisecond
)

type RuntimeConfig struct {
	RootDir               string
	FirecrackerBinaryPath string
	JailerBinaryPath      string
}

type Runtime struct {
	rootDir               string
	firecrackerBinaryPath string
	jailerBinaryPath      string
	networkAllocator      *NetworkAllocator
	networkProvisioner    NetworkProvisioner
}

const debugPreserveFailureEnv = "FIRECRACKER_DEBUG_PRESERVE_FAILURE"

func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	rootDir := filepath.Clean(strings.TrimSpace(cfg.RootDir))
	if rootDir == "." || rootDir == "" {
		return nil, fmt.Errorf("runtime root dir is required")
	}

	firecrackerBinaryPath := strings.TrimSpace(cfg.FirecrackerBinaryPath)
	if firecrackerBinaryPath == "" {
		return nil, fmt.Errorf("firecracker binary path is required")
	}

	jailerBinaryPath := strings.TrimSpace(cfg.JailerBinaryPath)
	if jailerBinaryPath == "" {
		return nil, fmt.Errorf("jailer binary path is required")
	}

	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("create runtime root dir %q: %w", rootDir, err)
	}

	allocator, err := NewNetworkAllocator(defaultNetworkCIDR)
	if err != nil {
		return nil, err
	}

	return &Runtime{
		rootDir:               rootDir,
		firecrackerBinaryPath: firecrackerBinaryPath,
		jailerBinaryPath:      jailerBinaryPath,
		networkAllocator:      allocator,
		networkProvisioner:    NewIPTapProvisioner(),
	}, nil
}

func (r *Runtime) Boot(ctx context.Context, spec MachineSpec, usedNetworks []NetworkAllocation) (*MachineState, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	cleanup := func(network NetworkAllocation, paths machinePaths, command *exec.Cmd, firecrackerPID int) {
		if preserveFailureArtifacts() {
			return
		}
		cleanupRunningProcess(firecrackerPID)
		cleanupStartedProcess(command)
		_ = r.networkProvisioner.Remove(context.Background(), network)
		if paths.BaseDir != "" {
			_ = os.RemoveAll(paths.BaseDir)
		}
	}

	network, err := r.networkAllocator.Allocate(usedNetworks)
	if err != nil {
		return nil, err
	}

	paths, err := buildMachinePaths(r.rootDir, spec.ID, r.firecrackerBinaryPath)
	if err != nil {
		cleanup(network, machinePaths{}, nil, 0)
		return nil, err
	}
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		cleanup(network, paths, nil, 0)
		return nil, fmt.Errorf("create machine log dir %q: %w", paths.LogDir, err)
	}
	if err := r.networkProvisioner.Ensure(ctx, network); err != nil {
		cleanup(network, paths, nil, 0)
		return nil, err
	}

	command, err := launchJailedFirecracker(paths, spec.ID, r.firecrackerBinaryPath, r.jailerBinaryPath)
	if err != nil {
		cleanup(network, paths, nil, 0)
		return nil, err
	}
	firecrackerPID, err := waitForPIDFile(ctx, paths.PIDFilePath)
	if err != nil {
		cleanup(network, paths, command, 0)
		return nil, fmt.Errorf("wait for firecracker pid: %w", err)
	}

	socketPath := procSocketPath(firecrackerPID)
	client := newAPIClient(socketPath)
	if err := waitForSocket(ctx, client, socketPath); err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("wait for firecracker socket: %w", err)
	}

	jailedSpec, err := stageMachineFiles(spec, paths)
	if err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, err
	}
	if err := configureMachine(ctx, client, paths, jailedSpec, network); err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, err
	}

	now := time.Now().UTC()
	state := MachineState{
		ID:          spec.ID,
		Phase:       PhaseRunning,
		PID:         firecrackerPID,
		RuntimeHost: network.GuestIP().String(),
		SocketPath:  socketPath,
		TapName:     network.TapName,
		StartedAt:   &now,
	}
	return &state, nil
}

func (r *Runtime) Inspect(state MachineState) (*MachineState, error) {
	if state.PID > 0 && !processExists(state.PID) {
		state.Phase = PhaseFailed
		state.PID = 0
		state.Error = "firecracker process not found"
	}
	return &state, nil
}

func (r *Runtime) Stop(ctx context.Context, state MachineState) error {
	if state.PID < 1 || !processExists(state.PID) {
		return nil
	}

	process, err := os.FindProcess(state.PID)
	if err != nil {
		return fmt.Errorf("find process for machine %q: %w", state.ID, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop machine %q: %w", state.ID, err)
	}

	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	deadline := time.Now().Add(stopGracePeriod)
	sentKill := false

	for {
		if !processExists(state.PID) {
			return nil
		}
		if !sentKill && time.Now().After(deadline) {
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("kill machine %q: %w", state.ID, err)
			}
			sentKill = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runtime) Delete(ctx context.Context, state MachineState) error {
	if err := r.Stop(ctx, state); err != nil {
		return err
	}
	if strings.TrimSpace(state.RuntimeHost) != "" && strings.TrimSpace(state.TapName) != "" {
		network, err := AllocationFromGuestIP(state.RuntimeHost, state.TapName)
		if err != nil {
			return err
		}
		if err := r.networkProvisioner.Remove(ctx, network); err != nil {
			return err
		}
	}

	paths, err := buildMachinePaths(r.rootDir, state.ID, r.firecrackerBinaryPath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(paths.BaseDir); err != nil {
		return fmt.Errorf("remove machine dir %q: %w", paths.BaseDir, err)
	}
	return nil
}

func processExists(pid int) bool {
	if pid < 1 {
		return false
	}
	if payload, err := os.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", pid), "stat")); err == nil {
		if marker := strings.LastIndexByte(string(payload), ')'); marker >= 0 && marker+2 < len(payload) {
			if payload[marker+2] == 'Z' {
				return false
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func cleanupRunningProcess(pid int) {
	if pid < 1 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
}

func preserveFailureArtifacts() bool {
	value := strings.TrimSpace(os.Getenv(debugPreserveFailureEnv))
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
