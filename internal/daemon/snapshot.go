package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) CreateSnapshot(ctx context.Context, machineID contracthost.MachineID, req contracthost.CreateSnapshotRequest) (*contracthost.CreateSnapshotResponse, error) {
	unlock := d.lockMachine(machineID)
	defer unlock()

	if err := validateSnapshotID(req.SnapshotID); err != nil {
		return nil, err
	}

	record, err := d.store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if record.Phase != contracthost.MachinePhaseRunning {
		return nil, fmt.Errorf("machine %q is not running", machineID)
	}

	// Reject if an operation is already pending for this machine
	if ops, err := d.store.ListOperations(ctx); err == nil {
		for _, op := range ops {
			if op.MachineID == machineID {
				return nil, fmt.Errorf("machine %q has a pending %q operation (started %s)", machineID, op.Type, op.StartedAt.Format(time.RFC3339))
			}
		}
	}

	snapshotID := req.SnapshotID
	if _, err := d.store.GetSnapshot(ctx, snapshotID); err == nil {
		return nil, fmt.Errorf("snapshot %q already exists", snapshotID)
	} else if err != nil && err != store.ErrNotFound {
		return nil, err
	}

	if err := d.store.UpsertOperation(ctx, model.OperationRecord{
		MachineID:  machineID,
		Type:       model.MachineOperationSnapshot,
		StartedAt:  time.Now().UTC(),
		SnapshotID: &snapshotID,
	}); err != nil {
		return nil, err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), machineID)
		}
	}()

	snapshotDir := filepath.Join(d.config.SnapshotsDir, string(snapshotID))
	if err := os.Mkdir(snapshotDir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("snapshot %q already exists", snapshotID)
		}
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	runtimeState := machineToRuntimeState(*record)

	// Pause the VM
	if err := d.runtime.Pause(ctx, runtimeState); err != nil {
		return nil, fmt.Errorf("pause machine %q: %w", machineID, err)
	}

	// Write snapshot inside the chroot (Firecracker can only write there)
	// Use jailed paths relative to the chroot root
	chrootMemPath := "memory.bin"
	chrootStatePath := "vmstate.bin"

	if err := d.runtime.CreateSnapshot(ctx, runtimeState, firecracker.SnapshotPaths{
		MemFilePath:   chrootMemPath,
		StateFilePath: chrootStatePath,
	}); err != nil {
		_ = d.runtime.Resume(ctx, runtimeState)
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("create snapshot for %q: %w", machineID, err)
	}

	// COW-copy disk files while paused for consistency
	var diskPaths []string
	systemVolume, err := d.store.GetVolume(ctx, record.SystemVolumeID)
	if err != nil {
		_ = d.runtime.Resume(ctx, runtimeState)
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("get system volume: %w", err)
	}
	systemDiskTarget := filepath.Join(snapshotDir, "system.img")
	if err := cowCopyFile(systemVolume.Path, systemDiskTarget); err != nil {
		_ = d.runtime.Resume(ctx, runtimeState)
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("copy system disk: %w", err)
	}
	diskPaths = append(diskPaths, systemDiskTarget)

	// Resume the source VM
	if err := d.runtime.Resume(ctx, runtimeState); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("resume machine %q: %w", machineID, err)
	}

	// Copy snapshot files from chroot to snapshot directory, then remove originals.
	// os.Rename fails across filesystem boundaries (/proc/<pid>/root/ is on procfs).
	chrootRoot := filepath.Dir(filepath.Dir(runtimeState.SocketPath)) // strip /run/firecracker.socket
	srcMemPath := filepath.Join(chrootRoot, chrootMemPath)
	srcStatePath := filepath.Join(chrootRoot, chrootStatePath)
	dstMemPath := filepath.Join(snapshotDir, "memory.bin")
	dstStatePath := filepath.Join(snapshotDir, "vmstate.bin")

	if err := moveFile(srcMemPath, dstMemPath); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("move memory file: %w", err)
	}
	if err := moveFile(srcStatePath, dstStatePath); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("move vmstate file: %w", err)
	}

	now := time.Now().UTC()
	snapshotRecord := model.SnapshotRecord{
		ID:                snapshotID,
		MachineID:         machineID,
		Artifact:          record.Artifact,
		MemFilePath:       dstMemPath,
		StateFilePath:     dstStatePath,
		DiskPaths:         diskPaths,
		SourceRuntimeHost: record.RuntimeHost,
		SourceTapDevice:   record.TapDevice,
		CreatedAt:         now,
	}
	if err := d.store.CreateSnapshot(ctx, snapshotRecord); err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, err
	}

	clearOperation = true
	return &contracthost.CreateSnapshotResponse{
		Snapshot: snapshotToContract(snapshotRecord),
	}, nil
}

func (d *Daemon) RestoreSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID, req contracthost.RestoreSnapshotRequest) (*contracthost.RestoreSnapshotResponse, error) {
	if err := validateMachineID(req.MachineID); err != nil {
		return nil, err
	}

	unlock := d.lockMachine(req.MachineID)
	defer unlock()

	snap, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	if _, err := d.store.GetMachine(ctx, req.MachineID); err == nil {
		return nil, fmt.Errorf("machine %q already exists", req.MachineID)
	}

	if err := d.store.UpsertOperation(ctx, model.OperationRecord{
		MachineID:  req.MachineID,
		Type:       model.MachineOperationRestore,
		StartedAt:  time.Now().UTC(),
		SnapshotID: &snapshotID,
	}); err != nil {
		return nil, err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), req.MachineID)
		}
	}()

	sourceMachine, err := d.store.GetMachine(ctx, snap.MachineID)
	switch {
	case err == nil && sourceMachine.Phase == contracthost.MachinePhaseRunning:
		clearOperation = true
		return nil, fmt.Errorf("restore from snapshot %q while source machine %q is running is not supported yet", snapshotID, snap.MachineID)
	case err != nil && err != store.ErrNotFound:
		return nil, fmt.Errorf("get source machine for restore: %w", err)
	}

	usedNetworks, err := d.listRunningNetworks(ctx, req.MachineID)
	if err != nil {
		return nil, err
	}
	restoreNetwork, err := restoreNetworkFromSnapshot(snap)
	if err != nil {
		clearOperation = true
		return nil, err
	}
	if networkAllocationInUse(restoreNetwork, usedNetworks) {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q restore network %q (%s) is already in use", snapshotID, restoreNetwork.TapName, restoreNetwork.GuestIP())
	}

	artifact, err := d.store.GetArtifact(ctx, snap.Artifact)
	if err != nil {
		return nil, fmt.Errorf("get artifact for restore: %w", err)
	}

	// COW-copy system disk from snapshot to new machine's disk dir.
	newSystemDiskPath := d.systemVolumePath(req.MachineID)
	if err := os.MkdirAll(filepath.Dir(newSystemDiskPath), 0o755); err != nil {
		return nil, fmt.Errorf("create machine disk dir: %w", err)
	}
	if len(snap.DiskPaths) < 1 {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q has no disk paths", snapshotID)
	}
	if err := cowCopyFile(snap.DiskPaths[0], newSystemDiskPath); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("copy system disk for restore: %w", err)
	}

	loadSpec := firecracker.SnapshotLoadSpec{
		ID:              firecracker.MachineID(req.MachineID),
		SnapshotPath:    snap.StateFilePath,
		MemFilePath:     snap.MemFilePath,
		RootFSPath:      newSystemDiskPath,
		KernelImagePath: artifact.KernelImagePath,
		DiskPaths:       map[string]string{},
		Network:         &restoreNetwork,
	}

	machineState, err := d.runtime.RestoreBoot(ctx, loadSpec, usedNetworks)
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("restore boot: %w", err)
	}

	// Wait for guest to become ready
	if err := waitForGuestReady(ctx, machineState.RuntimeHost, defaultMachinePorts()); err != nil {
		_ = d.runtime.Delete(ctx, *machineState)
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("wait for restored guest ready: %w", err)
	}
	if err := d.reconfigureGuestIdentity(ctx, machineState.RuntimeHost, req.MachineID); err != nil {
		_ = d.runtime.Delete(ctx, *machineState)
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("reconfigure restored guest identity: %w", err)
	}
	guestSSHPublicKey, err := d.readGuestSSHPublicKey(ctx, machineState.RuntimeHost)
	if err != nil {
		_ = d.runtime.Delete(ctx, *machineState)
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("read restored guest ssh host key: %w", err)
	}

	systemVolumeID := d.systemVolumeID(req.MachineID)
	now := time.Now().UTC()

	if err := d.store.CreateVolume(ctx, model.VolumeRecord{
		ID:                systemVolumeID,
		Kind:              contracthost.VolumeKindSystem,
		AttachedMachineID: machineIDPtr(req.MachineID),
		SourceArtifact:    &snap.Artifact,
		Pool:              model.StoragePoolMachineDisks,
		Path:              newSystemDiskPath,
		CreatedAt:         now,
	}); err != nil {
		return nil, err
	}

	machineRecord := model.MachineRecord{
		ID:                req.MachineID,
		Artifact:          snap.Artifact,
		SystemVolumeID:    systemVolumeID,
		RuntimeHost:       machineState.RuntimeHost,
		TapDevice:         machineState.TapName,
		Ports:             defaultMachinePorts(),
		GuestSSHPublicKey: guestSSHPublicKey,
		Phase:             contracthost.MachinePhaseRunning,
		PID:               machineState.PID,
		SocketPath:        machineState.SocketPath,
		CreatedAt:         now,
		StartedAt:         machineState.StartedAt,
	}
	d.relayAllocMu.Lock()
	sshRelayPort, err := d.allocateMachineRelayProxy(ctx, machineRecord, contracthost.MachinePortNameSSH, machineRecord.RuntimeHost, defaultSSHPort, minMachineSSHRelayPort, maxMachineSSHRelayPort)
	var vncRelayPort uint16
	if err == nil {
		vncRelayPort, err = d.allocateMachineRelayProxy(ctx, machineRecord, contracthost.MachinePortNameVNC, machineRecord.RuntimeHost, defaultVNCPort, minMachineVNCRelayPort, maxMachineVNCRelayPort)
	}
	d.relayAllocMu.Unlock()
	if err != nil {
		d.stopMachineRelays(machineRecord.ID)
		return nil, err
	}
	machineRecord.Ports = buildMachinePorts(sshRelayPort, vncRelayPort)
	startedRelays := true
	defer func() {
		if startedRelays {
			d.stopMachineRelays(machineRecord.ID)
		}
	}()
	if err := d.store.CreateMachine(ctx, machineRecord); err != nil {
		return nil, err
	}

	startedRelays = false
	clearOperation = true
	return &contracthost.RestoreSnapshotResponse{
		Machine: machineToContract(machineRecord),
	}, nil
}

func (d *Daemon) GetSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID) (*contracthost.GetSnapshotResponse, error) {
	snap, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	return &contracthost.GetSnapshotResponse{Snapshot: snapshotToContract(*snap)}, nil
}

func (d *Daemon) ListSnapshots(ctx context.Context, machineID contracthost.MachineID) (*contracthost.ListSnapshotsResponse, error) {
	records, err := d.store.ListSnapshotsByMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	snapshots := make([]contracthost.Snapshot, 0, len(records))
	for _, r := range records {
		snapshots = append(snapshots, snapshotToContract(r))
	}
	return &contracthost.ListSnapshotsResponse{Snapshots: snapshots}, nil
}

func (d *Daemon) DeleteSnapshotByID(ctx context.Context, snapshotID contracthost.SnapshotID) error {
	snap, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}
	snapshotDir := filepath.Dir(snap.MemFilePath)
	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("remove snapshot dir %q: %w", snapshotDir, err)
	}
	return d.store.DeleteSnapshot(ctx, snapshotID)
}

func snapshotToContract(record model.SnapshotRecord) contracthost.Snapshot {
	return contracthost.Snapshot{
		ID:        record.ID,
		MachineID: record.MachineID,
		CreatedAt: record.CreatedAt,
	}
}

func restoreNetworkFromSnapshot(snap *model.SnapshotRecord) (firecracker.NetworkAllocation, error) {
	if snap == nil {
		return firecracker.NetworkAllocation{}, fmt.Errorf("snapshot is required")
	}
	if strings.TrimSpace(snap.SourceRuntimeHost) == "" || strings.TrimSpace(snap.SourceTapDevice) == "" {
		return firecracker.NetworkAllocation{}, fmt.Errorf("snapshot %q is missing restore network metadata", snap.ID)
	}
	network, err := firecracker.AllocationFromGuestIP(snap.SourceRuntimeHost, snap.SourceTapDevice)
	if err != nil {
		return firecracker.NetworkAllocation{}, fmt.Errorf("reconstruct snapshot %q network: %w", snap.ID, err)
	}
	return network, nil
}

func networkAllocationInUse(target firecracker.NetworkAllocation, used []firecracker.NetworkAllocation) bool {
	targetTap := strings.TrimSpace(target.TapName)
	for _, network := range used {
		if network.GuestIP() == target.GuestIP() {
			return true
		}
		if targetTap != "" && strings.TrimSpace(network.TapName) == targetTap {
			return true
		}
	}
	return false
}

// moveFile copies src to dst then removes src. Works across filesystem boundaries
// unlike os.Rename, which is needed when moving files out of /proc/<pid>/root/.
func moveFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		_ = in.Close()
	}()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

func cowCopyFile(source string, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create target dir for %q: %w", target, err)
	}
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", source, target)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if cloneErr := cloneFile(source, target); cloneErr == nil {
			return nil
		} else {
			return fmt.Errorf("cow copy %q to %q: cp failed: %w: %s; clone fallback failed: %w", source, target, err, string(output), cloneErr)
		}
	}
	return nil
}
