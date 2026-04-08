package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		reconciled, err := d.reconcileMachine(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		machines = append(machines, machineToContract(*reconciled))
	}
	return &contracthost.ListMachinesResponse{Machines: machines}, nil
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
		switch operation.Type {
		case model.MachineOperationCreate:
			if err := d.reconcileCreate(ctx, operation.MachineID); err != nil {
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
		default:
			return fmt.Errorf("unsupported operation type %q", operation.Type)
		}
	}

	records, err := d.store.ListMachines(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if _, err := d.reconcileMachine(ctx, record.ID); err != nil {
			return err
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
		if record.ID == ignore || record.Phase != contracthost.MachinePhaseRunning {
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
	if record.Phase != contracthost.MachinePhaseRunning {
		return record, nil
	}

	state, err := d.runtime.Inspect(machineToRuntimeState(*record))
	if err != nil {
		return nil, err
	}
	if state.Phase == firecracker.PhaseRunning {
		return record, nil
	}

	if err := d.runtime.Delete(ctx, *state); err != nil {
		return nil, err
	}
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

func (d *Daemon) deleteMachineRecord(ctx context.Context, record *model.MachineRecord) error {
	if err := d.runtime.Delete(ctx, machineToRuntimeState(*record)); err != nil {
		return err
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
	if err := d.runtime.Delete(ctx, machineToRuntimeState(*record)); err != nil {
		return err
	}
	record.Phase = contracthost.MachinePhaseStopped
	record.Error = ""
	record.PID = 0
	record.SocketPath = ""
	record.RuntimeHost = ""
	record.TapDevice = ""
	record.StartedAt = nil
	return d.store.UpdateMachine(ctx, *record)
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
