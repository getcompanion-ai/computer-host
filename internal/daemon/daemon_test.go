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
	bootState    firecracker.MachineState
	bootCalls    int
	restoreCalls int
	deleteCalls  []firecracker.MachineState
	lastSpec     firecracker.MachineSpec
	lastLoadSpec firecracker.SnapshotLoadSpec
	mmdsWrites   []any
}

func (f *fakeRuntime) Boot(_ context.Context, spec firecracker.MachineSpec, _ []firecracker.NetworkAllocation) (*firecracker.MachineState, error) {
	f.bootCalls++
	f.lastSpec = spec
	state := f.bootState
	return &state, nil
}

func (f *fakeRuntime) Inspect(state firecracker.MachineState) (*firecracker.MachineState, error) {
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

	if response.Machine.Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("machine phase mismatch: got %q", response.Machine.Phase)
	}
	if response.Machine.RuntimeHost != "127.0.0.1" {
		t.Fatalf("runtime host mismatch: got %q", response.Machine.RuntimeHost)
	}
	if len(response.Machine.Ports) != 2 {
		t.Fatalf("machine ports mismatch: got %d want 2", len(response.Machine.Ports))
	}
	if response.Machine.Ports[0].Port != defaultSSHPort || response.Machine.Ports[1].Port != defaultVNCPort {
		t.Fatalf("machine ports mismatch: got %#v", response.Machine.Ports)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.lastSpec.KernelImagePath == "" || runtime.lastSpec.RootFSPath == "" {
		t.Fatalf("runtime spec paths not populated: %#v", runtime.lastSpec)
	}
	if _, err := os.Stat(runtime.lastSpec.KernelImagePath); err != nil {
		t.Fatalf("kernel artifact not staged: %v", err)
	}
	if _, err := os.Stat(runtime.lastSpec.RootFSPath); err != nil {
		t.Fatalf("system disk not staged: %v", err)
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
	if machine.Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("stored machine phase mismatch: got %q", machine.Phase)
	}
	if machine.GuestConfig == nil || len(machine.GuestConfig.AuthorizedKeys) == 0 {
		t.Fatalf("stored guest config missing authorized keys: %#v", machine.GuestConfig)
	}

	operations, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operation journal should be empty after success: got %d entries", len(operations))
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

	if syncedHost != "172.16.0.2" {
		t.Fatalf("sync host mismatch: got %q want %q", syncedHost, "172.16.0.2")
	}
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

func TestReconcileStartingMachinePersonalizesBeforeRunning(t *testing.T) {
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
	hostDaemon.personalizeGuest = func(_ context.Context, record *model.MachineRecord, state firecracker.MachineState) error {
		personalized = true
		if record.ID != "vm-starting" {
			t.Fatalf("personalized machine mismatch: got %q", record.ID)
		}
		if state.RuntimeHost != "127.0.0.1" || state.PID != 4321 {
			t.Fatalf("personalized state mismatch: %#v", state)
		}
		return nil
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

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

	artifactRef := contracthost.ArtifactRef{KernelImageURL: "kernel", RootFSURL: "rootfs"}
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
		MemFilePath:       filepath.Join(root, "snapshots", "snap1", "memory.bin"),
		StateFilePath:     filepath.Join(root, "snapshots", "snap1", "vmstate.bin"),
		DiskPaths:         []string{filepath.Join(root, "snapshots", "snap1", "system.img")},
		SourceRuntimeHost: "172.16.0.2",
		SourceTapDevice:   "fctap0",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel":  []byte("kernel"),
		"/rootfs":  []byte("rootfs"),
		"/memory":  []byte("mem"),
		"/vmstate": []byte("state"),
		"/system":  []byte("disk"),
	})
	defer server.Close()

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: contracthost.DurableSnapshotSpec{
			SnapshotID: "snap1",
			MachineID:  "source",
			ImageID:    "image-1",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "memory", Kind: contracthost.SnapshotArtifactKindMemory, Name: "memory.bin", DownloadURL: server.URL + "/memory"},
				{ID: "vmstate", Kind: contracthost.SnapshotArtifactKindVMState, Name: "vmstate.bin", DownloadURL: server.URL + "/vmstate"},
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
	if response.Machine.Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("restored machine phase mismatch: got %q", response.Machine.Phase)
	}
	if runtime.restoreCalls != 1 {
		t.Fatalf("restore boot call count mismatch: got %d want 1", runtime.restoreCalls)
	}
	if runtime.lastLoadSpec.Network == nil {
		t.Fatalf("restore boot should preserve snapshot network")
	}
	if got := runtime.lastLoadSpec.Network.GuestIP().String(); got != "172.16.0.2" {
		t.Fatalf("restore guest ip mismatch: got %q want %q", got, "172.16.0.2")
	}
	if got := runtime.lastLoadSpec.Network.TapName; got != "fctap0" {
		t.Fatalf("restore tap mismatch: got %q want %q", got, "fctap0")
	}

	ops, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("operation journal should be empty after successful restore: got %d entries", len(ops))
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
		"/kernel":  []byte("kernel"),
		"/rootfs":  []byte("rootfs"),
		"/memory":  []byte("mem"),
		"/vmstate": []byte("state"),
		"/system":  []byte("disk"),
		"/user-0":  []byte("user-disk"),
	})
	defer server.Close()

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap1",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "memory", Kind: contracthost.SnapshotArtifactKindMemory, Name: "memory.bin", DownloadURL: server.URL + "/memory"},
				{ID: "vmstate", Kind: contracthost.SnapshotArtifactKindVMState, Name: "vmstate.bin", DownloadURL: server.URL + "/vmstate"},
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
	if response.Machine.Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("restored machine phase mismatch: got %q", response.Machine.Phase)
	}
	if runtime.restoreCalls != 1 {
		t.Fatalf("restore boot call count mismatch: got %d want 1", runtime.restoreCalls)
	}
	if runtime.lastLoadSpec.Network == nil {
		t.Fatalf("restore boot should preserve durable snapshot network")
	}
	if got := runtime.lastLoadSpec.Network.GuestIP().String(); got != "172.16.0.2" {
		t.Fatalf("restore guest ip mismatch: got %q want %q", got, "172.16.0.2")
	}
	if got := runtime.lastLoadSpec.Network.TapName; got != "fctap0" {
		t.Fatalf("restore tap mismatch: got %q want %q", got, "fctap0")
	}
	if !strings.Contains(runtime.lastLoadSpec.KernelImagePath, filepath.Join("artifacts", artifactKey(contracthost.ArtifactRef{
		KernelImageURL: server.URL + "/kernel",
		RootFSURL:      server.URL + "/rootfs",
	}), "kernel")) {
		t.Fatalf("restore boot kernel path mismatch: got %q", runtime.lastLoadSpec.KernelImagePath)
	}

	machine, err := fileStore.GetMachine(context.Background(), "restored")
	if err != nil {
		t.Fatalf("get restored machine: %v", err)
	}
	if machine.Phase != contracthost.MachinePhaseStarting {
		t.Fatalf("restored machine phase mismatch: got %q", machine.Phase)
	}
	if machine.GuestConfig == nil || machine.GuestConfig.Hostname != "restored-shell" {
		t.Fatalf("stored guest config mismatch: %#v", machine.GuestConfig)
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

func TestRestoreSnapshotRejectsWhenRestoreNetworkInUseOnHost(t *testing.T) {
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

	_, err = hostDaemon.RestoreSnapshot(context.Background(), "snap1", contracthost.RestoreSnapshotRequest{
		MachineID: "restored",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: "https://example.com/kernel",
			RootFSURL:      "https://example.com/rootfs",
		},
		Snapshot: contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap1",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "still in use on this host") {
		t.Fatalf("restore snapshot error = %v, want restore network in-use failure", err)
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
		SocketPath:            filepath.Join(root, "firecracker-host.sock"),
		EgressInterface:       "eth0",
		FirecrackerBinaryPath: "/usr/bin/firecracker",
		JailerBinaryPath:      "/usr/bin/jailer",
	}
}

func stubGuestSSHPublicKeyReader(hostDaemon *Daemon) {
	hostDaemon.readGuestSSHPublicKey = func(context.Context, string) (string, error) {
		return "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO0j1AyW0mQm9a1G2rY0R4fP2G5+4Qx2V3FJ9P2mA6N3", nil
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
