package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type fakeRuntime struct {
	bootState       firecracker.MachineState
	bootCalls       int
	restoreCalls    int
	deleteCalls     []firecracker.MachineState
	lastSpec        firecracker.MachineSpec
	lastLoadSpec    firecracker.SnapshotLoadSpec
	mmdsWrites      []any
	inspectOverride func(firecracker.MachineState) (*firecracker.MachineState, error)
}

func (f *fakeRuntime) Boot(_ context.Context, spec firecracker.MachineSpec, _ []firecracker.NetworkAllocation) (*firecracker.MachineState, error) {
	f.bootCalls++
	f.lastSpec = spec
	state := f.bootState
	return &state, nil
}

func (f *fakeRuntime) Inspect(state firecracker.MachineState) (*firecracker.MachineState, error) {
	if f.inspectOverride != nil {
		return f.inspectOverride(state)
	}
	copy := state
	return &copy, nil
}

func (f *fakeRuntime) Delete(_ context.Context, state firecracker.MachineState) error {
	f.deleteCalls = append(f.deleteCalls, state)
	return nil
}

func (f *fakeRuntime) Pause(_ context.Context, _ firecracker.MachineState) error {
	return nil
}

func (f *fakeRuntime) Resume(_ context.Context, _ firecracker.MachineState) error {
	return nil
}

func (f *fakeRuntime) CreateSnapshot(_ context.Context, _ firecracker.MachineState, _ firecracker.SnapshotPaths) error {
	return nil
}

func (f *fakeRuntime) RestoreBoot(_ context.Context, spec firecracker.SnapshotLoadSpec, _ []firecracker.NetworkAllocation) (*firecracker.MachineState, error) {
	f.restoreCalls++
	f.lastLoadSpec = spec
	return &f.bootState, nil
}

func (f *fakeRuntime) PutMMDS(_ context.Context, _ firecracker.MachineState, data any) error {
	f.mmdsWrites = append(f.mmdsWrites, data)
	return nil
}

func TestCreateMachineStagesArtifactsAndPersistsState(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() {
		_ = sshListener.Close()
	}()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() {
		_ = vncListener.Close()
	}()

	startedAt := time.Unix(1700000005, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "vm-1",
			Phase:       firecracker.PhaseRunning,
			PID:         4321,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "vm-1", "root", "run", "firecracker.sock"),
			TapName:     "fctap0",
			StartedAt:   &startedAt,
		},
	}

	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	kernelPayload := []byte("kernel-image")
	rootFSImagePath := filepath.Join(root, "guest-rootfs.ext4")
	if err := buildTestExt4Image(root, rootFSImagePath); err != nil {
		t.Fatalf("build ext4 image: %v", err)
	}
	rootFSPayload, err := os.ReadFile(rootFSImagePath)
	if err != nil {
		t.Fatalf("read ext4 image: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kernel":
			_, _ = w.Write(kernelPayload)
		case "/rootfs":
			_, _ = w.Write(rootFSPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	response, err := hostDaemon.CreateMachine(context.Background(), contracthost.CreateMachineRequest{
		MachineID: "vm-1",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		MemoryMiB:    4096,
		StorageBytes: int64(12 * 1024 * 1024 * 1024),
		GuestConfig: &contracthost.GuestConfig{
			Hostname: "workbox",
			AuthorizedKeys: []string{
				"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestOverrideKey daemon-test",
			},
			LoginWebhook: &contracthost.GuestLoginWebhook{
				URL:         "https://example.com/login",
				BearerToken: "token",
			},
		},
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("machine phase mismatch: got %q", response.Machine.Phase)
	}
	if response.Machine.RuntimeHost != "127.0.0.1" {
		t.Fatalf("runtime host mismatch: got %q", response.Machine.RuntimeHost)
	}
	if len(response.Machine.Ports) != 3 {
		t.Fatalf("machine ports mismatch: got %d want 3", len(response.Machine.Ports))
	}
	if response.Machine.Ports[0].Port != defaultSSHPort || response.Machine.Ports[1].Port != defaultVNCPort || response.Machine.Ports[2].Port != defaultGuestdPort {
		t.Fatalf("machine ports mismatch: got %#v", response.Machine.Ports)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.lastSpec.MemoryMiB != 4096 {
		t.Fatalf("runtime memory mismatch: got %d want 4096", runtime.lastSpec.MemoryMiB)
	}
	if runtime.lastSpec.KernelImagePath == "" || runtime.lastSpec.RootFSPath == "" {
		t.Fatalf("runtime spec paths not populated: %#v", runtime.lastSpec)
	}
	if _, err := os.Stat(runtime.lastSpec.KernelImagePath); err != nil {
		t.Fatalf("kernel artifact not staged: %v", err)
	}
	if info, err := os.Stat(runtime.lastSpec.RootFSPath); err != nil {
		t.Fatalf("system disk not staged: %v", err)
	} else if info.Size() != 12*1024*1024*1024 {
		t.Fatalf("system disk size mismatch: got %d want %d", info.Size(), int64(12*1024*1024*1024))
	}
	hostAuthorizedKeyBytes, err := os.ReadFile(hostDaemon.backendSSHPublicKeyPath())
	if err != nil {
		t.Fatalf("read backend ssh public key: %v", err)
	}
	if runtime.lastSpec.MMDS == nil {
		t.Fatalf("expected MMDS configuration on machine spec")
	}
	if runtime.lastSpec.Vsock == nil {
		t.Fatalf("expected vsock configuration on machine spec")
	}
	if runtime.lastSpec.Vsock.ID != defaultGuestPersonalizationVsockID {
		t.Fatalf("vsock id mismatch: got %q", runtime.lastSpec.Vsock.ID)
	}
	if runtime.lastSpec.Vsock.CID < minGuestVsockCID {
		t.Fatalf("vsock cid mismatch: got %d", runtime.lastSpec.Vsock.CID)
	}
	if runtime.lastSpec.MMDS.Version != firecracker.MMDSVersionV2 {
		t.Fatalf("mmds version mismatch: got %q", runtime.lastSpec.MMDS.Version)
	}
	payload, ok := runtime.lastSpec.MMDS.Data.(guestMetadataEnvelope)
	if !ok {
		t.Fatalf("mmds payload type mismatch: got %T", runtime.lastSpec.MMDS.Data)
	}
	if payload.Latest.MetaData.Hostname != "workbox" {
		t.Fatalf("mmds hostname mismatch: got %q", payload.Latest.MetaData.Hostname)
	}
	authorizedKeys := strings.Join(payload.Latest.MetaData.AuthorizedKeys, "\n")
	if !strings.Contains(authorizedKeys, strings.TrimSpace(string(hostAuthorizedKeyBytes))) {
		t.Fatalf("mmds authorized_keys missing backend ssh key: %q", authorizedKeys)
	}
	if !strings.Contains(authorizedKeys, "daemon-test") {
		t.Fatalf("mmds authorized_keys missing request override key: %q", authorizedKeys)
	}

	artifact, err := fileStore.GetArtifact(context.Background(), response.Machine.Artifact)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if artifact.KernelImagePath == "" || artifact.RootFSPath == "" {
		t.Fatalf("artifact paths missing: %#v", artifact)
	}
	if payload, err := os.ReadFile(artifact.KernelImagePath); err != nil {
		t.Fatalf("read kernel artifact: %v", err)
	} else if string(payload) != string(kernelPayload) {
		t.Fatalf("kernel artifact payload mismatch: got %q", string(payload))
	}

	machine, err := fileStore.GetMachine(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if machine.SystemVolumeID != "vm-1-system" {
		t.Fatalf("system volume mismatch: got %q", machine.SystemVolumeID)
	}
	if machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("stored machine phase mismatch: got %q", machine.Phase)
	}
	if machine.MemoryMiB != 4096 {
		t.Fatalf("stored memory mismatch: got %d want 4096", machine.MemoryMiB)
	}
	if machine.StorageBytes != 12*1024*1024*1024 {
		t.Fatalf("stored storage mismatch: got %d want %d", machine.StorageBytes, int64(12*1024*1024*1024))
	}
	if machine.GuestConfig == nil || len(machine.GuestConfig.AuthorizedKeys) == 0 {
		t.Fatalf("stored guest config missing authorized keys: %#v", machine.GuestConfig)
	}
	machineName, err := readExt4File(runtime.lastSpec.RootFSPath, "/etc/microagent/machine-name")
	if err != nil {
		t.Fatalf("read injected machine-name: %v", err)
	}
	if machineName != "agentcomputer\n" {
		t.Fatalf("machine-name mismatch: got %q want %q", machineName, "agentcomputer\n")
	}
	injectedAuthorizedKeys, err := readExt4File(runtime.lastSpec.RootFSPath, "/etc/microagent/authorized_keys")
	if err != nil {
		t.Fatalf("read injected authorized_keys: %v", err)
	}
	if !strings.Contains(injectedAuthorizedKeys, strings.TrimSpace(string(hostAuthorizedKeyBytes))) {
		t.Fatalf("disk authorized_keys missing backend ssh key: %q", injectedAuthorizedKeys)
	}
	if !strings.Contains(injectedAuthorizedKeys, "daemon-test") {
		t.Fatalf("disk authorized_keys missing request override key: %q", injectedAuthorizedKeys)
	}

	operations, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operation journal should be empty after success: got %d entries", len(operations))
	}
}

func TestCreateMachineCleansSystemVolumeOnInjectFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.injectMachineIdentity = func(context.Context, string, contracthost.MachineID) error {
		return errors.New("inject failed")
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel-image"),
		"/rootfs": buildTestExt4ImageBytes(t),
	})
	defer server.Close()

	_, err = hostDaemon.CreateMachine(context.Background(), contracthost.CreateMachineRequest{
		MachineID: "vm-inject-fail",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "inject machine identity") {
		t.Fatalf("CreateMachine error = %v, want inject machine identity failure", err)
	}

	systemVolumePath := hostDaemon.systemVolumePath("vm-inject-fail")
	if _, statErr := os.Stat(systemVolumePath); !os.IsNotExist(statErr) {
		t.Fatalf("system volume should be cleaned up, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Dir(systemVolumePath)); !os.IsNotExist(statErr) {
		t.Fatalf("system volume dir should be cleaned up, stat err = %v", statErr)
	}
}

func TestResolveRequestedMemoryMiBFallsBackToLegacyDefault(t *testing.T) {
	if got := resolveRequestedMemoryMiB(0); got != defaultGuestMemoryMiB {
		t.Fatalf("resolveRequestedMemoryMiB(0) = %d, want %d", got, defaultGuestMemoryMiB)
	}
	if got := resolveRequestedMemoryMiB(2048); got != 2048 {
		t.Fatalf("resolveRequestedMemoryMiB(2048) = %d, want 2048", got)
	}
}

func TestResolveRequestedStorageBytesValidatesAgainstSourceDisk(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "rootfs.img")
	if err := os.WriteFile(sourcePath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write source disk: %v", err)
	}

	resolved, err := resolveRequestedStorageBytes(0, sourcePath)
	if err != nil {
		t.Fatalf("resolveRequestedStorageBytes default returned error: %v", err)
	}
	if resolved != defaultGuestDiskSizeBytes {
		t.Fatalf("default requested storage = %d, want %d", resolved, defaultGuestDiskSizeBytes)
	}

	if _, err := resolveRequestedStorageBytes(3, sourcePath); err == nil {
		t.Fatal("expected smaller-than-source storage to fail")
	}

	resolved, err = resolveRequestedStorageBytes(16, sourcePath)
	if err != nil {
		t.Fatalf("resolveRequestedStorageBytes explicit returned error: %v", err)
	}
	if resolved != 16 {
		t.Fatalf("explicit requested storage = %d, want 16", resolved)
	}
}

func TestStopMachineSyncsGuestFilesystemBeforeDelete(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	var syncedHost string
	hostDaemon.syncGuestFilesystem = func(_ context.Context, runtimeHost string) error {
		syncedHost = runtimeHost
		return nil
	}
	var shutdownHost string
	hostDaemon.shutdownGuest = func(_ context.Context, runtimeHost string) error {
		shutdownHost = runtimeHost
		// Simulate the VM exiting after poweroff by making Inspect return stopped.
		runtime.inspectOverride = func(state firecracker.MachineState) (*firecracker.MachineState, error) {
			state.Phase = firecracker.PhaseStopped
			state.PID = 0
			return &state, nil
		}
		return nil
	}

	now := time.Now().UTC()
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-stop",
		SystemVolumeID: "vm-stop-system",
		RuntimeHost:    "172.16.0.2",
		TapDevice:      "fctap-stop",
		Phase:          contracthost.MachinePhaseRunning,
		PID:            1234,
		SocketPath:     filepath.Join(root, "runtime", "vm-stop.sock"),
		Ports:          defaultMachinePorts(),
		CreatedAt:      now,
		StartedAt:      &now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if err := hostDaemon.StopMachine(context.Background(), "vm-stop"); err != nil {
		t.Fatalf("stop machine: %v", err)
	}

	if shutdownHost != "172.16.0.2" {
		t.Fatalf("shutdown host mismatch: got %q want %q", shutdownHost, "172.16.0.2")
	}
	if syncedHost != "172.16.0.2" {
		t.Fatalf("sync host mismatch: got %q want %q", syncedHost, "172.16.0.2")
	}
	// runtime.Delete is always called to clean up TAP device and runtime dir.
	if len(runtime.deleteCalls) != 1 {
		t.Fatalf("runtime delete call count mismatch: got %d want 1", len(runtime.deleteCalls))
	}

	stopped, err := fileStore.GetMachine(context.Background(), "vm-stop")
	if err != nil {
		t.Fatalf("get stopped machine: %v", err)
	}
	if stopped.Phase != contracthost.MachinePhaseStopped {
		t.Fatalf("machine phase mismatch: got %q want %q", stopped.Phase, contracthost.MachinePhaseStopped)
	}
	if stopped.RuntimeHost != "" {
		t.Fatalf("runtime host should be cleared after stop, got %q", stopped.RuntimeHost)
	}
}

func TestResizeMachineStopsAndExpandsSystemVolume(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.syncGuestFilesystem = func(context.Context, string) error {
		return nil
	}
	hostDaemon.shutdownGuest = func(context.Context, string) error {
		runtime.inspectOverride = func(state firecracker.MachineState) (*firecracker.MachineState, error) {
			state.Phase = firecracker.PhaseStopped
			state.PID = 0
			return &state, nil
		}
		return nil
	}

	systemVolumePath := filepath.Join(root, "machine-disks", "vm-resize", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(systemVolumePath), 0o755); err != nil {
		t.Fatalf("create system volume dir: %v", err)
	}
	currentStorageBytes := int64(5 << 20)
	nextStorageBytes := int64(8 << 20)
	if err := os.WriteFile(systemVolumePath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write system volume: %v", err)
	}
	if err := os.Truncate(systemVolumePath, currentStorageBytes); err != nil {
		t.Fatalf("size system volume: %v", err)
	}

	now := time.Now().UTC()
	if err := fileStore.CreateVolume(context.Background(), model.VolumeRecord{
		ID:        "vm-resize-system",
		Kind:      contracthost.VolumeKindSystem,
		Pool:      model.StoragePoolMachineDisks,
		Path:      systemVolumePath,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create system volume record: %v", err)
	}
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-resize",
		MemoryMiB:      2048,
		StorageBytes:   currentStorageBytes,
		SystemVolumeID: "vm-resize-system",
		RuntimeHost:    "172.16.0.3",
		TapDevice:      "fctap-resize",
		Phase:          contracthost.MachinePhaseRunning,
		PID:            2345,
		SocketPath:     filepath.Join(root, "runtime", "vm-resize.sock"),
		Ports:          defaultMachinePorts(),
		CreatedAt:      now,
		StartedAt:      &now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	response, err := hostDaemon.ResizeMachine(context.Background(), "vm-resize", contracthost.ResizeMachineRequest{
		MemoryMiB:    4096,
		StorageBytes: nextStorageBytes,
	})
	if err != nil {
		t.Fatalf("resize machine: %v", err)
	}
	if response.Machine.Phase != contracthost.MachinePhaseStopped {
		t.Fatalf("response phase = %q, want stopped", response.Machine.Phase)
	}
	if len(runtime.deleteCalls) != 1 {
		t.Fatalf("runtime delete call count = %d, want 1", len(runtime.deleteCalls))
	}

	resized, err := fileStore.GetMachine(context.Background(), "vm-resize")
	if err != nil {
		t.Fatalf("get resized machine: %v", err)
	}
	if resized.MemoryMiB != 4096 {
		t.Fatalf("memory = %d, want 4096", resized.MemoryMiB)
	}
	if resized.StorageBytes != nextStorageBytes {
		t.Fatalf("storage = %d, want %d", resized.StorageBytes, nextStorageBytes)
	}
	if resized.RuntimeHost != "" {
		t.Fatalf("runtime host should be cleared after resize, got %q", resized.RuntimeHost)
	}
	info, err := os.Stat(systemVolumePath)
	if err != nil {
		t.Fatalf("stat system volume: %v", err)
	}
	if info.Size() != nextStorageBytes {
		t.Fatalf("system volume size = %d, want %d", info.Size(), nextStorageBytes)
	}
	operations, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operations left behind: %#v", operations)
	}
}

func TestResizeMachineOmittedMemoryPreservesCurrentMemory(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	systemVolumePath := filepath.Join(root, "machine-disks", "vm-resize-storage-only", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(systemVolumePath), 0o755); err != nil {
		t.Fatalf("create system volume dir: %v", err)
	}
	currentStorageBytes := int64(5 << 20)
	nextStorageBytes := int64(8 << 20)
	if err := os.WriteFile(systemVolumePath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write system volume: %v", err)
	}
	if err := os.Truncate(systemVolumePath, currentStorageBytes); err != nil {
		t.Fatalf("size system volume: %v", err)
	}

	now := time.Now().UTC()
	if err := fileStore.CreateVolume(context.Background(), model.VolumeRecord{
		ID:        "vm-resize-storage-only-system",
		Kind:      contracthost.VolumeKindSystem,
		Pool:      model.StoragePoolMachineDisks,
		Path:      systemVolumePath,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create system volume record: %v", err)
	}
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-resize-storage-only",
		MemoryMiB:      8192,
		StorageBytes:   currentStorageBytes,
		SystemVolumeID: "vm-resize-storage-only-system",
		Phase:          contracthost.MachinePhaseStopped,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if _, err := hostDaemon.ResizeMachine(context.Background(), "vm-resize-storage-only", contracthost.ResizeMachineRequest{
		StorageBytes: nextStorageBytes,
	}); err != nil {
		t.Fatalf("resize machine: %v", err)
	}

	resized, err := fileStore.GetMachine(context.Background(), "vm-resize-storage-only")
	if err != nil {
		t.Fatalf("get resized machine: %v", err)
	}
	if resized.MemoryMiB != 8192 {
		t.Fatalf("memory = %d, want 8192", resized.MemoryMiB)
	}
	if resized.StorageBytes != nextStorageBytes {
		t.Fatalf("storage = %d, want %d", resized.StorageBytes, nextStorageBytes)
	}
}

func TestResizeMachineAlreadyAtRequestedSizeDoesNotStopRunningMachine(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	now := time.Now().UTC()
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:           "vm-resize-idempotent",
		MemoryMiB:    4096,
		StorageBytes: 8 << 20,
		RuntimeHost:  "172.16.0.8",
		TapDevice:    "fctap-idem",
		Phase:        contracthost.MachinePhaseRunning,
		PID:          3456,
		SocketPath:   filepath.Join(root, "runtime", "vm-resize-idempotent.sock"),
		Ports:        defaultMachinePorts(),
		CreatedAt:    now,
		StartedAt:    &now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	response, err := hostDaemon.ResizeMachine(context.Background(), "vm-resize-idempotent", contracthost.ResizeMachineRequest{
		MemoryMiB:    4096,
		StorageBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("resize machine: %v", err)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("response phase = %q, want running", response.Machine.Phase)
	}
	if len(runtime.deleteCalls) != 0 {
		t.Fatalf("runtime delete call count = %d, want 0", len(runtime.deleteCalls))
	}
	operations, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operations left behind: %#v", operations)
	}
}

func TestGetMachineReconcilesStartingMachineBeforeRunning(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000100, 0).UTC()
	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	t.Cleanup(func() { hostDaemon.stopMachineRelays("vm-starting") })

	personalized := false
	hostDaemon.personalizeGuest = func(_ context.Context, record *model.MachineRecord, state firecracker.MachineState) (*guestReadyResult, error) {
		personalized = true
		if record.ID != "vm-starting" {
			t.Fatalf("personalized machine mismatch: got %q", record.ID)
		}
		if state.RuntimeHost != "127.0.0.1" || state.PID != 4321 {
			t.Fatalf("personalized state mismatch: %#v", state)
		}
		guestSSHPublicKey := strings.TrimSpace(record.GuestSSHPublicKey)
		if guestSSHPublicKey == "" {
			guestSSHPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO0j1AyW0mQm9a1G2rY0R4fP2G5+4Qx2V3FJ9P2mA6N3"
		}
		return &guestReadyResult{
			ReadyNonce:        record.GuestReadyNonce,
			GuestSSHPublicKey: guestSSHPublicKey,
		}, nil
	}

	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-starting",
		SystemVolumeID: "vm-starting-system",
		RuntimeHost:    "127.0.0.1",
		TapDevice:      "fctap-starting",
		Ports:          defaultMachinePorts(),
		Phase:          contracthost.MachinePhaseStarting,
		PID:            4321,
		SocketPath:     filepath.Join(cfg.RuntimeDir, "machines", "vm-starting", "root", "run", "firecracker.sock"),
		CreatedAt:      time.Now().UTC(),
		StartedAt:      &startedAt,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	response, err := hostDaemon.GetMachine(context.Background(), "vm-starting")
	if err != nil {
		t.Fatalf("GetMachine returned error: %v", err)
	}
	if !personalized {
		t.Fatalf("guest personalization was not called")
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("machine phase = %q, want %q", response.Machine.Phase, contracthost.MachinePhaseRunning)
	}
	if response.Machine.GuestSSHPublicKey == "" {
		t.Fatalf("guest ssh public key should be recorded after convergence")
	}
}

func TestListMachinesDoesNotReconcileStartingMachines(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.personalizeGuest = func(context.Context, *model.MachineRecord, firecracker.MachineState) (*guestReadyResult, error) {
		t.Fatalf("ListMachines should not reconcile guest personalization")
		return nil, nil
	}
	hostDaemon.readGuestSSHPublicKey = func(context.Context, string) (string, error) {
		t.Fatalf("ListMachines should not read guest ssh public key")
		return "", nil
	}

	now := time.Now().UTC()
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-list",
		SystemVolumeID: "vm-list-system",
		RuntimeHost:    "127.0.0.1",
		TapDevice:      "fctap-list",
		Ports:          defaultMachinePorts(),
		Phase:          contracthost.MachinePhaseStarting,
		PID:            4321,
		SocketPath:     filepath.Join(cfg.RuntimeDir, "machines", "vm-list", "root", "run", "firecracker.sock"),
		CreatedAt:      now,
		StartedAt:      &now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	response, err := hostDaemon.ListMachines(context.Background())
	if err != nil {
		t.Fatalf("ListMachines returned error: %v", err)
	}
	if len(response.Machines) != 1 {
		t.Fatalf("machine count = %d, want 1", len(response.Machines))
	}
	if response.Machines[0].Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("machine phase = %q, want %q", response.Machines[0].Phase, contracthost.MachinePhaseStarting)
	}
}

func TestReconcileStartingMachineFailsWhenHandshakeFails(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000201, 0).UTC()
	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.personalizeGuest = func(context.Context, *model.MachineRecord, firecracker.MachineState) (*guestReadyResult, error) {
		return nil, errors.New("vsock EOF")
	}

	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:                "vm-best-effort",
		SystemVolumeID:    "vm-best-effort-system",
		RuntimeHost:       "127.0.0.1",
		TapDevice:         "fctap-best-effort",
		Ports:             defaultMachinePorts(),
		GuestSSHPublicKey: "ssh-ed25519 AAAAExistingHostKey",
		Phase:             contracthost.MachinePhaseStarting,
		PID:               4322,
		SocketPath:        filepath.Join(cfg.RuntimeDir, "machines", "vm-best-effort", "root", "run", "firecracker.sock"),
		CreatedAt:         time.Now().UTC(),
		StartedAt:         &startedAt,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if err := hostDaemon.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	record, err := fileStore.GetMachine(context.Background(), "vm-best-effort")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if record.Phase != contracthost.MachinePhaseFailed {
		t.Fatalf("machine phase = %q, want %q", record.Phase, contracthost.MachinePhaseFailed)
	}
	if !strings.Contains(record.Error, "vsock EOF") {
		t.Fatalf("failure reason = %q, want vsock error", record.Error)
	}
	if len(runtime.deleteCalls) != 1 {
		t.Fatalf("runtime delete calls = %d, want 1", len(runtime.deleteCalls))
	}
}

func TestShutdownGuestCleanChecksGuestStateAfterPoweroffTimeout(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{
		inspectOverride: func(state firecracker.MachineState) (*firecracker.MachineState, error) {
			state.Phase = firecracker.PhaseStopped
			state.PID = 0
			return &state, nil
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.shutdownGuest = func(context.Context, string) error {
		return context.DeadlineExceeded
	}

	now := time.Now().UTC()
	record := &model.MachineRecord{
		ID:          "vm-timeout",
		RuntimeHost: "172.16.0.2",
		TapDevice:   "fctap-timeout",
		Phase:       contracthost.MachinePhaseRunning,
		PID:         1234,
		SocketPath:  filepath.Join(root, "runtime", "vm-timeout.sock"),
		Ports:       defaultMachinePorts(),
		CreatedAt:   now,
		StartedAt:   &now,
	}

	if ok := hostDaemon.shutdownGuestClean(context.Background(), record); !ok {
		t.Fatal("shutdownGuestClean should treat a timed-out poweroff as success when the VM is already stopped")
	}
}

func TestShutdownGuestCleanRespectsContextCancellation(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{
		inspectOverride: func(state firecracker.MachineState) (*firecracker.MachineState, error) {
			state.Phase = firecracker.PhaseRunning
			return &state, nil
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	hostDaemon.shutdownGuest = func(context.Context, string) error { return nil }

	now := time.Now().UTC()
	record := &model.MachineRecord{
		ID:          "vm-cancel",
		RuntimeHost: "172.16.0.2",
		TapDevice:   "fctap-cancel",
		Phase:       contracthost.MachinePhaseRunning,
		PID:         1234,
		SocketPath:  filepath.Join(root, "runtime", "vm-cancel.sock"),
		Ports:       defaultMachinePorts(),
		CreatedAt:   now,
		StartedAt:   &now,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if ok := hostDaemon.shutdownGuestClean(ctx, record); ok {
		t.Fatal("shutdownGuestClean should not report a clean shutdown after cancellation")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdownGuestClean took %v after cancellation, want fast return", elapsed)
	}
}

func TestNewEnsuresBackendSSHKeyPair(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	if _, err := os.Stat(hostDaemon.backendSSHPrivateKeyPath()); err != nil {
		t.Fatalf("stat backend ssh private key: %v", err)
	}
	publicKeyPayload, err := os.ReadFile(hostDaemon.backendSSHPublicKeyPath())
	if err != nil {
		t.Fatalf("read backend ssh public key: %v", err)
	}
	if !strings.HasPrefix(string(publicKeyPayload), "ssh-ed25519 ") {
		t.Fatalf("unexpected backend ssh public key: %q", string(publicKeyPayload))
	}
}

func TestRestoreSnapshotFallsBackToLocalSnapshotNetwork(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000099, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "restored",
			Phase:       firecracker.PhaseRunning,
			PID:         1234,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored", "root", "run", "firecracker.sock"),
			TapName:     "fctap0",
			StartedAt:   &startedAt,
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(context.Context, string, contracthost.MachineID, *contracthost.GuestConfig) error { return nil }

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": buildTestExt4ImageBytes(t),
	})
	defer server.Close()

	artifactRef := contracthost.ArtifactRef{
		KernelImageURL: server.URL + "/kernel",
		RootFSURL:      server.URL + "/rootfs",
	}
	kernelPath := filepath.Join(root, "artifact-kernel")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	if err := fileStore.PutArtifact(context.Background(), model.ArtifactRecord{
		Ref:             artifactRef,
		LocalKey:        "artifact",
		LocalDir:        filepath.Join(root, "artifact"),
		KernelImagePath: kernelPath,
		RootFSPath:      filepath.Join(root, "artifact-rootfs"),
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:                "snap1",
		MachineID:         "source",
		Artifact:          artifactRef,
		DiskPaths:         []string{filepath.Join(root, "snapshots", "snap1", "system.img")},
		SourceRuntimeHost: "172.16.0.2",
		SourceTapDevice:   "fctap0",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		MemoryMiB:    4096,
		StorageBytes: int64(12 * 1024 * 1024 * 1024),
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID: "snap1",
			MachineID:  "source",
			ImageID:    "image-1",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system"},
			},
		},
		GuestConfig: &contracthost.GuestConfig{Hostname: "restored-shell"},
	})
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if response.Machine.ID != "restored" {
		t.Fatalf("restored machine id mismatch: got %q", response.Machine.ID)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("restored machine phase mismatch: got %q", response.Machine.Phase)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.lastSpec.MemoryMiB != 4096 {
		t.Fatalf("restore runtime memory mismatch: got %d want 4096", runtime.lastSpec.MemoryMiB)
	}
	if runtime.restoreCalls != 0 {
		t.Fatalf("restore boot call count mismatch: got %d want 0", runtime.restoreCalls)
	}

	ops, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("operation journal should be empty after successful restore: got %d entries", len(ops))
	}
}

func TestRestoreSnapshotUsesLocalSnapshotArtifacts(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000199, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "restored-local",
			Phase:       firecracker.PhaseRunning,
			PID:         1234,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored-local", "root", "run", "firecracker.sock"),
			TapName:     "fctap0",
			StartedAt:   &startedAt,
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(context.Context, string, contracthost.MachineID, *contracthost.GuestConfig) error { return nil }

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
	})
	defer server.Close()

	artifactRef := contracthost.ArtifactRef{
		KernelImageURL: server.URL + "/kernel",
		RootFSURL:      server.URL + "/rootfs",
	}
	artifactDir := filepath.Join(root, "artifact")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("create artifact dir: %v", err)
	}
	kernelPath := filepath.Join(artifactDir, "vmlinux")
	rootFSPath := filepath.Join(artifactDir, "rootfs.ext4")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	if err := os.WriteFile(rootFSPath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	if err := fileStore.PutArtifact(context.Background(), model.ArtifactRecord{
		Ref:             artifactRef,
		LocalKey:        "artifact",
		LocalDir:        artifactDir,
		KernelImagePath: kernelPath,
		RootFSPath:      rootFSPath,
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	snapshotDir := filepath.Join(root, "snapshots", "snap-local")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	systemPath := filepath.Join(snapshotDir, "system.img")
	if err := os.WriteFile(systemPath, buildTestExt4ImageBytes(t), 0o644); err != nil {
		t.Fatalf("write system disk: %v", err)
	}
	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:        "snap-local",
		MachineID: "source",
		Artifact:  artifactRef,
		DiskPaths: []string{systemPath},
		Artifacts: []model.SnapshotArtifactRecord{
			{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", LocalPath: systemPath, SizeBytes: 4},
		},
		SourceRuntimeHost: "172.16.0.2",
		SourceTapDevice:   "fctap0",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap-local", contracthost.RestoreSnapshotRequest{
		MachineID: "restored-local",
		Artifact:  artifactRef,
		LocalSnapshot: &contracthost.LocalSnapshotSpec{
			SnapshotID: "snap-local",
		},
		GuestConfig: &contracthost.GuestConfig{Hostname: "restored-local-shell"},
	})
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if response.Machine.ID != "restored-local" {
		t.Fatalf("restored machine id mismatch: got %q", response.Machine.ID)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.restoreCalls != 0 {
		t.Fatalf("restore boot call count mismatch: got %d want 0", runtime.restoreCalls)
	}
	machineName, err := readExt4File(runtime.lastSpec.RootFSPath, "/etc/microagent/machine-name")
	if err != nil {
		t.Fatalf("read restored machine-name: %v", err)
	}
	if machineName != "agentcomputer\n" {
		t.Fatalf("restored machine-name mismatch: got %q want %q", machineName, "agentcomputer\n")
	}
}

func TestGetSnapshotArtifactReturnsLocalArtifactPath(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	snapshotDir := filepath.Join(root, "snapshots", "snap-artifact")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	memoryPath := filepath.Join(snapshotDir, "memory.bin")
	if err := os.WriteFile(memoryPath, []byte("mem"), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:            "snap-artifact",
		MachineID:     "source",
		MemFilePath:   memoryPath,
		StateFilePath: filepath.Join(snapshotDir, "vmstate.bin"),
		Artifacts: []model.SnapshotArtifactRecord{
			{ID: "memory", Kind: contracthost.SnapshotArtifactKindMemory, Name: "memory.bin", LocalPath: memoryPath, SizeBytes: 3},
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	artifact, err := hostDaemon.GetSnapshotArtifact(context.Background(), "snap-artifact", "memory")
	if err != nil {
		t.Fatalf("GetSnapshotArtifact returned error: %v", err)
	}
	if artifact == nil {
		t.Fatalf("GetSnapshotArtifact returned nil artifact")
	}
	if artifact.Name != "memory.bin" {
		t.Fatalf("artifact name = %q, want memory.bin", artifact.Name)
	}
	if artifact.Path != memoryPath {
		t.Fatalf("artifact path = %q, want %q", artifact.Path, memoryPath)
	}
}

func TestDeleteSnapshotByIDRemovesDiskOnlySnapshotDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	snapshotDir := filepath.Join(root, "snapshots", "snap-delete")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	systemPath := filepath.Join(snapshotDir, "system.img")
	if err := os.WriteFile(systemPath, []byte("disk"), 0o644); err != nil {
		t.Fatalf("write system disk: %v", err)
	}
	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:        "snap-delete",
		MachineID: "source",
		DiskPaths: []string{systemPath},
		Artifacts: []model.SnapshotArtifactRecord{
			{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", LocalPath: systemPath, SizeBytes: 4},
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if err := hostDaemon.DeleteSnapshotByID(context.Background(), "snap-delete"); err != nil {
		t.Fatalf("DeleteSnapshotByID returned error: %v", err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir should be removed, stat error: %v", err)
	}
	if _, err := fileStore.GetSnapshot(context.Background(), "snap-delete"); err != store.ErrNotFound {
		t.Fatalf("snapshot should be removed from store, got: %v", err)
	}
}

func TestRestoreSnapshotUsesDurableSnapshotSpec(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() {
		_ = sshListener.Close()
	}()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() {
		_ = vncListener.Close()
	}()

	startedAt := time.Unix(1700000099, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "restored",
			Phase:       firecracker.PhaseRunning,
			PID:         1234,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored", "root", "run", "firecracker.sock"),
			TapName:     "fctap0",
			StartedAt:   &startedAt,
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(_ context.Context, host string, machineID contracthost.MachineID, guestConfig *contracthost.GuestConfig) error {
		t.Fatalf("restore snapshot should not synchronously reconfigure guest identity, host=%q machine=%q guest_config=%#v", host, machineID, guestConfig)
		return nil
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": buildTestExt4ImageBytes(t),
		"/user-0": []byte("user-disk"),
	})
	defer server.Close()

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		MemoryMiB:    4096,
		StorageBytes: int64(12 * 1024 * 1024 * 1024),
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap1",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system"},
				{ID: "disk-user-0", Kind: contracthost.SnapshotArtifactKindDisk, Name: "user-0.img", DownloadURL: server.URL + "/user-0"},
			},
		},
		GuestConfig: &contracthost.GuestConfig{Hostname: "restored-shell"},
	})
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if response.Machine.ID != "restored" {
		t.Fatalf("restored machine id mismatch: got %q", response.Machine.ID)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("restored machine phase mismatch: got %q", response.Machine.Phase)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.restoreCalls != 0 {
		t.Fatalf("restore boot call count mismatch: got %d want 0", runtime.restoreCalls)
	}
	if !strings.Contains(runtime.lastSpec.KernelImagePath, filepath.Join("artifacts", artifactKey(contracthost.ArtifactRef{
		KernelImageURL: server.URL + "/kernel",
		RootFSURL:      server.URL + "/rootfs",
	}), "kernel")) {
		t.Fatalf("restore boot kernel path mismatch: got %q", runtime.lastSpec.KernelImagePath)
	}

	machine, err := fileStore.GetMachine(context.Background(), "restored")
	if err != nil {
		t.Fatalf("get restored machine: %v", err)
	}
	if machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("restored machine phase mismatch: got %q", machine.Phase)
	}
	if machine.GuestConfig == nil || machine.GuestConfig.Hostname != "restored-shell" {
		t.Fatalf("stored guest config mismatch: %#v", machine.GuestConfig)
	}
	if machine.MemoryMiB != 4096 {
		t.Fatalf("stored restored memory mismatch: got %d want 4096", machine.MemoryMiB)
	}
	if machine.StorageBytes != 12*1024*1024*1024 {
		t.Fatalf("stored restored storage mismatch: got %d want %d", machine.StorageBytes, int64(12*1024*1024*1024))
	}
	if len(machine.UserVolumeIDs) != 1 {
		t.Fatalf("restored machine user volumes mismatch: got %#v", machine.UserVolumeIDs)
	}
	if _, err := os.Stat(filepath.Join(cfg.MachineDisksDir, "restored", "user-0.img")); err != nil {
		t.Fatalf("restored user disk missing: %v", err)
	}

	ops, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("operation journal should be empty after successful restore: got %d entries", len(ops))
	}
}

func TestRestoreSnapshotBootsWithFreshNetworkWhenSourceNetworkInUseOnHost(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(context.Context, string, contracthost.MachineID, *contracthost.GuestConfig) error { return nil }

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000299, 0).UTC()
	runtime.bootState = firecracker.MachineState{
		ID:          "restored",
		Phase:       firecracker.PhaseRunning,
		PID:         1234,
		RuntimeHost: "127.0.0.1",
		SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored", "root", "run", "firecracker.sock"),
		TapName:     "fctap9",
		StartedAt:   &startedAt,
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": buildTestExt4ImageBytes(t),
	})
	defer server.Close()

	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "source",
		Artifact:       contracthost.ArtifactRef{KernelImageURL: "https://example.com/kernel", RootFSURL: "https://example.com/rootfs"},
		SystemVolumeID: "source-system",
		RuntimeHost:    "172.16.0.2",
		TapDevice:      "fctap0",
		Phase:          contracthost.MachinePhaseRunning,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create running source machine: %v", err)
	}

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap1",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system"},
			},
		},
	})
	if err != nil {
		t.Fatalf("restore snapshot error = %v, want success", err)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("restored machine phase mismatch: got %q", response.Machine.Phase)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.restoreCalls != 0 {
		t.Fatalf("restore boot should not be attempted, got %d calls", runtime.restoreCalls)
	}
}

func newRestoreArtifactServer(t *testing.T, payloads map[string][]byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
}

func TestCreateMachineRejectsNonHTTPArtifactURLs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	_, err = hostDaemon.CreateMachine(context.Background(), contracthost.CreateMachineRequest{
		MachineID: "vm-1",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: "file:///kernel",
			RootFSURL:      "https://example.com/rootfs",
		},
	})
	if err == nil {
		t.Fatal("expected create machine to fail for non-http artifact url")
	}
	if got := err.Error(); got != "artifact.kernel_image_url must use http or https" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestDeleteMachineMissingIsNoOp(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	if err := hostDaemon.DeleteMachine(context.Background(), "missing"); err != nil {
		t.Fatalf("delete missing machine: %v", err)
	}
	if len(runtime.deleteCalls) != 0 {
		t.Fatalf("delete runtime should not be called for missing machine")
	}
}

func testConfig(root string) appconfig.Config {
	return appconfig.Config{
		RootDir:               root,
		StatePath:             filepath.Join(root, "state", "state.json"),
		OperationsPath:        filepath.Join(root, "state", "ops.json"),
		ArtifactsDir:          filepath.Join(root, "artifacts"),
		MachineDisksDir:       filepath.Join(root, "machine-disks"),
		SnapshotsDir:          filepath.Join(root, "snapshots"),
		RuntimeDir:            filepath.Join(root, "runtime"),
		DiskCloneMode:         appconfig.DiskCloneModeCopy,
		DriveIOEngine:         firecracker.DriveIOEngineSync,
		SocketPath:            filepath.Join(root, "firecracker-host.sock"),
		EgressInterface:       "eth0",
		ReconcileInterval:     time.Second,
		FirecrackerBinaryPath: "/usr/bin/firecracker",
		JailerBinaryPath:      "/usr/bin/jailer",
	}
}

func buildTestExt4ImageBytes(t *testing.T) []byte {
	t.Helper()

	root := t.TempDir()
	imagePath := filepath.Join(root, "rootfs.ext4")
	if err := buildTestExt4Image(root, imagePath); err != nil {
		t.Fatalf("build ext4 image: %v", err)
	}
	payload, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("read ext4 image: %v", err)
	}
	return payload
}

func TestGuestKernelArgsDisablesPCIByDefault(t *testing.T) {
	t.Parallel()

	if got := guestKernelArgs(false); !strings.Contains(got, "pci=off") {
		t.Fatalf("guestKernelArgs(false) = %q, want pci=off", got)
	}
}

func TestGuestKernelArgsRemovesPCIOffWhenPCIEnabled(t *testing.T) {
	t.Parallel()

	if got := guestKernelArgs(true); strings.Contains(got, "pci=off") {
		t.Fatalf("guestKernelArgs(true) = %q, want no pci=off", got)
	}
}

func stubGuestSSHPublicKeyReader(hostDaemon *Daemon) {
	hostDaemon.personalizeGuest = func(_ context.Context, record *model.MachineRecord, _ firecracker.MachineState) (*guestReadyResult, error) {
		guestSSHPublicKey := strings.TrimSpace(record.GuestSSHPublicKey)
		if guestSSHPublicKey == "" {
			guestSSHPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO0j1AyW0mQm9a1G2rY0R4fP2G5+4Qx2V3FJ9P2mA6N3"
		}
		return &guestReadyResult{
			ReadyNonce:        record.GuestReadyNonce,
			GuestSSHPublicKey: guestSSHPublicKey,
		}, nil
	}
}

func listenTestPort(t *testing.T, port int) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		var bindErr *net.OpError
		if errors.As(err, &bindErr) && strings.Contains(strings.ToLower(err.Error()), "address already in use") {
			t.Skipf("port %d already in use", port)
		}
		t.Fatalf("listen on port %d: %v", port, err)
	}

	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	return listener
}
