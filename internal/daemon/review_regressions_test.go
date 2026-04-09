package daemon

import (
	"context"
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
