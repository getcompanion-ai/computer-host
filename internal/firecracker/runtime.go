package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
)

// ErrMachineNotFound is returned when the runtime does not know a machine ID.
var ErrMachineNotFound = errors.New("machine not found")

// RuntimeConfig configures the host-local Firecracker runtime wrapper.
type RuntimeConfig struct {
	RootDir               string
	FirecrackerBinaryPath string
	JailerBinaryPath      string
}

// Runtime manages local Firecracker machines on a single host.
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
	spec    MachineSpec
	network NetworkAllocation
	paths   machinePaths
	machine *sdk.Machine
	state   MachineState
}

const (
	defaultVSockCIDStart = uint32(3)
	defaultVSockDirName  = "vsock"
	defaultVSockID       = "vsock0"
)

// NewRuntime creates a new host-local Firecracker runtime wrapper.
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
	if err := os.MkdirAll(filepath.Join(rootDir, defaultVSockDirName), 0o755); err != nil {
		return nil, fmt.Errorf("create runtime vsock dir: %w", err)
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

// Boot provisions host resources and starts a new jailed Firecracker process.
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
	}
	r.mu.Unlock()

	cleanup := func(network NetworkAllocation, paths machinePaths) {
		_ = r.networkProvisioner.Remove(context.Background(), network)
		_ = removeIfExists(vsockPath(spec))
		if paths.BaseDir != "" {
			_ = os.RemoveAll(paths.BaseDir)
		}
		r.mu.Lock()
		delete(r.machines, spec.ID)
		r.mu.Unlock()
	}

	network, err := r.networkAllocator.Allocate(usedNetworks)
	if err != nil {
		cleanup(NetworkAllocation{}, machinePaths{})
		return nil, err
	}

	paths, err := buildMachinePaths(r.rootDir, spec.ID, r.firecrackerBinaryPath)
	if err != nil {
		cleanup(network, machinePaths{})
		return nil, err
	}
	if err := os.MkdirAll(paths.JailerBaseDir, 0o755); err != nil {
		cleanup(network, paths)
		return nil, fmt.Errorf("create machine jailer dir %q: %w", paths.JailerBaseDir, err)
	}
	if err := r.networkProvisioner.Ensure(ctx, network); err != nil {
		cleanup(network, paths)
		return nil, err
	}

	cfg, err := buildSDKConfig(spec, paths, network, r.firecrackerBinaryPath, r.jailerBinaryPath)
	if err != nil {
		cleanup(network, paths)
		return nil, err
	}

	machine, err := sdk.NewMachine(ctx, cfg)
	if err != nil {
		cleanup(network, paths)
		return nil, fmt.Errorf("create firecracker machine: %w", err)
	}
	if err := machine.Start(ctx); err != nil {
		cleanup(network, paths)
		return nil, fmt.Errorf("start firecracker machine: %w", err)
	}

	pid, _ := machine.PID()
	now := time.Now().UTC()
	state := MachineState{
		ID:          spec.ID,
		Phase:       PhaseRunning,
		PID:         pid,
		RuntimeHost: network.GuestIP().String(),
		SocketPath:  machine.Cfg.SocketPath,
		TapName:     network.TapName,
		StartedAt:   &now,
	}

	r.mu.Lock()
	entry := r.machines[spec.ID]
	entry.network = network
	entry.paths = paths
	entry.machine = machine
	entry.state = state
	r.mu.Unlock()

	go r.watchMachine(spec.ID, machine)

	out := state
	return &out, nil
}

// Inspect returns the currently known state for a machine.
func (r *Runtime) Inspect(id MachineID) (*MachineState, error) {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return nil, ErrMachineNotFound
	}

	state := entry.state
	if entry.machine != nil {
		pid, err := entry.machine.PID()
		if err != nil {
			if state.Phase == PhaseRunning {
				state.Phase = PhaseStopped
				state.PID = 0
			}
		} else {
			state.PID = pid
		}
	}

	return &state, nil
}

// Stop terminates a running Firecracker process and updates local state.
func (r *Runtime) Stop(ctx context.Context, id MachineID) error {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return ErrMachineNotFound
	}
	if entry.machine == nil {
		return fmt.Errorf("machine %q has no firecracker process", id)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- entry.machine.StopVMM()
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("stop machine %q: %w", id, err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	r.mu.Lock()
	entry.state.Phase = PhaseStopped
	entry.state.PID = 0
	entry.state.Error = ""
	r.mu.Unlock()

	return nil
}

// Delete stops a machine if necessary and removes its local resources.
func (r *Runtime) Delete(ctx context.Context, id MachineID) error {
	r.mu.RLock()
	entry, ok := r.machines[id]
	r.mu.RUnlock()
	if !ok || entry == nil {
		return ErrMachineNotFound
	}

	if entry.machine != nil {
		if err := r.Stop(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	if err := r.networkProvisioner.Remove(ctx, entry.network); err != nil {
		return err
	}
	if err := removeIfExists(vsockPath(entry.spec)); err != nil {
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

func (r *Runtime) watchMachine(id MachineID, machine *sdk.Machine) {
	err := machine.Wait(context.Background())

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.machines[id]
	if !ok || entry == nil || entry.machine != machine {
		return
	}

	entry.state.PID = 0
	if err != nil {
		entry.state.Phase = PhaseError
		entry.state.Error = err.Error()
		return
	}

	if entry.state.Phase != PhaseStopped {
		entry.state.Phase = PhaseStopped
	}
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
		Path: filepath.Join(r.rootDir, defaultVSockDirName, string(spec.ID)+".sock"),
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

func vsockPath(spec MachineSpec) string {
	if spec.Vsock == nil {
		return ""
	}
	return strings.TrimSpace(spec.Vsock.Path)
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
