package daemon

import (
	"context"
	"fmt"
	"strings"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) EnsureExecRelay(ctx context.Context, id contracthost.MachineID) (*contracthost.GetMachineResponse, error) {
	unlock := d.lockMachine(id)
	defer unlock()

	record, err := d.store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.Phase != contracthost.MachinePhaseRunning {
		return nil, fmt.Errorf("machine %q is not running", id)
	}
	if strings.TrimSpace(record.RuntimeHost) == "" {
		return nil, fmt.Errorf("machine %q runtime host is unavailable", id)
	}

	d.relayAllocMu.Lock()
	execRelayPort, err := d.allocateMachineRelayProxy(
		ctx,
		*record,
		contracthost.MachinePortNameExec,
		record.RuntimeHost,
		defaultGuestdPort,
		minMachineExecRelayPort,
		maxMachineExecRelayPort,
	)
	d.relayAllocMu.Unlock()
	if err != nil {
		d.stopMachineRelayProxy(record.ID, contracthost.MachinePortNameExec)
		return nil, err
	}

	record.Ports = setMachineExecRelayPort(record.Ports, execRelayPort)
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		d.stopMachineRelayProxy(record.ID, contracthost.MachinePortNameExec)
		return nil, err
	}
	return &contracthost.GetMachineResponse{Machine: machineToContract(*record)}, nil
}
