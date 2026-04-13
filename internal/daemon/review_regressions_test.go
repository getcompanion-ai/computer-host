package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	hoststore "github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type blockingPublishedPortStore struct {
	hoststore.Store
	createEntered chan struct{}
	releaseCreate chan struct{}
	firstCreate   sync.Once
}

func (s *blockingPublishedPortStore) CreatePublishedPort(ctx context.Context, record model.PublishedPortRecord) error {
	shouldBlock := false
	s.firstCreate.Do(func() {
		shouldBlock = true
		close(s.createEntered)
	})
	if shouldBlock {
		<-s.releaseCreate
	}
	return s.Store.CreatePublishedPort(ctx, record)
}

type snapshotLookupErrorStore struct {
	hoststore.Store
	err error
}

func (s snapshotLookupErrorStore) GetSnapshot(context.Context, contracthost.SnapshotID) (*model.SnapshotRecord, error) {
	return nil, s.err
}

type machineLookupErrorStore struct {
	hoststore.Store
	err error
}

func (s machineLookupErrorStore) GetMachine(context.Context, contracthost.MachineID) (*model.MachineRecord, error) {
	return nil, s.err
}

type publishedPortResult struct {
	response *contracthost.CreatePublishedPortResponse
	err      error
}

type failingInspectRuntime struct {
	fakeRuntime
}

func (r *failingInspectRuntime) Inspect(state firecracker.MachineState) (*firecracker.MachineState, error) {
	state.Phase = firecracker.PhaseFailed
	state.Error = "vm exited unexpectedly"
	return &state, nil
}

func TestCreatePublishedPortSerializesHostPortAllocationAcrossMachines(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	wrappedStore := &blockingPublishedPortStore{
		Store:         baseStore,
		createEntered: make(chan struct{}),
		releaseCreate: make(chan struct{}),
	}

	hostDaemon, err := New(cfg, wrappedStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	for _, machineID := range []contracthost.MachineID{"vm-1", "vm-2"} {
		if err := baseStore.CreateMachine(context.Background(), model.MachineRecord{
			ID:          machineID,
			RuntimeHost: "127.0.0.1",
			Phase:       contracthost.MachinePhaseRunning,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create machine %q: %v", machineID, err)
		}
	}

	resultCh1 := make(chan publishedPortResult, 1)
	go func() {
		response, err := hostDaemon.CreatePublishedPort(context.Background(), "vm-1", contracthost.CreatePublishedPortRequest{
			PublishedPortID: "port-1",
			Port:            8080,
		})
		resultCh1 <- publishedPortResult{response: response, err: err}
	}()

	<-wrappedStore.createEntered

	resultCh2 := make(chan publishedPortResult, 1)
	go func() {
		response, err := hostDaemon.CreatePublishedPort(context.Background(), "vm-2", contracthost.CreatePublishedPortRequest{
			PublishedPortID: "port-2",
			Port:            9090,
		})
		resultCh2 <- publishedPortResult{response: response, err: err}
	}()

	close(wrappedStore.releaseCreate)

	first := waitPublishedPortResult(t, resultCh1)
	second := waitPublishedPortResult(t, resultCh2)

	if first.err != nil {
		t.Fatalf("first CreatePublishedPort returned error: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second CreatePublishedPort returned error: %v", second.err)
	}
	if first.response.Port.HostPort == second.response.Port.HostPort {
		t.Fatalf("host ports collided: both requests received %d", first.response.Port.HostPort)
	}

	t.Cleanup(func() {
		hostDaemon.stopPublishedPortProxy("port-1")
		hostDaemon.stopPublishedPortProxy("port-2")
	})
}

func TestGetStorageReportHandlesSparseSnapshotPathsAndIncludesPublishedPortPool(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:        "snap-1",
		MachineID: "vm-1",
		DiskPaths: []string{""},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := fileStore.CreatePublishedPort(context.Background(), model.PublishedPortRecord{
		ID:        "port-1",
		MachineID: "vm-1",
		Port:      8080,
		HostPort:  minPublishedHostPort,
		Protocol:  contracthost.PortProtocolTCP,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create published port: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	response, err := hostDaemon.GetStorageReport(context.Background())
	if err != nil {
		t.Fatalf("GetStorageReport returned error: %v", err)
	}
	if response.Report.PublishedPorts != 1 {
		t.Fatalf("published port count = %d, want 1", response.Report.PublishedPorts)
	}
	if len(response.Report.Snapshots) != 1 || response.Report.Snapshots[0].Bytes != 0 {
		t.Fatalf("snapshot usage = %#v, want zero-byte sparse snapshot", response.Report.Snapshots)
	}

	foundPool := false
	for _, pool := range response.Report.Pools {
		if pool.Pool == contracthost.StoragePoolPublishedPort {
			foundPool = true
			if pool.Bytes != 0 {
				t.Fatalf("published port pool bytes = %d, want 0", pool.Bytes)
			}
		}
	}
	if !foundPool {
		t.Fatal("storage report missing published ports pool")
	}
}

func TestReconcileSnapshotPreservesArtifactsOnUnexpectedStoreError(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	lookupErr := errors.New("snapshot backend unavailable")
	hostDaemon, err := New(cfg, snapshotLookupErrorStore{Store: baseStore, err: lookupErr}, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	snapshotID := contracthost.SnapshotID("snap-1")
	operation := model.OperationRecord{
		MachineID:  "vm-1",
		Type:       model.MachineOperationSnapshot,
		StartedAt:  time.Now().UTC(),
		SnapshotID: &snapshotID,
	}
	if err := baseStore.UpsertOperation(context.Background(), operation); err != nil {
		t.Fatalf("upsert operation: %v", err)
	}

	snapshotDir := filepath.Join(cfg.SnapshotsDir, string(snapshotID))
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}

	err = hostDaemon.reconcileSnapshot(context.Background(), operation)
	if err == nil || !strings.Contains(err.Error(), "snapshot backend unavailable") {
		t.Fatalf("reconcileSnapshot error = %v, want wrapped lookup error", err)
	}
	if _, statErr := os.Stat(snapshotDir); statErr != nil {
		t.Fatalf("snapshot dir should be preserved, stat error: %v", statErr)
	}
	assertOperationCount(t, baseStore, 1)
}

func TestReconcileSkipsInFlightSnapshotOperationWhileMachineLocked(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, baseStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	snapshotID := contracthost.SnapshotID("snap-inflight")
	operation := model.OperationRecord{
		MachineID:  "vm-1",
		Type:       model.MachineOperationSnapshot,
		StartedAt:  time.Now().UTC(),
		SnapshotID: &snapshotID,
	}
	if err := baseStore.UpsertOperation(context.Background(), operation); err != nil {
		t.Fatalf("upsert operation: %v", err)
	}

	snapshotDir := filepath.Join(cfg.SnapshotsDir, string(snapshotID))
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	markerPath := filepath.Join(snapshotDir, "keep.txt")
	if err := os.WriteFile(markerPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	unlock := hostDaemon.lockMachine("vm-1")
	defer unlock()

	if err := hostDaemon.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("in-flight snapshot artifacts should be preserved, stat error: %v", statErr)
	}
	assertOperationCount(t, baseStore, 1)
}

func TestReconcileRestorePreservesArtifactsOnUnexpectedStoreError(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	lookupErr := errors.New("machine backend unavailable")
	hostDaemon, err := New(cfg, machineLookupErrorStore{Store: baseStore, err: lookupErr}, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	operation := model.OperationRecord{
		MachineID: "vm-1",
		Type:      model.MachineOperationRestore,
		StartedAt: time.Now().UTC(),
	}
	if err := baseStore.UpsertOperation(context.Background(), operation); err != nil {
		t.Fatalf("upsert operation: %v", err)
	}

	systemVolumeDir := filepath.Dir(hostDaemon.systemVolumePath("vm-1"))
	runtimeDir := hostDaemon.machineRuntimeBaseDir("vm-1")
	for _, dir := range []string{systemVolumeDir, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create dir %q: %v", dir, err)
		}
	}

	err = hostDaemon.reconcileRestore(context.Background(), operation)
	if err == nil || !strings.Contains(err.Error(), "machine backend unavailable") {
		t.Fatalf("reconcileRestore error = %v, want wrapped lookup error", err)
	}
	for _, dir := range []string{systemVolumeDir, runtimeDir} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("directory %q should be preserved, stat error: %v", dir, statErr)
		}
	}
	assertOperationCount(t, baseStore, 1)
}

func TestStartMachineTransitionsToRunningWithHandshake(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
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

	startedAt := time.Unix(1700000200, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "vm-start",
			Phase:       firecracker.PhaseRunning,
			PID:         9999,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "vm-start", "root", "run", "firecracker.sock"),
			TapName:     "fctap-start",
			StartedAt:   &startedAt,
		},
	}

	hostDaemon, err := New(cfg, baseStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	artifactRef := contracthost.ArtifactRef{KernelImageURL: "kernel", RootFSURL: "rootfs"}
	kernelPath := filepath.Join(root, "artifact-kernel")
	rootFSPath := filepath.Join(root, "artifact-rootfs")
	systemVolumePath := filepath.Join(root, "machine-disks", "vm-start", "rootfs.ext4")
	for _, file := range []string{kernelPath, rootFSPath, systemVolumePath} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", file, err)
		}
		if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
			t.Fatalf("write file %q: %v", file, err)
		}
	}
	if err := baseStore.PutArtifact(context.Background(), model.ArtifactRecord{
		Ref:             artifactRef,
		LocalKey:        "artifact",
		LocalDir:        filepath.Join(root, "artifact"),
		KernelImagePath: kernelPath,
		RootFSPath:      rootFSPath,
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if err := baseStore.CreateVolume(context.Background(), model.VolumeRecord{
		ID:        "vm-start-system",
		Kind:      contracthost.VolumeKindSystem,
		Pool:      model.StoragePoolMachineDisks,
		Path:      systemVolumePath,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create system volume: %v", err)
	}
	if err := baseStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-start",
		Artifact:       artifactRef,
		SystemVolumeID: "vm-start-system",
		Ports:          defaultMachinePorts(),
		Phase:          contracthost.MachinePhaseStopped,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	response, err := hostDaemon.StartMachine(context.Background(), "vm-start")
	if err != nil {
		t.Fatalf("StartMachine error = %v", err)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("response machine phase = %q, want %q", response.Machine.Phase, contracthost.MachinePhaseRunning)
	}

	machine, err := baseStore.GetMachine(context.Background(), "vm-start")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("machine phase = %q, want %q", machine.Phase, contracthost.MachinePhaseRunning)
	}
	if machine.RuntimeHost != "127.0.0.1" || machine.TapDevice != "fctap-start" {
		t.Fatalf("machine runtime state mismatch, got runtime_host=%q tap=%q", machine.RuntimeHost, machine.TapDevice)
	}
	if machine.PID != 9999 || machine.SocketPath == "" || machine.StartedAt == nil {
		t.Fatalf("machine process state missing: pid=%d socket=%q started_at=%v", machine.PID, machine.SocketPath, machine.StartedAt)
	}
	if len(runtime.deleteCalls) != 0 {
		t.Fatalf("runtime delete calls = %d, want 0", len(runtime.deleteCalls))
	}
}

func TestRestoreSnapshotTransitionsToRunningWithHandshake(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	baseStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	startedAt := time.Unix(1700000300, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "restored-exhausted",
			Phase:       firecracker.PhaseRunning,
			PID:         8888,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored-exhausted", "root", "run", "firecracker.sock"),
			TapName:     "fctap-restore",
			StartedAt:   &startedAt,
		},
	}

	hostDaemon, err := New(cfg, baseStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(_ context.Context, host string, machineID contracthost.MachineID, guestConfig *contracthost.GuestConfig) error {
		t.Fatalf("restore snapshot should not synchronously reconfigure guest identity, host=%q machine=%q guest_config=%#v", host, machineID, guestConfig)
		return nil
	}

	artifactRef := contracthost.ArtifactRef{KernelImageURL: "kernel", RootFSURL: "rootfs"}
	kernelPath := filepath.Join(root, "artifact-kernel")
	rootFSPath := filepath.Join(root, "artifact-rootfs")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	if err := os.WriteFile(rootFSPath, []byte("rootfs"), 0o644); err != nil {
		t.Fatalf("write rootfs: %v", err)
	}
	if err := baseStore.PutArtifact(context.Background(), model.ArtifactRecord{
		Ref:             artifactRef,
		LocalKey:        "artifact",
		LocalDir:        filepath.Join(root, "artifact"),
		KernelImagePath: kernelPath,
		RootFSPath:      rootFSPath,
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	snapDir := filepath.Join(root, "snapshots", "snap-exhausted")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	systemDisk := buildTestExt4ImageBytes(t)
	snapDisk := filepath.Join(snapDir, "system.img")
	if err := os.WriteFile(snapDisk, systemDisk, 0o644); err != nil {
		t.Fatalf("write snapshot disk: %v", err)
	}
	if err := baseStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:                "snap-exhausted",
		MachineID:         "source",
		Artifact:          artifactRef,
		DiskPaths:         []string{snapDisk},
		SourceRuntimeHost: "172.16.0.2",
		SourceTapDevice:   "fctap0",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": systemDisk,
	})
	defer server.Close()

	response, err := hostDaemon.RestoreSnapshot(context.Background(), "snap-exhausted", contracthost.RestoreSnapshotRequest{
		MachineID: "restored-exhausted",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID: "snap-exhausted",
			MachineID:  "source",
			ImageID:    "image-1",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system"},
			},
		},
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}
	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("restored machine phase = %q, want %q", response.Machine.Phase, contracthost.MachinePhaseRunning)
	}
	if _, err := baseStore.GetVolume(context.Background(), "restored-exhausted-system"); err != nil {
		t.Fatalf("restored system volume record should exist: %v", err)
	}
	if _, err := os.Stat(hostDaemon.systemVolumePath("restored-exhausted")); err != nil {
		t.Fatalf("restored system disk should exist: %v", err)
	}
	if len(runtime.deleteCalls) != 0 {
		t.Fatalf("runtime delete calls = %d, want 0", len(runtime.deleteCalls))
	}
	assertOperationCount(t, baseStore, 0)
}

func TestCreateSnapshotRejectsDuplicateSnapshotIDWithoutTouchingExistingArtifacts(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	machineID := contracthost.MachineID("vm-1")
	snapshotID := contracthost.SnapshotID("snap-1")
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             machineID,
		SystemVolumeID: hostDaemon.systemVolumeID(machineID),
		RuntimeHost:    "127.0.0.1",
		SocketPath:     filepath.Join(cfg.RuntimeDir, "machines", string(machineID), "root", "run", "firecracker.sock"),
		Phase:          contracthost.MachinePhaseRunning,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	snapshotDir := filepath.Join(cfg.SnapshotsDir, string(snapshotID))
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	markerPath := filepath.Join(snapshotDir, "keep.txt")
	if err := os.WriteFile(markerPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}
	if err := fileStore.CreateSnapshot(context.Background(), model.SnapshotRecord{
		ID:          snapshotID,
		MachineID:   machineID,
		MemFilePath: markerPath,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	_, err = hostDaemon.CreateSnapshot(context.Background(), machineID, contracthost.CreateSnapshotRequest{
		SnapshotID: snapshotID,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateSnapshot error = %v, want duplicate snapshot failure", err)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("marker file should be preserved, stat error: %v", statErr)
	}
	assertOperationCount(t, fileStore, 0)
}

func TestStopMachineContinuesWhenGuestSyncFails(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.syncGuestFilesystem = func(context.Context, string) error {
		return errors.New("guest sync failed")
	}
	hostDaemon.shutdownGuest = func(context.Context, string) error {
		runtime.inspectOverride = func(state firecracker.MachineState) (*firecracker.MachineState, error) {
			state.Phase = firecracker.PhaseStopped
			state.PID = 0
			return &state, nil
		}
		return nil
	}

	now := time.Now().UTC()
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:             "vm-stop-fail",
		SystemVolumeID: "vm-stop-fail-system",
		RuntimeHost:    "172.16.0.2",
		TapDevice:      "fctap-stop-fail",
		Phase:          contracthost.MachinePhaseRunning,
		PID:            1234,
		SocketPath:     filepath.Join(root, "runtime", "vm-stop-fail.sock"),
		Ports:          defaultMachinePorts(),
		CreatedAt:      now,
		StartedAt:      &now,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if err := hostDaemon.StopMachine(context.Background(), "vm-stop-fail"); err != nil {
		t.Fatalf("StopMachine returned error despite sync failure: %v", err)
	}
	if len(runtime.deleteCalls) != 1 {
		t.Fatalf("runtime delete calls = %d, want 1", len(runtime.deleteCalls))
	}

	machine, err := fileStore.GetMachine(context.Background(), "vm-stop-fail")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if machine.Phase != contracthost.MachinePhaseStopped {
		t.Fatalf("machine phase = %q, want %q", machine.Phase, contracthost.MachinePhaseStopped)
	}
}

func TestOrderedRestoredUserDiskArtifactsSortsByDriveIndex(t *testing.T) {
	ordered := orderedRestoredUserDiskArtifacts(map[string]restoredSnapshotArtifact{
		"user-10.img": {Artifact: contracthost.SnapshotArtifact{Name: "user-10.img"}},
		"user-2.img":  {Artifact: contracthost.SnapshotArtifact{Name: "user-2.img"}},
		"user-1.img":  {Artifact: contracthost.SnapshotArtifact{Name: "user-1.img"}},
		"system.img":  {Artifact: contracthost.SnapshotArtifact{Name: "system.img"}},
	})

	names := make([]string, 0, len(ordered))
	for _, artifact := range ordered {
		names = append(names, artifact.Artifact.Name)
	}
	if got, want := strings.Join(names, ","), "user-1.img,user-2.img,user-10.img"; got != want {
		t.Fatalf("ordered restored artifacts = %q, want %q", got, want)
	}
}

func TestRestoreSnapshotCleansStagingArtifactsAfterSuccess(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer func() { _ = sshListener.Close() }()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer func() { _ = vncListener.Close() }()

	startedAt := time.Unix(1700000400, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "restored-clean",
			Phase:       firecracker.PhaseRunning,
			PID:         7777,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "restored-clean", "root", "run", "firecracker.sock"),
			TapName:     "fctap-clean",
			StartedAt:   &startedAt,
		},
	}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.reconfigureGuestIdentity = func(context.Context, string, contracthost.MachineID, *contracthost.GuestConfig) error { return nil }
	systemDisk := buildTestExt4ImageBytes(t)

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": systemDisk,
	})
	defer server.Close()

	_, err = hostDaemon.RestoreSnapshot(context.Background(), "snap-clean", contracthost.RestoreSnapshotRequest{
		MachineID: "restored-clean",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap-clean",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system", SHA256Hex: mustSHA256Hex(t, systemDisk)},
			},
		},
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}

	stagingDir := filepath.Join(cfg.SnapshotsDir, "snap-clean", "restores", "restored-clean")
	if _, statErr := os.Stat(stagingDir); !os.IsNotExist(statErr) {
		t.Fatalf("restore staging dir should be cleaned up, stat err = %v", statErr)
	}
}

func TestRestoreSnapshotCleansStagingArtifactsAfterDownloadFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	systemDisk := buildTestExt4ImageBytes(t)

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": systemDisk,
	})
	defer server.Close()

	_, err = hostDaemon.RestoreSnapshot(context.Background(), "snap-fail-clean", contracthost.RestoreSnapshotRequest{
		MachineID: "restored-fail-clean",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap-fail-clean",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/missing"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "download durable snapshot artifacts") {
		t.Fatalf("RestoreSnapshot error = %v, want durable artifact download failure", err)
	}

	stagingDir := filepath.Join(cfg.SnapshotsDir, "snap-fail-clean", "restores", "restored-fail-clean")
	if _, statErr := os.Stat(stagingDir); !os.IsNotExist(statErr) {
		t.Fatalf("restore staging dir should be cleaned up after download failure, stat err = %v", statErr)
	}
}

func TestRestoreSnapshotCleansMachineDiskDirOnInjectFailure(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)
	hostDaemon.injectMachineIdentity = func(context.Context, string, contracthost.MachineID) error {
		return errors.New("inject failed")
	}

	server := newRestoreArtifactServer(t, map[string][]byte{
		"/kernel": []byte("kernel"),
		"/rootfs": []byte("rootfs"),
		"/system": buildTestExt4ImageBytes(t),
	})
	defer server.Close()

	_, err = hostDaemon.RestoreSnapshot(context.Background(), "snap-inject-fail", contracthost.RestoreSnapshotRequest{
		MachineID: "restored-inject-fail",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		Snapshot: &contracthost.DurableSnapshotSpec{
			SnapshotID:        "snap-inject-fail",
			MachineID:         "source",
			ImageID:           "image-1",
			SourceRuntimeHost: "172.16.0.2",
			SourceTapDevice:   "fctap0",
			Artifacts: []contracthost.SnapshotArtifact{
				{ID: "disk-system", Kind: contracthost.SnapshotArtifactKindDisk, Name: "system.img", DownloadURL: server.URL + "/system"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "inject machine identity for restore") {
		t.Fatalf("RestoreSnapshot error = %v, want inject machine identity failure", err)
	}

	machineDiskDir := filepath.Join(cfg.MachineDisksDir, "restored-inject-fail")
	if _, statErr := os.Stat(machineDiskDir); !os.IsNotExist(statErr) {
		t.Fatalf("machine disk dir should be cleaned up, stat err = %v", statErr)
	}
}

func TestReconcileUsesReconciledMachineStateForPublishedPorts(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := hoststore.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &failingInspectRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}
	stubGuestSSHPublicKeyReader(hostDaemon)

	t.Cleanup(func() {
		hostDaemon.stopPublishedPortProxy("port-1")
	})

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve host port: %v", err)
	}
	hostPort := uint16(reserved.Addr().(*net.TCPAddr).Port)
	if err := reserved.Close(); err != nil {
		t.Fatalf("close reserved host port: %v", err)
	}

	machineID := contracthost.MachineID("vm-1")
	if err := fileStore.CreateMachine(context.Background(), model.MachineRecord{
		ID:          machineID,
		RuntimeHost: "127.0.0.1",
		SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", string(machineID), "root", "run", "firecracker.sock"),
		TapDevice:   "fctap0",
		Phase:       contracthost.MachinePhaseRunning,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	if err := fileStore.CreatePublishedPort(context.Background(), model.PublishedPortRecord{
		ID:        "port-1",
		MachineID: machineID,
		Port:      8080,
		HostPort:  hostPort,
		Protocol:  contracthost.PortProtocolTCP,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create published port: %v", err)
	}

	if err := hostDaemon.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	updated, err := fileStore.GetMachine(context.Background(), machineID)
	if err != nil {
		t.Fatalf("get machine after reconcile: %v", err)
	}
	if updated.Phase != contracthost.MachinePhaseFailed {
		t.Fatalf("machine phase = %q, want failed", updated.Phase)
	}
	if updated.RuntimeHost != "" {
		t.Fatalf("machine runtime host = %q, want cleared", updated.RuntimeHost)
	}

	hostDaemon.publishedPortsMu.Lock()
	listenerCount := len(hostDaemon.publishedPortListeners)
	hostDaemon.publishedPortsMu.Unlock()
	if listenerCount != 0 {
		t.Fatalf("published port listeners = %d, want 0", listenerCount)
	}
}

func waitPublishedPortResult(t *testing.T, ch <-chan publishedPortResult) publishedPortResult {
	t.Helper()

	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for CreatePublishedPort result")
		return publishedPortResult{}
	}
}

func mustSHA256Hex(t *testing.T, payload []byte) string {
	t.Helper()

	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func assertOperationCount(t *testing.T, store hoststore.Store, want int) {
	t.Helper()

	operations, err := store.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != want {
		t.Fatalf("operation count = %d, want %d", len(operations), want)
	}
}
