package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) GetMachine(ctx context.Context, id contracthost.MachineID) (*contracthost.GetMachineResponse, error) {
	record, err := d.reconcileMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	return &contracthost.GetMachineResponse{Machine: machineToContract(*record)}, nil
}

func (d *Daemon) ListMachines(ctx context.Context) (*contracthost.ListMachinesResponse, error) {
	records, err := d.store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}

	machines := make([]contracthost.Machine, 0, len(records))
	for _, record := range records {
		machines = append(machines, machineToContract(record))
	}
	return &contracthost.ListMachinesResponse{Machines: machines}, nil
}

func (d *Daemon) StartMachine(ctx context.Context, id contracthost.MachineID) (*contracthost.GetMachineResponse, error) {
	unlock := d.lockMachine(id)
	defer unlock()

	record, err := d.store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	previousRecord := *record
	if record.Phase == contracthost.MachinePhaseRunning {
		return &contracthost.GetMachineResponse{Machine: machineToContract(*record)}, nil
	}
	if record.Phase == contracthost.MachinePhaseStarting {
		// reconcileMachine acquires the machine lock, so we must release
		// ours first to avoid self-deadlock.
		unlock()
		reconciled, err := d.reconcileMachine(ctx, id)
		if err != nil {
			return nil, err
		}
		return &contracthost.GetMachineResponse{Machine: machineToContract(*reconciled)}, nil
	}
	if record.Phase != contracthost.MachinePhaseStopped {
		return nil, fmt.Errorf("machine %q is not startable from phase %q", id, record.Phase)
	}

	if err := d.store.UpsertOperation(ctx, model.OperationRecord{
		MachineID: id,
		Type:      model.MachineOperationStart,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), id)
		}
	}()

	var (
		systemVolume *model.VolumeRecord
		artifact     *model.ArtifactRecord
		userVolumes  []model.VolumeRecord
		readyNonce   string
	)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		systemVolume, err = d.store.GetVolume(groupCtx, record.SystemVolumeID)
		return err
	})
	group.Go(func() error {
		var err error
		artifact, err = d.store.GetArtifact(groupCtx, record.Artifact)
		return err
	})
	group.Go(func() error {
		var err error
		userVolumes, err = d.loadAttachableUserVolumes(groupCtx, id, record.UserVolumeIDs)
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
	repairDirtyFilesystem(systemVolume.Path)
	spec, err := d.buildMachineSpec(id, artifact, userVolumes, systemVolume.Path, record.GuestConfig, readyNonce)
	if err != nil {
		return nil, err
	}
	usedNetworks, err := d.listRunningNetworks(ctx, id)
	if err != nil {
		return nil, err
	}
	state, err := d.runtime.Boot(ctx, spec, usedNetworks)
	if err != nil {
		return nil, err
	}
	record.RuntimeHost = state.RuntimeHost
	record.TapDevice = state.TapName
	record.Ports = defaultMachinePorts()
	record.GuestReadyNonce = readyNonce
	record.Phase = contracthost.MachinePhaseStarting
	record.Error = ""
	record.PID = state.PID
	record.SocketPath = state.SocketPath
	record.StartedAt = state.StartedAt
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		_ = d.runtime.Delete(context.Background(), *state)
		_ = d.store.UpdateMachine(context.Background(), previousRecord)
		return nil, err
	}

	record, err = d.completeMachineStartup(ctx, record, *state)
	if err != nil {
		return nil, err
	}

	clearOperation = true
	return &contracthost.GetMachineResponse{Machine: machineToContract(*record)}, nil
}

func (d *Daemon) StopMachine(ctx context.Context, id contracthost.MachineID) error {
	unlock := d.lockMachine(id)
	defer unlock()

	record, err := d.store.GetMachine(ctx, id)
	if err != nil {
		return err
	}
	if record.Phase == contracthost.MachinePhaseStopped {
		return nil
	}

	if err := d.store.UpsertOperation(ctx, model.OperationRecord{
		MachineID: id,
		Type:      model.MachineOperationStop,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), id)
		}
	}()

	if err := d.stopMachineRecord(ctx, record); err != nil {
		return err
	}

	clearOperation = true
	return nil
}

func (d *Daemon) DeleteMachine(ctx context.Context, id contracthost.MachineID) error {
	unlock := d.lockMachine(id)
	defer unlock()

	record, err := d.store.GetMachine(ctx, id)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}

	if err := d.store.UpsertOperation(ctx, model.OperationRecord{
		MachineID: id,
		Type:      model.MachineOperationDelete,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	clearOperation := false
	defer func() {
		if clearOperation {
			_ = d.store.DeleteOperation(context.Background(), id)
		}
	}()

	if err := d.deleteMachineRecord(ctx, record); err != nil {
		return err
	}

	clearOperation = true
	return nil
}

func (d *Daemon) Reconcile(ctx context.Context) error {
	operations, err := d.store.ListOperations(ctx)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		unlock, ok := d.tryLockMachine(operation.MachineID)
		if !ok {
			continue
		}
		unlock()

		switch operation.Type {
		case model.MachineOperationCreate:
			if err := d.reconcileCreate(ctx, operation.MachineID); err != nil {
				return err
			}
		case model.MachineOperationStart:
			if err := d.reconcileStart(ctx, operation.MachineID); err != nil {
				return err
			}
		case model.MachineOperationStop:
			if err := d.reconcileStop(ctx, operation.MachineID); err != nil {
				return err
			}
		case model.MachineOperationDelete:
			if err := d.reconcileDelete(ctx, operation.MachineID); err != nil {
				return err
			}
		case model.MachineOperationSnapshot:
			if err := d.reconcileSnapshot(ctx, operation); err != nil {
				return err
			}
		case model.MachineOperationRestore:
			if err := d.reconcileRestore(ctx, operation); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported operation type %q", operation.Type)
		}
	}

	records, err := d.store.ListMachines(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		reconciled, err := d.reconcileMachine(ctx, record.ID)
		if err != nil {
			return err
		}
		if reconciled.Phase == contracthost.MachinePhaseRunning {
			if err := d.ensureMachineRelays(ctx, reconciled); err != nil {
				return err
			}
			if err := d.ensurePublishedPortsForMachine(ctx, *reconciled); err != nil {
				return err
			}
		} else {
			d.stopMachineRelays(reconciled.ID)
			d.stopPublishedPortsForMachine(reconciled.ID)
		}
	}
	return nil
}

func (d *Daemon) listRunningNetworks(ctx context.Context, ignore contracthost.MachineID) ([]firecracker.NetworkAllocation, error) {
	records, err := d.store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}

	networks := make([]firecracker.NetworkAllocation, 0, len(records))
	for _, record := range records {
		if record.ID == ignore {
			continue
		}
		if record.Phase != contracthost.MachinePhaseRunning && record.Phase != contracthost.MachinePhaseStarting {
			continue
		}
		if strings.TrimSpace(record.RuntimeHost) == "" || strings.TrimSpace(record.TapDevice) == "" {
			continue
		}
		network, err := firecracker.AllocationFromGuestIP(record.RuntimeHost, record.TapDevice)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func (d *Daemon) reconcileCreate(ctx context.Context, machineID contracthost.MachineID) error {
	_, err := d.store.GetMachine(ctx, machineID)
	if err == nil {
		if _, err := d.reconcileMachine(ctx, machineID); err != nil {
			return err
		}
		return d.store.DeleteOperation(ctx, machineID)
	}
	if err != store.ErrNotFound {
		return err
	}

	if err := os.Remove(d.systemVolumePath(machineID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup system volume for %q: %w", machineID, err)
	}
	if err := d.store.DeleteVolume(ctx, d.systemVolumeID(machineID)); err != nil && err != store.ErrNotFound {
		return err
	}
	if err := d.detachVolumesForMachine(ctx, machineID); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Dir(d.systemVolumePath(machineID)))
	if err := os.RemoveAll(d.machineRuntimeBaseDir(machineID)); err != nil {
		return fmt.Errorf("cleanup runtime dir for %q: %w", machineID, err)
	}
	return d.store.DeleteOperation(ctx, machineID)
}

func (d *Daemon) reconcileStop(ctx context.Context, machineID contracthost.MachineID) error {
	record, err := d.store.GetMachine(ctx, machineID)
	if err == store.ErrNotFound {
		return d.store.DeleteOperation(ctx, machineID)
	}
	if err != nil {
		return err
	}
	if err := d.stopMachineRecord(ctx, record); err != nil {
		return err
	}
	return d.store.DeleteOperation(ctx, machineID)
}

func (d *Daemon) reconcileStart(ctx context.Context, machineID contracthost.MachineID) error {
	record, err := d.store.GetMachine(ctx, machineID)
	if err == store.ErrNotFound {
		return d.store.DeleteOperation(ctx, machineID)
	}
	if err != nil {
		return err
	}
	if record.Phase == contracthost.MachinePhaseRunning || record.Phase == contracthost.MachinePhaseStarting {
		if _, err := d.reconcileMachine(ctx, machineID); err != nil {
			return err
		}
		return d.store.DeleteOperation(ctx, machineID)
	}
	if _, err := d.StartMachine(ctx, machineID); err != nil {
		return err
	}
	return d.store.DeleteOperation(ctx, machineID)
}

func (d *Daemon) reconcileDelete(ctx context.Context, machineID contracthost.MachineID) error {
	record, err := d.store.GetMachine(ctx, machineID)
	if err == store.ErrNotFound {
		if err := os.Remove(d.systemVolumePath(machineID)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := d.store.DeleteVolume(ctx, d.systemVolumeID(machineID)); err != nil && err != store.ErrNotFound {
			return err
		}
		if err := d.detachVolumesForMachine(ctx, machineID); err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Dir(d.systemVolumePath(machineID)))
		_ = os.RemoveAll(d.machineRuntimeBaseDir(machineID))
		return d.store.DeleteOperation(ctx, machineID)
	}
	if err != nil {
		return err
	}
	if err := d.deleteMachineRecord(ctx, record); err != nil {
		return err
	}
	return d.store.DeleteOperation(ctx, machineID)
}

func (d *Daemon) reconcileMachine(ctx context.Context, machineID contracthost.MachineID) (*model.MachineRecord, error) {
	unlock := d.lockMachine(machineID)
	defer unlock()

	record, err := d.store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if record.Phase != contracthost.MachinePhaseRunning && record.Phase != contracthost.MachinePhaseStarting {
		return record, nil
	}

	state, err := d.runtime.Inspect(machineToRuntimeState(*record))
	if err != nil {
		return nil, err
	}
	if record.Phase == contracthost.MachinePhaseStarting {
		return d.completeMachineStartup(ctx, record, *state)
	}
	if state.Phase == firecracker.PhaseRunning {
		if err := d.ensureMachineRelays(ctx, record); err != nil {
			return nil, err
		}
		return record, nil
	}

	if err := d.runtime.Delete(ctx, *state); err != nil {
		return nil, err
	}
	d.stopMachineRelays(record.ID)
	d.stopPublishedPortsForMachine(record.ID)
	record.Phase = contracthost.MachinePhaseFailed
	record.Error = state.Error
	record.PID = 0
	record.SocketPath = ""
	record.RuntimeHost = ""
	record.TapDevice = ""
	record.StartedAt = nil
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		return nil, err
	}
	return record, nil
}

func (d *Daemon) failMachineStartup(ctx context.Context, record *model.MachineRecord, failureReason string) (*model.MachineRecord, error) {
	if record == nil {
		return nil, fmt.Errorf("machine record is required")
	}
	_ = d.runtime.Delete(ctx, machineToRuntimeState(*record))
	d.stopMachineRelays(record.ID)
	d.stopPublishedPortsForMachine(record.ID)
	record.Phase = contracthost.MachinePhaseFailed
	record.Error = strings.TrimSpace(failureReason)
	record.Ports = defaultMachinePorts()
	record.GuestReadyNonce = ""
	record.PID = 0
	record.SocketPath = ""
	record.RuntimeHost = ""
	record.TapDevice = ""
	record.StartedAt = nil
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		return nil, err
	}
	return record, nil
}

func (d *Daemon) deleteMachineRecord(ctx context.Context, record *model.MachineRecord) error {
	d.stopMachineRelays(record.ID)
	d.stopPublishedPortsForMachine(record.ID)
	if err := d.runtime.Delete(ctx, machineToRuntimeState(*record)); err != nil {
		return err
	}
	if ports, err := d.store.ListPublishedPorts(ctx, record.ID); err == nil {
		for _, port := range ports {
			_ = d.store.DeletePublishedPort(ctx, port.ID)
		}
	}
	if err := d.detachVolumesForMachine(ctx, record.ID); err != nil {
		return err
	}

	systemVolume, err := d.store.GetVolume(ctx, record.SystemVolumeID)
	if err != nil {
		return err
	}
	if err := os.Remove(systemVolume.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove system volume %q: %w", systemVolume.Path, err)
	}
	if err := os.RemoveAll(filepath.Dir(systemVolume.Path)); err != nil {
		return fmt.Errorf("remove machine disk dir %q: %w", filepath.Dir(systemVolume.Path), err)
	}
	if err := d.store.DeleteVolume(ctx, record.SystemVolumeID); err != nil {
		return err
	}
	return d.store.DeleteMachine(ctx, record.ID)
}

func (d *Daemon) stopMachineRecord(ctx context.Context, record *model.MachineRecord) error {
	d.stopMachineRelays(record.ID)
	d.stopPublishedPortsForMachine(record.ID)

	if record.Phase == contracthost.MachinePhaseRunning && strings.TrimSpace(record.RuntimeHost) != "" {
		if err := d.syncGuestFilesystem(ctx, record.RuntimeHost); err != nil {
			fmt.Fprintf(os.Stderr, "warning: guest filesystem sync for %q failed: %v\n", record.ID, err)
		}
		d.shutdownGuestClean(ctx, record)
	}
	// Always call runtime.Delete: it cleans up the TAP device, runtime
	// directory, and process (no-op if the process already exited).
	if err := d.runtime.Delete(ctx, machineToRuntimeState(*record)); err != nil {
		return err
	}

	record.Phase = contracthost.MachinePhaseStopped
	record.Error = ""
	record.GuestReadyNonce = ""
	record.PID = 0
	record.SocketPath = ""
	record.RuntimeHost = ""
	record.TapDevice = ""
	record.StartedAt = nil
	return d.store.UpdateMachine(ctx, *record)
}

// shutdownGuestClean attempts an in-guest poweroff and waits for the
// Firecracker process to exit within the stop timeout. Returns true if the
// process exited cleanly, false if a forced teardown is needed.
func (d *Daemon) shutdownGuestClean(ctx context.Context, record *model.MachineRecord) bool {
	shutdownCtx, cancel := context.WithTimeout(ctx, defaultGuestStopTimeout)
	defer cancel()

	if err := d.shutdownGuest(shutdownCtx, record.RuntimeHost); err != nil {
		if ctx.Err() != nil {
			return false
		}
		if shutdownCtx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "warning: guest poweroff for %q failed: %v\n", record.ID, err)
			return false
		}
		fmt.Fprintf(os.Stderr, "warning: guest poweroff for %q timed out before confirmation; checking whether shutdown is already in progress: %v\n", record.ID, err)
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := d.runtime.Inspect(machineToRuntimeState(*record))
		if err != nil {
			return false
		}
		if state.Phase != firecracker.PhaseRunning {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-shutdownCtx.Done():
			if ctx.Err() != nil {
				return false
			}
			fmt.Fprintf(os.Stderr, "warning: guest %q did not exit within stop window; forcing teardown\n", record.ID)
			return false
		case <-ticker.C:
		}
	}
}

func (d *Daemon) issueGuestPoweroff(ctx context.Context, runtimeHost string) error {
	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-i", d.backendSSHPrivateKeyPath(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-p", fmt.Sprintf("%d", defaultSSHPort),
		"node@"+runtimeHost,
		"sudo poweroff",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// poweroff may kill the SSH session before sending exit status;
		// this is expected and not a real error.
		if ctx.Err() == nil && strings.Contains(string(output), "closed by remote host") {
			return nil
		}
		// Also treat exit status 255 (SSH disconnect) as success.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
			return nil
		}
		return fmt.Errorf("issue guest poweroff: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *Daemon) detachVolumesForMachine(ctx context.Context, machineID contracthost.MachineID) error {
	volumes, err := d.store.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if volume.AttachedMachineID == nil || *volume.AttachedMachineID != machineID {
			continue
		}
		volume.AttachedMachineID = nil
		if err := d.store.UpdateVolume(ctx, volume); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) reconcileSnapshot(ctx context.Context, operation model.OperationRecord) error {
	if operation.SnapshotID == nil {
		return d.store.DeleteOperation(ctx, operation.MachineID)
	}
	_, err := d.store.GetSnapshot(ctx, *operation.SnapshotID)
	if err == nil {
		// Snapshot completed successfully, just clear the journal
		return d.store.DeleteOperation(ctx, operation.MachineID)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get snapshot %q during reconciliation: %w", *operation.SnapshotID, err)
	}
	// Snapshot did not complete: clean up partial snapshot directory and resume the machine
	snapshotDir := filepath.Join(d.config.SnapshotsDir, string(*operation.SnapshotID))
	_ = os.RemoveAll(snapshotDir)

	// Try to resume the source machine in case it was left paused
	record, err := d.store.GetMachine(ctx, operation.MachineID)
	if err == nil && record.Phase == contracthost.MachinePhaseRunning && record.PID > 0 {
		_ = d.runtime.Resume(ctx, machineToRuntimeState(*record))
	}
	return d.store.DeleteOperation(ctx, operation.MachineID)
}

func (d *Daemon) reconcileRestore(ctx context.Context, operation model.OperationRecord) error {
	_, err := d.store.GetMachine(ctx, operation.MachineID)
	if err == nil {
		// Restore completed, clear journal
		return d.store.DeleteOperation(ctx, operation.MachineID)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get machine %q during restore reconciliation: %w", operation.MachineID, err)
	}
	// Restore did not complete: clean up partial machine directory and disk
	_ = os.RemoveAll(filepath.Dir(d.systemVolumePath(operation.MachineID)))
	_ = os.RemoveAll(d.machineRuntimeBaseDir(operation.MachineID))
	return d.store.DeleteOperation(ctx, operation.MachineID)
}
