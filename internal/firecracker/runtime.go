package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrMachineNotFound = errors.New("machine not found")

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

	mu       sync.RWMutex
	machines map[MachineID]*managedMachine
}

type managedMachine struct {
	cmd      *exec.Cmd
	entered  bool
	exited   chan struct{}
	network  NetworkAllocation
	paths    machinePaths
	spec     MachineSpec
	state    MachineState
	stopping bool
}

const (
	defaultVSockCIDStart = uint32(3)
	defaultVSockID       = "vsock0"
)

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
		machines:              make(map[MachineID]*managedMachine),
	}, nil
}

func (r *Runtime) Boot(ctx context.Context, spec MachineSpec) (*MachineState, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if _, exists := r.machines[spec.ID]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("machine %q already exists", spec.ID)
	}

	usedNetworks := make([]NetworkAllocation, 0, len(r.machines))
	usedVSockCIDs := make(map[uint32]struct{}, len(r.machines))
	for _, machine := range r.machines {
		if machine == nil {
			continue
		}
		usedNetworks = append(usedNetworks, machine.network)
		if machine.spec.Vsock != nil {
			usedVSockCIDs[machine.spec.Vsock.CID] = struct{}{}
		}
	}

	spec, err := r.resolveVSock(spec, usedVSockCIDs)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}

	r.machines[spec.ID] = &managedMachine{
		spec: spec,
		state: MachineState{
			ID:    spec.ID,
			Phase: PhaseProvisioning,
		},
		entered: true,
	}
	r.mu.Unlock()

	cleanup := func(network NetworkAllocation, paths machinePaths, command *exec.Cmd) {
		cleanupStartedProcess(command)
		_ = r.networkProvisioner.Remove(context.Background(), network)
		_ = removeIfExists(hostVSockPath(paths, spec))
		if paths.BaseDir != "" {
			_ = os.RemoveAll(paths.BaseDir)
		}
		r.mu.Lock()
		delete(r.machines, spec.ID)
		r.mu.Unlock()
	}

	network, err := r.networkAllocator.Allocate(usedNetworks)
	if err != nil {
		cleanup(NetworkAllocation{}, machinePaths{}, nil)
		return nil, err
	}

	paths, err := buildMachinePaths(r.rootDir, spec.ID, r.firecrackerBinaryPath)
	if err != nil {
		cleanup(network, machinePaths{}, nil)
		return nil, err
	}
	if err := os.MkdirAll(paths.JailerBaseDir, 0o755); err != nil {
		cleanup(network, paths, nil)
		return nil, fmt.Errorf("create machine jailer dir %q: %w", paths.JailerBaseDir, err)
	}
	if err := r.networkProvisioner.Ensure(ctx, network); err != nil {
		cleanup(network, paths, nil)
		return nil, err
	}

	command, err := launchJailedFirecracker(paths, spec.ID, r.firecrackerBinaryPath, r.jailerBinaryPath)
	if err != nil {
		cleanup(network, paths, nil)
		return nil, err
	}

	client := newAPIClient(paths.SocketPath)
	if err := waitForSocket(ctx, client, paths.SocketPath); err != nil {
		cleanup(network, paths, command)
		return nil, fmt.Errorf("wait for firecracker socket: %w", err)
	}

	jailedSpec, err := stageMachineFiles(spec, paths)
	if err != nil {
		cleanup(network, paths, command)
		return nil, err
	}
	if err := configureMachine(ctx, client, jailedSpec, network); err != nil {
		cleanup(network, paths, command)
		return nil, err
	}

	pid := 0
	if command.Process != nil {
		pid = command.Process.Pid
	}

	now := time.Now().UTC()
	state := MachineState{
		ID:          spec.ID,
		Phase:       PhaseRunning,
		PID:         pid,
		RuntimeHost: network.GuestIP().String(),
		SocketPath:  paths.SocketPath,
		TapName:     network.TapName,
		StartedAt:   &now,
	}

	r.mu.Lock()
	entry := r.machines[spec.ID]
	entry.cmd = command
	entry.exited = make(chan struct{})
	entry.network = network
	entry.paths = paths
	entry.state = state
	r.mu.Unlock()

	go r.watchMachine(spec.ID, command, entry.exited)

	out := state
	return &out, nil
}

func (r *Runtime) Inspect(id MachineID) (*MachineState, error) {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return nil, ErrMachineNotFound
	}

	state := entry.state
	if state.PID > 0 && !processExists(state.PID) {
		state.Phase = PhaseStopped
		state.PID = 0
	}
	return &state, nil
}

func (r *Runtime) Stop(ctx context.Context, id MachineID) error {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return ErrMachineNotFound
	}
	if entry.cmd == nil || entry.cmd.Process == nil {
		return fmt.Errorf("machine %q has no firecracker process", id)
	}
	if entry.state.Phase == PhaseStopped {
		return nil
	}

	r.mu.Lock()
	entry.stopping = true
	process := entry.cmd.Process
	exited := entry.exited
	r.mu.Unlock()

	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop machine %q: %w", id, err)
	}

	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) Delete(ctx context.Context, id MachineID) error {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return ErrMachineNotFound
	}

	if entry.state.Phase == PhaseRunning {
		if err := r.Stop(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if err := r.networkProvisioner.Remove(ctx, entry.network); err != nil {
		return err
	}
	if err := removeIfExists(hostVSockPath(entry.paths, entry.spec)); err != nil {
		return err
	}
	if err := os.RemoveAll(entry.paths.BaseDir); err != nil {
		return fmt.Errorf("remove machine dir %q: %w", entry.paths.BaseDir, err)
	}

	r.mu.Lock()
	delete(r.machines, id)
	r.mu.Unlock()
	return nil
}

func (r *Runtime) watchMachine(id MachineID, command *exec.Cmd, exited chan struct{}) {
	err := command.Wait()
	close(exited)

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.machines[id]
	if !ok || entry == nil || entry.cmd != command {
		return
	}

	entry.state.PID = 0
	if entry.stopping {
		entry.state.Phase = PhaseStopped
		entry.state.Error = ""
		entry.stopping = false
		return
	}
	if err != nil {
		entry.state.Phase = PhaseError
		entry.state.Error = err.Error()
		return
	}

	entry.state.Phase = PhaseStopped
	entry.state.Error = ""
}

func (r *Runtime) resolveVSock(spec MachineSpec, used map[uint32]struct{}) (MachineSpec, error) {
	if spec.Vsock != nil {
		if _, exists := used[spec.Vsock.CID]; exists {
			return MachineSpec{}, fmt.Errorf("vsock cid %d already in use", spec.Vsock.CID)
		}
		return spec, nil
	}

	cid, err := nextVSockCID(used)
	if err != nil {
		return MachineSpec{}, err
	}

	spec.Vsock = &VsockSpec{
		ID:   defaultVSockID,
		CID:  cid,
		Path: string(spec.ID) + ".sock",
	}
	return spec, nil
}

func nextVSockCID(used map[uint32]struct{}) (uint32, error) {
	for cid := defaultVSockCIDStart; cid != 0; cid++ {
		if _, exists := used[cid]; !exists {
			return cid, nil
		}
	}
	return 0, fmt.Errorf("vsock cid space exhausted")
}

func processExists(pid int) bool {
	if pid < 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func removeIfExists(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove path %q: %w", path, err)
	}
	return nil
}
