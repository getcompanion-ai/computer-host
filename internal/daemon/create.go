package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) CreateMachine(ctx context.Context, req contracthost.CreateMachineRequest) (*contracthost.CreateMachineResponse, error) {
	if err := validateMachineID(req.MachineID); err != nil {
		return nil, err
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
		MachineID: req.MachineID,
		Type:      model.MachineOperationCreate,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), req.MachineID)
		}
	}()

	var (
		artifact     *model.ArtifactRecord
		userVolumes  []model.VolumeRecord
		guestHostKey *guestSSHHostKeyPair
		readyNonce   string
	)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		artifact, err = d.ensureArtifact(groupCtx, req.Artifact)
		return err
	})
	group.Go(func() error {
		var err error
		userVolumes, err = d.loadAttachableUserVolumes(groupCtx, req.MachineID, req.UserVolumeIDs)
		return err
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
		return nil, err
	}

	systemVolumePath := d.systemVolumePath(req.MachineID)
	if err := os.MkdirAll(filepath.Dir(systemVolumePath), 0o755); err != nil {
		return nil, fmt.Errorf("create system volume dir for %q: %w", req.MachineID, err)
	}
	if err := cloneDiskFile(artifact.RootFSPath, systemVolumePath, d.config.DiskCloneMode); err != nil {
		return nil, fmt.Errorf("clone rootfs for %q: %w", req.MachineID, err)
	}
	removeSystemVolumeOnFailure := true
	defer func() {
		if !removeSystemVolumeOnFailure {
			return
		}
		_ = os.Remove(systemVolumePath)
		_ = os.RemoveAll(filepath.Dir(systemVolumePath))
	}()
	if err := os.Truncate(systemVolumePath, defaultGuestDiskSizeBytes); err != nil {
		return nil, fmt.Errorf("expand system volume for %q: %w", req.MachineID, err)
	}
	if err := d.injectMachineIdentity(ctx, systemVolumePath, req.MachineID); err != nil {
		return nil, fmt.Errorf("inject machine identity for %q: %w", req.MachineID, err)
	}
	if err := d.injectGuestConfig(ctx, systemVolumePath, guestConfig); err != nil {
		return nil, fmt.Errorf("inject guest config for %q: %w", req.MachineID, err)
	}
	if err := injectGuestSSHHostKey(ctx, systemVolumePath, guestHostKey); err != nil {
		return nil, fmt.Errorf("inject guest ssh host key for %q: %w", req.MachineID, err)
	}

	spec, err := d.buildMachineSpec(req.MachineID, artifact, userVolumes, systemVolumePath, guestConfig, readyNonce)
	if err != nil {
		return nil, err
	}
	usedNetworks, err := d.listRunningNetworks(ctx, req.MachineID)
	if err != nil {
		return nil, err
	}

	state, err := d.runtime.Boot(ctx, spec, usedNetworks)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	systemVolumeRecord := model.VolumeRecord{
		ID:                d.systemVolumeID(req.MachineID),
		Kind:              contracthost.VolumeKindSystem,
		AttachedMachineID: machineIDPtr(req.MachineID),
		SourceArtifact:    &req.Artifact,
		Pool:              model.StoragePoolMachineDisks,
		Path:              systemVolumePath,
		CreatedAt:         now,
	}
	if err := d.store.CreateVolume(ctx, systemVolumeRecord); err != nil {
		_ = d.runtime.Delete(context.Background(), *state)
		return nil, err
	}

	attachedUserVolumeIDs := make([]contracthost.VolumeID, 0, len(userVolumes))
	for _, volume := range userVolumes {
		volume.AttachedMachineID = machineIDPtr(req.MachineID)
		if err := d.store.UpdateVolume(ctx, volume); err != nil {
			for _, attachedVolumeID := range attachedUserVolumeIDs {
				attachedVolume, getErr := d.store.GetVolume(context.Background(), attachedVolumeID)
				if getErr == nil {
					attachedVolume.AttachedMachineID = nil
					_ = d.store.UpdateVolume(context.Background(), *attachedVolume)
				}
			}
			_ = d.store.DeleteVolume(context.Background(), systemVolumeRecord.ID)
			_ = d.runtime.Delete(context.Background(), *state)
			return nil, err
		}
		attachedUserVolumeIDs = append(attachedUserVolumeIDs, volume.ID)
	}

	record := model.MachineRecord{
		ID:                req.MachineID,
		Artifact:          req.Artifact,
		GuestConfig:       cloneGuestConfig(guestConfig),
		SystemVolumeID:    systemVolumeRecord.ID,
		UserVolumeIDs:     append([]contracthost.VolumeID(nil), attachedUserVolumeIDs...),
		RuntimeHost:       state.RuntimeHost,
		TapDevice:         state.TapName,
		Ports:             defaultMachinePorts(),
		GuestSSHPublicKey: strings.TrimSpace(guestHostKey.PublicKey),
		GuestReadyNonce:   readyNonce,
		Phase:             contracthost.MachinePhaseStarting,
		PID:               state.PID,
		SocketPath:        state.SocketPath,
		CreatedAt:         now,
		StartedAt:         state.StartedAt,
	}
	if err := d.store.CreateMachine(ctx, record); err != nil {
		for _, volume := range userVolumes {
			volume.AttachedMachineID = nil
			_ = d.store.UpdateVolume(context.Background(), volume)
		}
		_ = d.store.DeleteVolume(context.Background(), systemVolumeRecord.ID)
		_ = d.runtime.Delete(context.Background(), *state)
		return nil, err
	}

	recordReady, err := d.completeMachineStartup(ctx, &record, *state)
	if err != nil {
		return nil, err
	}

	removeSystemVolumeOnFailure = false
	clearOperation = true
	return &contracthost.CreateMachineResponse{Machine: machineToContract(*recordReady)}, nil
}

func (d *Daemon) buildMachineSpec(machineID contracthost.MachineID, artifact *model.ArtifactRecord, userVolumes []model.VolumeRecord, systemVolumePath string, guestConfig *contracthost.GuestConfig, readyNonce string) (firecracker.MachineSpec, error) {
	drives := make([]firecracker.DriveSpec, 0, len(userVolumes))
	for i, volume := range userVolumes {
		drives = append(drives, firecracker.DriveSpec{
			ID:        fmt.Sprintf("user-%d", i),
			Path:      volume.Path,
			ReadOnly:  false,
			CacheType: firecracker.DriveCacheTypeUnsafe,
			IOEngine:  d.config.DriveIOEngine,
		})
	}

	mmds, err := d.guestMetadataSpec(machineID, guestConfig, readyNonce)
	if err != nil {
		return firecracker.MachineSpec{}, err
	}
	spec := firecracker.MachineSpec{
		ID:              firecracker.MachineID(machineID),
		VCPUs:           defaultGuestVCPUs,
		MemoryMiB:       defaultGuestMemoryMiB,
		KernelImagePath: artifact.KernelImagePath,
		RootFSPath:      systemVolumePath,
		RootDrive: firecracker.DriveSpec{
			ID:        "root_drive",
			Path:      systemVolumePath,
			CacheType: firecracker.DriveCacheTypeUnsafe,
			IOEngine:  d.config.DriveIOEngine,
		},
		KernelArgs: guestKernelArgs(d.config.EnablePCI),
		Drives:     drives,
		MMDS:       mmds,
		Vsock:      guestVsockSpec(machineID),
	}
	if err := spec.Validate(); err != nil {
		return firecracker.MachineSpec{}, err
	}
	return spec, nil
}

func (d *Daemon) ensureArtifact(ctx context.Context, ref contracthost.ArtifactRef) (*model.ArtifactRecord, error) {
	key := artifactKey(ref)
	unlock := d.lockArtifact(key)
	defer unlock()

	if artifact, err := d.store.GetArtifact(ctx, ref); err == nil {
		return artifact, nil
	} else if err != store.ErrNotFound {
		return nil, err
	}

	dir := filepath.Join(d.config.ArtifactsDir, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact dir %q: %w", dir, err)
	}

	kernelPath := filepath.Join(dir, "kernel")
	rootFSPath := filepath.Join(dir, "rootfs")
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return downloadFile(groupCtx, ref.KernelImageURL, kernelPath)
	})
	group.Go(func() error {
		return downloadFile(groupCtx, ref.RootFSURL, rootFSPath)
	})
	if err := group.Wait(); err != nil {
		return nil, err
	}

	artifact := model.ArtifactRecord{
		Ref:             ref,
		LocalKey:        key,
		LocalDir:        dir,
		KernelImagePath: kernelPath,
		RootFSPath:      rootFSPath,
		CreatedAt:       time.Now().UTC(),
	}
	if err := d.store.PutArtifact(ctx, artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (d *Daemon) loadAttachableUserVolumes(ctx context.Context, machineID contracthost.MachineID, volumeIDs []contracthost.VolumeID) ([]model.VolumeRecord, error) {
	volumes := make([]model.VolumeRecord, 0, len(volumeIDs))
	for _, volumeID := range volumeIDs {
		volume, err := d.store.GetVolume(ctx, volumeID)
		if err != nil {
			return nil, err
		}
		if volume.Kind != contracthost.VolumeKindUser {
			return nil, fmt.Errorf("volume %q is not a user volume", volumeID)
		}
		if volume.AttachedMachineID != nil && *volume.AttachedMachineID != machineID {
			return nil, fmt.Errorf("volume %q is already attached to machine %q", volumeID, *volume.AttachedMachineID)
		}
		volumes = append(volumes, *volume)
	}
	return volumes, nil
}
