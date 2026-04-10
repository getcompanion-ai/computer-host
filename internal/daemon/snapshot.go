package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	if err := cloneDiskFile(systemVolume.Path, systemDiskTarget, d.config.DiskCloneMode); err != nil {
		_ = d.runtime.Resume(ctx, runtimeState)
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("copy system disk: %w", err)
	}
	diskPaths = append(diskPaths, systemDiskTarget)
	for i, volumeID := range record.UserVolumeIDs {
		volume, err := d.store.GetVolume(ctx, volumeID)
		if err != nil {
			_ = d.runtime.Resume(ctx, runtimeState)
			_ = os.RemoveAll(snapshotDir)
			return nil, fmt.Errorf("get attached volume %q: %w", volumeID, err)
		}
		driveID := fmt.Sprintf("user-%d", i)
		targetPath := filepath.Join(snapshotDir, driveID+".img")
		if err := cloneDiskFile(volume.Path, targetPath, d.config.DiskCloneMode); err != nil {
			_ = d.runtime.Resume(ctx, runtimeState)
			_ = os.RemoveAll(snapshotDir)
			return nil, fmt.Errorf("copy attached volume %q: %w", volumeID, err)
		}
		diskPaths = append(diskPaths, targetPath)
	}

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

	artifacts, err := buildSnapshotArtifacts(dstMemPath, dstStatePath, diskPaths)
	if err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("build snapshot artifacts: %w", err)
	}

	now := time.Now().UTC()
	snapshotRecord := model.SnapshotRecord{
		ID:                snapshotID,
		MachineID:         machineID,
		Artifact:          record.Artifact,
		MemFilePath:       dstMemPath,
		StateFilePath:     dstStatePath,
		DiskPaths:         diskPaths,
		Artifacts:         artifacts,
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
		Snapshot:  snapshotToContract(snapshotRecord),
		Artifacts: snapshotArtifactsToContract(snapshotRecord.Artifacts),
	}, nil
}

func (d *Daemon) UploadSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID, req contracthost.UploadSnapshotRequest) (*contracthost.UploadSnapshotResponse, error) {
	snapshot, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	artifactIndex := make(map[string]model.SnapshotArtifactRecord, len(snapshot.Artifacts))
	for _, artifact := range snapshot.Artifacts {
		artifactIndex[artifact.ID] = artifact
	}

	response := &contracthost.UploadSnapshotResponse{
		Artifacts: make([]contracthost.UploadedSnapshotArtifact, 0, len(req.Artifacts)),
	}
	for _, upload := range req.Artifacts {
		artifact, ok := artifactIndex[upload.ArtifactID]
		if !ok {
			return nil, fmt.Errorf("snapshot %q artifact %q not found", snapshotID, upload.ArtifactID)
		}
		completedParts, err := uploadSnapshotArtifact(ctx, artifact.LocalPath, upload.Parts)
		if err != nil {
			return nil, fmt.Errorf("upload snapshot artifact %q: %w", upload.ArtifactID, err)
		}
		response.Artifacts = append(response.Artifacts, contracthost.UploadedSnapshotArtifact{
			ArtifactID:     upload.ArtifactID,
			CompletedParts: completedParts,
		})
	}

	return response, nil
}

func (d *Daemon) RestoreSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID, req contracthost.RestoreSnapshotRequest) (*contracthost.RestoreSnapshotResponse, error) {
	if err := validateMachineID(req.MachineID); err != nil {
		return nil, err
	}
	if req.Snapshot.SnapshotID != "" && req.Snapshot.SnapshotID != snapshotID {
		return nil, fmt.Errorf("snapshot id mismatch: path=%q payload=%q", snapshotID, req.Snapshot.SnapshotID)
	}
	if err := validateArtifactRef(req.Artifact); err != nil {
		return nil, err
	}
	if err := validateGuestConfig(req.GuestConfig); err != nil {
		return nil, err
	}
	guestConfig, err := d.mergedGuestConfig(req.GuestConfig)
	if err != nil {
		return nil, err
	}

	unlock := d.lockMachine(req.MachineID)
	defer unlock()

	if _, err := d.store.GetMachine(ctx, req.MachineID); err == nil {
		return nil, fmt.Errorf("machine %q already exists", req.MachineID)
	} else if err != nil && err != store.ErrNotFound {
		return nil, err
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

	usedNetworks, err := d.listRunningNetworks(ctx, req.MachineID)
	if err != nil {
		return nil, err
	}
	restoreNetwork, err := d.resolveRestoreNetwork(ctx, snapshotID, req.Snapshot)
	if err != nil {
		clearOperation = true
		return nil, err
	}
	if networkAllocationInUse(restoreNetwork, usedNetworks) {
		clearOperation = true
		return nil, fmt.Errorf("restore network for snapshot %q is still in use on this host (runtime_host=%s tap_device=%s)", snapshotID, restoreNetwork.GuestIP(), restoreNetwork.TapName)
	}

	artifact, err := d.ensureArtifact(ctx, req.Artifact)
	if err != nil {
		return nil, fmt.Errorf("ensure artifact for restore: %w", err)
	}

	stagingDir := filepath.Join(d.config.SnapshotsDir, string(snapshotID), "restores", string(req.MachineID))
	restoredArtifacts, err := downloadDurableSnapshotArtifacts(ctx, stagingDir, req.Snapshot.Artifacts)
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		clearOperation = true
		return nil, fmt.Errorf("download durable snapshot artifacts: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	// COW-copy system disk from snapshot to new machine's disk dir.
	newSystemDiskPath := d.systemVolumePath(req.MachineID)
	if err := os.MkdirAll(filepath.Dir(newSystemDiskPath), 0o755); err != nil {
		return nil, fmt.Errorf("create machine disk dir: %w", err)
	}
	systemDiskPath, ok := restoredArtifacts["system.img"]
	if !ok {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q is missing system disk artifact", snapshotID)
	}
	memoryArtifact, ok := restoredArtifacts["memory.bin"]
	if !ok {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q is missing memory artifact", snapshotID)
	}
	vmstateArtifact, ok := restoredArtifacts["vmstate.bin"]
	if !ok {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q is missing vmstate artifact", snapshotID)
	}
	if err := cloneDiskFile(systemDiskPath.LocalPath, newSystemDiskPath, d.config.DiskCloneMode); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("copy system disk for restore: %w", err)
	}

	type restoredUserVolume struct {
		ID      contracthost.VolumeID
		Path    string
		DriveID string
	}
	restoredUserVolumes := make([]restoredUserVolume, 0)
	restoredDrivePaths := make(map[string]string)
	for _, restored := range orderedRestoredUserDiskArtifacts(restoredArtifacts) {
		name := restored.Artifact.Name
		driveID := strings.TrimSuffix(name, filepath.Ext(name))
		volumeID := contracthost.VolumeID(fmt.Sprintf("%s-%s", req.MachineID, driveID))
		volumePath := filepath.Join(d.config.MachineDisksDir, string(req.MachineID), name)
		if err := cloneDiskFile(restored.LocalPath, volumePath, d.config.DiskCloneMode); err != nil {
			clearOperation = true
			return nil, fmt.Errorf("copy restored drive %q: %w", driveID, err)
		}
		restoredUserVolumes = append(restoredUserVolumes, restoredUserVolume{
			ID:      volumeID,
			Path:    volumePath,
			DriveID: driveID,
		})
		restoredDrivePaths[driveID] = volumePath
	}

	// Do not force vsock_override on restore: Firecracker rejects it for old
	// snapshots without a vsock device, and the jailed /run path already
	// relocates safely for snapshots created with the new vsock-backed guest.
	loadSpec := firecracker.SnapshotLoadSpec{
		ID:              firecracker.MachineID(req.MachineID),
		SnapshotPath:    vmstateArtifact.LocalPath,
		MemFilePath:     memoryArtifact.LocalPath,
		RootFSPath:      newSystemDiskPath,
		KernelImagePath: artifact.KernelImagePath,
		DiskPaths:       restoredDrivePaths,
		Network:         &restoreNetwork,
	}

	machineState, err := d.runtime.RestoreBoot(ctx, loadSpec, usedNetworks)
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("restore boot: %w", err)
	}

	systemVolumeID := d.systemVolumeID(req.MachineID)
	now := time.Now().UTC()

	if err := d.store.CreateVolume(ctx, model.VolumeRecord{
		ID:                systemVolumeID,
		Kind:              contracthost.VolumeKindSystem,
		AttachedMachineID: machineIDPtr(req.MachineID),
		SourceArtifact:    &req.Artifact,
		Pool:              model.StoragePoolMachineDisks,
		Path:              newSystemDiskPath,
		CreatedAt:         now,
	}); err != nil {
		_ = d.runtime.Delete(ctx, *machineState)
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, fmt.Errorf("create system volume record for restore: %w", err)
	}
	restoredUserVolumeIDs := make([]contracthost.VolumeID, 0, len(restoredUserVolumes))
	for _, volume := range restoredUserVolumes {
		if err := d.store.CreateVolume(ctx, model.VolumeRecord{
			ID:                volume.ID,
			Kind:              contracthost.VolumeKindUser,
			AttachedMachineID: machineIDPtr(req.MachineID),
			SourceArtifact:    &req.Artifact,
			Pool:              model.StoragePoolMachineDisks,
			Path:              volume.Path,
			CreatedAt:         now,
		}); err != nil {
			for _, restoredVolumeID := range restoredUserVolumeIDs {
				_ = d.store.DeleteVolume(context.Background(), restoredVolumeID)
			}
			_ = d.store.DeleteVolume(context.Background(), systemVolumeID)
			_ = d.runtime.Delete(ctx, *machineState)
			_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
			clearOperation = true
			return nil, fmt.Errorf("create restored user volume record %q: %w", volume.ID, err)
		}
		restoredUserVolumeIDs = append(restoredUserVolumeIDs, volume.ID)
	}

	machineRecord := model.MachineRecord{
		ID:                req.MachineID,
		Artifact:          req.Artifact,
		GuestConfig:       cloneGuestConfig(guestConfig),
		SystemVolumeID:    systemVolumeID,
		UserVolumeIDs:     restoredUserVolumeIDs,
		RuntimeHost:       machineState.RuntimeHost,
		TapDevice:         machineState.TapName,
		Ports:             defaultMachinePorts(),
		GuestSSHPublicKey: "",
		Phase:             contracthost.MachinePhaseStarting,
		PID:               machineState.PID,
		SocketPath:        machineState.SocketPath,
		CreatedAt:         now,
		StartedAt:         machineState.StartedAt,
	}
	if err := d.store.CreateMachine(ctx, machineRecord); err != nil {
		for _, restoredVolumeID := range restoredUserVolumeIDs {
			_ = d.store.DeleteVolume(context.Background(), restoredVolumeID)
		}
		_ = d.store.DeleteVolume(context.Background(), systemVolumeID)
		_ = d.runtime.Delete(ctx, *machineState)
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
		clearOperation = true
		return nil, err
	}

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
		ID:                record.ID,
		MachineID:         record.MachineID,
		SourceRuntimeHost: record.SourceRuntimeHost,
		SourceTapDevice:   record.SourceTapDevice,
		CreatedAt:         record.CreatedAt,
	}
}

func snapshotArtifactsToContract(artifacts []model.SnapshotArtifactRecord) []contracthost.SnapshotArtifact {
	converted := make([]contracthost.SnapshotArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		converted = append(converted, contracthost.SnapshotArtifact{
			ID:        artifact.ID,
			Kind:      artifact.Kind,
			Name:      artifact.Name,
			SizeBytes: artifact.SizeBytes,
			SHA256Hex: artifact.SHA256Hex,
		})
	}
	return converted
}

func orderedRestoredUserDiskArtifacts(artifacts map[string]restoredSnapshotArtifact) []restoredSnapshotArtifact {
	ordered := make([]restoredSnapshotArtifact, 0, len(artifacts))
	for name, artifact := range artifacts {
		if !strings.HasPrefix(name, "user-") || filepath.Ext(name) != ".img" {
			continue
		}
		ordered = append(ordered, artifact)
	}
	sort.Slice(ordered, func(i, j int) bool {
		iIdx, iOK := restoredUserDiskIndex(ordered[i].Artifact.Name)
		jIdx, jOK := restoredUserDiskIndex(ordered[j].Artifact.Name)
		switch {
		case iOK && jOK && iIdx != jIdx:
			return iIdx < jIdx
		case iOK != jOK:
			return iOK
		default:
			return ordered[i].Artifact.Name < ordered[j].Artifact.Name
		}
	})
	return ordered
}

func restoredUserDiskIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "user-") || filepath.Ext(name) != ".img" {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, "user-"), filepath.Ext(name))
	index, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return index, true
}

func (d *Daemon) resolveRestoreNetwork(ctx context.Context, snapshotID contracthost.SnapshotID, spec contracthost.DurableSnapshotSpec) (firecracker.NetworkAllocation, error) {
	if network, err := restoreNetworkFromDurableSpec(spec); err == nil {
		return network, nil
	}

	snapshot, err := d.store.GetSnapshot(ctx, snapshotID)
	if err == nil {
		return restoreNetworkFromSnapshot(snapshot)
	}
	if err != store.ErrNotFound {
		return firecracker.NetworkAllocation{}, err
	}
	return firecracker.NetworkAllocation{}, fmt.Errorf("snapshot %q is missing restore network metadata", snapshotID)
}

func restoreNetworkFromDurableSpec(spec contracthost.DurableSnapshotSpec) (firecracker.NetworkAllocation, error) {
	if strings.TrimSpace(spec.SourceRuntimeHost) == "" || strings.TrimSpace(spec.SourceTapDevice) == "" {
		return firecracker.NetworkAllocation{}, fmt.Errorf("durable snapshot spec is missing restore network metadata")
	}
	network, err := firecracker.AllocationFromGuestIP(spec.SourceRuntimeHost, spec.SourceTapDevice)
	if err != nil {
		return firecracker.NetworkAllocation{}, fmt.Errorf("reconstruct durable snapshot %q network: %w", spec.SnapshotID, err)
	}
	return network, nil
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
