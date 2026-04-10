package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
)

const (
	defaultGuestKernelArgs  = "console=ttyS0 reboot=k panic=1 pci=off"
	defaultGuestMemoryMiB   = int64(512)
	defaultGuestVCPUs       = int64(1)
	defaultSSHPort          = uint16(2222)
	defaultVNCPort          = uint16(6080)
	defaultCopyBufferSize   = 1024 * 1024
	defaultGuestDialTimeout = 500 * time.Millisecond
)

type Runtime interface {
	Boot(context.Context, firecracker.MachineSpec, []firecracker.NetworkAllocation) (*firecracker.MachineState, error)
	Inspect(firecracker.MachineState) (*firecracker.MachineState, error)
	Delete(context.Context, firecracker.MachineState) error
	Pause(context.Context, firecracker.MachineState) error
	Resume(context.Context, firecracker.MachineState) error
	CreateSnapshot(context.Context, firecracker.MachineState, firecracker.SnapshotPaths) error
	RestoreBoot(context.Context, firecracker.SnapshotLoadSpec, []firecracker.NetworkAllocation) (*firecracker.MachineState, error)
	PutMMDS(context.Context, firecracker.MachineState, any) error
}

type Daemon struct {
	config  appconfig.Config
	store   store.Store
	runtime Runtime

	reconfigureGuestIdentity func(context.Context, string, contracthost.MachineID, *contracthost.GuestConfig) error
	readGuestSSHPublicKey    func(context.Context, string) (string, error)
	syncGuestFilesystem      func(context.Context, string) error
	personalizeGuest         func(context.Context, *model.MachineRecord, firecracker.MachineState) error

	locksMu       sync.Mutex
	machineLocks  map[contracthost.MachineID]*sync.Mutex
	artifactLocks map[string]*sync.Mutex

	relayAllocMu           sync.Mutex
	machineRelaysMu        sync.Mutex
	machineRelayListeners  map[string]net.Listener
	publishedPortAllocMu   sync.Mutex
	publishedPortsMu       sync.Mutex
	publishedPortListeners map[contracthost.PublishedPortID]net.Listener
}

func New(cfg appconfig.Config, store store.Store, runtime Runtime) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("runtime is required")
	}
	for _, dir := range []string{cfg.ArtifactsDir, cfg.MachineDisksDir, cfg.SnapshotsDir, cfg.RuntimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create daemon dir %q: %w", dir, err)
		}
	}
	daemon := &Daemon{
		config:                   cfg,
		store:                    store,
		runtime:                  runtime,
		reconfigureGuestIdentity: nil,
		readGuestSSHPublicKey:    nil,
		personalizeGuest:         nil,
		machineLocks:             make(map[contracthost.MachineID]*sync.Mutex),
		artifactLocks:            make(map[string]*sync.Mutex),
		machineRelayListeners:    make(map[string]net.Listener),
		publishedPortListeners:   make(map[contracthost.PublishedPortID]net.Listener),
	}
	daemon.reconfigureGuestIdentity = daemon.reconfigureGuestIdentityOverSSH
	daemon.readGuestSSHPublicKey = readGuestSSHPublicKey
	daemon.syncGuestFilesystem = daemon.syncGuestFilesystemOverSSH
	daemon.personalizeGuest = daemon.personalizeGuestConfig
	if err := daemon.ensureBackendSSHKeyPair(); err != nil {
		return nil, err
	}
	return daemon, nil
}

func (d *Daemon) Health(ctx context.Context) (*contracthost.HealthResponse, error) {
	if _, err := d.store.ListMachines(ctx); err != nil {
		return nil, err
	}
	return &contracthost.HealthResponse{OK: true}, nil
}

func (d *Daemon) lockMachine(machineID contracthost.MachineID) func() {
	d.locksMu.Lock()
	lock, ok := d.machineLocks[machineID]
	if !ok {
		lock = &sync.Mutex{}
		d.machineLocks[machineID] = lock
	}
	d.locksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func (d *Daemon) lockArtifact(key string) func() {
	d.locksMu.Lock()
	lock, ok := d.artifactLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		d.artifactLocks[key] = lock
	}
	d.locksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}
