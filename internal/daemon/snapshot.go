package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/getcompanion-ai/computer-host/internal/httpapi"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const localSnapshotRestoreUnavailablePrefix = "local snapshot restore unavailable"

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

	artifacts, err := buildSnapshotArtifacts(diskPaths)
	if err != nil {
		_ = os.RemoveAll(snapshotDir)
		return nil, fmt.Errorf("build snapshot artifacts: %w", err)
	}

	now := time.Now().UTC()
	snapshotRecord := model.SnapshotRecord{
		ID:        snapshotID,
		MachineID: machineID,
		Artifact:  record.Artifact,
		DiskPaths: diskPaths,
		Artifacts: artifacts,
		CreatedAt: now,
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
	uploads := make([]contracthost.UploadedSnapshotArtifact, len(req.Artifacts))
	group, groupCtx := errgroup.WithContext(ctx)
	for i, upload := range req.Artifacts {
		i := i
		upload := upload
		group.Go(func() error {
			artifact, ok := artifactIndex[upload.ArtifactID]
			if !ok {
				return fmt.Errorf("snapshot %q artifact %q not found", snapshotID, upload.ArtifactID)
			}
			completedParts, err := uploadSnapshotArtifact(groupCtx, artifact.LocalPath, upload.Parts)
			if err != nil {
				return fmt.Errorf("upload snapshot artifact %q: %w", upload.ArtifactID, err)
			}
			uploads[i] = contracthost.UploadedSnapshotArtifact{
				ArtifactID:     upload.ArtifactID,
				CompletedParts: completedParts,
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	response.Artifacts = append(response.Artifacts, uploads...)

	return response, nil
}

func (d *Daemon) RestoreSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID, req contracthost.RestoreSnapshotRequest) (*contracthost.RestoreSnapshotResponse, error) {
	if err := validateMachineID(req.MachineID); err != nil {
		return nil, err
	}
	if err := validateArtifactRef(req.Artifact); err != nil {
		return nil, err
	}
	if req.LocalSnapshot == nil && req.Snapshot == nil {
		return nil, fmt.Errorf("restore request must include local_snapshot or snapshot")
	}
	if req.LocalSnapshot != nil && req.LocalSnapshot.SnapshotID != "" && req.LocalSnapshot.SnapshotID != snapshotID {
		return nil, fmt.Errorf("local snapshot id mismatch: path=%q payload=%q", snapshotID, req.LocalSnapshot.SnapshotID)
	}
	if req.Snapshot != nil && req.Snapshot.SnapshotID != "" && req.Snapshot.SnapshotID != snapshotID {
		return nil, fmt.Errorf("snapshot id mismatch: path=%q payload=%q", snapshotID, req.Snapshot.SnapshotID)
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

	restoredArtifacts, cleanupRestoreArtifacts, err := d.prepareRestoreArtifacts(ctx, snapshotID, req)
	if err != nil {
		clearOperation = true
		return nil, err
	}
	defer cleanupRestoreArtifacts()
	var (
		artifact     *model.ArtifactRecord
		guestHostKey *guestSSHHostKeyPair
		readyNonce   string
	)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		artifact, err = d.ensureArtifact(groupCtx, req.Artifact)
		if err != nil {
			return fmt.Errorf("ensure artifact for restore: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		var err error
		guestHostKey, err = generateGuestSSHHostKeyPair(groupCtx)
		return err
	})
	group.Go(func() error {
		var err error
		readyNonce, err = newGuestReadyNonce()
		return err
	})
	if err := group.Wait(); err != nil {
		clearOperation = true
		return nil, err
	}

	// COW-copy system disk from snapshot to new machine's disk dir.
	newSystemDiskPath := d.systemVolumePath(req.MachineID)
	if err := os.MkdirAll(filepath.Dir(newSystemDiskPath), 0o755); err != nil {
		return nil, fmt.Errorf("create machine disk dir: %w", err)
	}
	removeMachineDiskDirOnFailure := true
	defer func() {
		if !removeMachineDiskDirOnFailure {
			return
		}
		_ = os.RemoveAll(filepath.Dir(newSystemDiskPath))
	}()
	systemDiskPath, ok := restoredArtifacts["system.img"]
	if !ok {
		clearOperation = true
		return nil, fmt.Errorf("snapshot %q is missing system disk artifact", snapshotID)
	}
	if err := cloneDiskFile(systemDiskPath.LocalPath, newSystemDiskPath, d.config.DiskCloneMode); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("copy system disk for restore: %w", err)
	}
	if err := d.injectMachineIdentity(ctx, newSystemDiskPath, req.MachineID); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("inject machine identity for restore: %w", err)
	}
	if err := d.injectGuestConfig(ctx, newSystemDiskPath, guestConfig); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("inject guest config for restore: %w", err)
	}
	if err := injectGuestSSHHostKey(ctx, newSystemDiskPath, guestHostKey); err != nil {
		clearOperation = true
		return nil, fmt.Errorf("inject guest ssh host key for restore: %w", err)
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

	userVolumes := make([]model.VolumeRecord, 0, len(restoredUserVolumes))
	for _, volume := range restoredUserVolumes {
		userVolumes = append(userVolumes, model.VolumeRecord{
			ID:   volume.ID,
			Kind: contracthost.VolumeKindUser,
			Path: volume.Path,
		})
	}
	spec, err := d.buildMachineSpec(req.MachineID, artifact, userVolumes, newSystemDiskPath, guestConfig, readyNonce)
	if err != nil {
		clearOperation = true
		return nil, fmt.Errorf("build machine spec for restore: %w", err)
	}
	usedNetworks, err := d.listRunningNetworks(ctx, req.MachineID)
	if err != nil {
		clearOperation = true
		return nil, err
	}
	machineState, err := d.runtime.Boot(ctx, spec, usedNetworks)
	if err != nil {
		clearOperation = true
		return nil, fmt.Errorf("boot restored machine: %w", err)
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
		GuestSSHPublicKey: strings.TrimSpace(guestHostKey.PublicKey),
		GuestReadyNonce:   readyNonce,
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
		clearOperation = true
		return nil, err
	}

	record, err := d.completeMachineStartup(ctx, &machineRecord, *machineState)
	if err != nil {
		return nil, err
	}

	removeMachineDiskDirOnFailure = false
	clearOperation = true
	return &contracthost.RestoreSnapshotResponse{
		Machine: machineToContract(*record),
	}, nil
}

func (d *Daemon) GetSnapshot(ctx context.Context, snapshotID contracthost.SnapshotID) (*contracthost.GetSnapshotResponse, error) {
	snap, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	return &contracthost.GetSnapshotResponse{Snapshot: snapshotToContract(*snap)}, nil
}

func (d *Daemon) GetSnapshotArtifact(ctx context.Context, snapshotID contracthost.SnapshotID, artifactID string) (*httpapi.SnapshotArtifactContent, error) {
	snapshot, err := d.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil, fmt.Errorf("snapshot artifact id is required")
	}
	for _, artifact := range snapshot.Artifacts {
		if artifact.ID != artifactID {
			continue
		}
		path := strings.TrimSpace(artifact.LocalPath)
		if path == "" {
			return nil, fmt.Errorf("snapshot %q artifact %q not found", snapshotID, artifactID)
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("snapshot %q artifact %q not found", snapshotID, artifactID)
			}
			return nil, fmt.Errorf("stat snapshot %q artifact %q: %w", snapshotID, artifactID, err)
		}
		return &httpapi.SnapshotArtifactContent{
			Name: artifact.Name,
			Path: path,
		}, nil
	}
	return nil, fmt.Errorf("snapshot %q artifact %q not found", snapshotID, artifactID)
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

func (d *Daemon) prepareRestoreArtifacts(ctx context.Context, snapshotID contracthost.SnapshotID, req contracthost.RestoreSnapshotRequest) (map[string]restoredSnapshotArtifact, func(), error) {
	if req.LocalSnapshot != nil {
		if req.LocalSnapshot.SnapshotID != "" && req.LocalSnapshot.SnapshotID != snapshotID {
			return nil, func() {}, fmt.Errorf("local snapshot id mismatch: path=%q payload=%q", snapshotID, req.LocalSnapshot.SnapshotID)
		}
		snapshot, err := d.store.GetSnapshot(ctx, snapshotID)
		if err != nil {
			if err == store.ErrNotFound {
				return nil, func() {}, localSnapshotRestoreUnavailable(snapshotID, "snapshot is not present on this host")
			}
			return nil, func() {}, err
		}
		artifacts, err := localSnapshotArtifacts(snapshot)
		if err != nil {
			return nil, func() {}, localSnapshotRestoreUnavailable(snapshotID, err.Error())
		}
		return artifacts, func() {}, nil
	}

	if req.Snapshot == nil {
		return nil, func() {}, fmt.Errorf("durable snapshot spec is required")
	}
	stagingDir := filepath.Join(d.config.SnapshotsDir, string(snapshotID), "restores", string(req.MachineID))
	artifacts, err := downloadDurableSnapshotArtifacts(ctx, stagingDir, req.Snapshot.Artifacts)
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, func() {}, fmt.Errorf("download durable snapshot artifacts: %w", err)
	}
	return artifacts, func() {
		_ = os.RemoveAll(stagingDir)
	}, nil
}

func localSnapshotArtifacts(snapshot *model.SnapshotRecord) (map[string]restoredSnapshotArtifact, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot is required")
	}
	restored := make(map[string]restoredSnapshotArtifact, len(snapshot.Artifacts))
	for _, artifact := range snapshot.Artifacts {
		if strings.TrimSpace(artifact.LocalPath) == "" {
			return nil, fmt.Errorf("snapshot %q artifact %q is missing a local path", snapshot.ID, artifact.ID)
		}
		if _, err := os.Stat(artifact.LocalPath); err != nil {
			return nil, fmt.Errorf("snapshot %q artifact %q is unavailable at %q: %w", snapshot.ID, artifact.ID, artifact.LocalPath, err)
		}
		restored[artifact.Name] = restoredSnapshotArtifact{
			Artifact: contracthost.SnapshotArtifact{
				ID:        artifact.ID,
				Kind:      artifact.Kind,
				Name:      artifact.Name,
				SizeBytes: artifact.SizeBytes,
				SHA256Hex: artifact.SHA256Hex,
			},
			LocalPath: artifact.LocalPath,
		}
	}
	return restored, nil
}

func localSnapshotRestoreUnavailable(snapshotID contracthost.SnapshotID, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "local restore is unavailable"
	}
	return fmt.Errorf("%s: snapshot %q %s", localSnapshotRestoreUnavailablePrefix, snapshotID, message)
}
