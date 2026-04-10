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
	EgressInterface       string
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
	egressInterface := strings.TrimSpace(cfg.EgressInterface)
	if egressInterface == "" {
		return nil, fmt.Errorf("egress interface is required")
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
		networkProvisioner:    NewIPTapProvisioner(defaultNetworkCIDR, egressInterface),
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

func (r *Runtime) Pause(ctx context.Context, state MachineState) error {
	client := newAPIClient(state.SocketPath)
	return client.PatchVm(ctx, VmStatePaused)
}

func (r *Runtime) Resume(ctx context.Context, state MachineState) error {
	client := newAPIClient(state.SocketPath)
	return client.PatchVm(ctx, VmStateResumed)
}

func (r *Runtime) CreateSnapshot(ctx context.Context, state MachineState, paths SnapshotPaths) error {
	client := newAPIClient(state.SocketPath)
	return client.PutSnapshotCreate(ctx, SnapshotCreateParams{
		MemFilePath:  paths.MemFilePath,
		SnapshotPath: paths.StateFilePath,
		SnapshotType: "Full",
	})
}

func (r *Runtime) RestoreBoot(ctx context.Context, loadSpec SnapshotLoadSpec, usedNetworks []NetworkAllocation) (*MachineState, error) {
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

	var network NetworkAllocation
	if loadSpec.Network != nil {
		network = *loadSpec.Network
	} else {
		var err error
		network, err = r.networkAllocator.Allocate(usedNetworks)
		if err != nil {
			return nil, err
		}
	}

	paths, err := buildMachinePaths(r.rootDir, loadSpec.ID, r.firecrackerBinaryPath)
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

	command, err := launchJailedFirecracker(paths, loadSpec.ID, r.firecrackerBinaryPath, r.jailerBinaryPath)
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

	// Stage snapshot files and disk images into the chroot
	chrootMemPath, err := stageSnapshotFile(loadSpec.MemFilePath, paths.ChrootRootDir, "memory.bin")
	if err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("stage memory file: %w", err)
	}
	chrootStatePath, err := stageSnapshotFile(loadSpec.SnapshotPath, paths.ChrootRootDir, "vmstate.bin")
	if err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("stage vmstate file: %w", err)
	}

	// Stage root filesystem
	rootFSName, err := stagedFileName(loadSpec.RootFSPath)
	if err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("rootfs path: %w", err)
	}
	if err := linkMachineFile(loadSpec.RootFSPath, filepath.Join(paths.ChrootRootDir, rootFSName)); err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("link rootfs into jail: %w", err)
	}

	// Stage additional drives
	for driveID, drivePath := range loadSpec.DiskPaths {
		driveName, err := stagedFileName(drivePath)
		if err != nil {
			cleanup(network, paths, command, firecrackerPID)
			return nil, fmt.Errorf("drive %q path: %w", driveID, err)
		}
		if err := linkMachineFile(drivePath, filepath.Join(paths.ChrootRootDir, driveName)); err != nil {
			cleanup(network, paths, command, firecrackerPID)
			return nil, fmt.Errorf("link drive %q into jail: %w", driveID, err)
		}
	}

	var vsockOverride *VsockOverride
	if loadSpec.Vsock != nil {
		vsockOverride = &VsockOverride{UDSPath: jailedVSockDevicePath(*loadSpec.Vsock)}
	}

	// Load snapshot (replaces the full configure+start sequence)
	if err := client.PutSnapshotLoad(ctx, SnapshotLoadParams{
		SnapshotPath: chrootStatePath,
		MemBackend: &MemBackend{
			BackendType: "File",
			BackendPath: chrootMemPath,
		},
		ResumeVm: false,
		NetworkOverrides: []NetworkOverride{
			{
				IfaceID:     network.InterfaceID,
				HostDevName: network.TapName,
			},
		},
		VsockOverride: vsockOverride,
	}); err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// Resume the restored VM
	if err := client.PatchVm(ctx, VmStateResumed); err != nil {
		cleanup(network, paths, command, firecrackerPID)
		return nil, fmt.Errorf("resume restored vm: %w", err)
	}

	now := time.Now().UTC()
	state := MachineState{
		ID:          loadSpec.ID,
		Phase:       PhaseRunning,
		PID:         firecrackerPID,
		RuntimeHost: network.GuestIP().String(),
		SocketPath:  socketPath,
		TapName:     network.TapName,
		StartedAt:   &now,
	}
	return &state, nil
}

func (r *Runtime) PutMMDS(ctx context.Context, state MachineState, data any) error {
	client := newAPIClient(state.SocketPath)
	return client.PutMMDS(ctx, data)
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
