package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const (
	minMachineSSHRelayPort = uint16(40000)
	maxMachineSSHRelayPort = uint16(44999)
	minMachineVNCRelayPort = uint16(45000)
	maxMachineVNCRelayPort = uint16(49999)
)

func machineRelayListenerKey(machineID contracthost.MachineID, name contracthost.MachinePortName) string {
	return string(machineID) + ":" + string(name)
}

func machineRelayHostPort(record model.MachineRecord, name contracthost.MachinePortName) uint16 {
	for _, port := range record.Ports {
		if port.Name == name {
			return port.HostPort
		}
	}
	return 0
}

func machineRelayGuestPort(record model.MachineRecord, name contracthost.MachinePortName) uint16 {
	for _, port := range record.Ports {
		if port.Name == name {
			return port.Port
		}
	}
	switch name {
	case contracthost.MachinePortNameVNC:
		return defaultVNCPort
	default:
		return defaultSSHPort
	}
}

func (d *Daemon) usedMachineRelayPorts(ctx context.Context, machineID contracthost.MachineID, name contracthost.MachinePortName) (map[uint16]struct{}, error) {
	records, err := d.store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}

	used := make(map[uint16]struct{}, len(records))
	for _, record := range records {
		if record.ID == machineID {
			continue
		}
		if record.Phase != contracthost.MachinePhaseRunning {
			continue
		}
		if port := machineRelayHostPort(record, name); port != 0 {
			used[port] = struct{}{}
		}
	}
	return used, nil
}

func (d *Daemon) allocateMachineRelayProxy(
	ctx context.Context,
	current model.MachineRecord,
	name contracthost.MachinePortName,
	runtimeHost string,
	guestPort uint16,
	minPort uint16,
	maxPort uint16,
) (uint16, error) {
	existingPort := machineRelayHostPort(current, name)
	if existingPort != 0 {
		if err := d.startMachineRelayProxy(current.ID, name, existingPort, runtimeHost, guestPort); err == nil {
			return existingPort, nil
		} else if !isAddrInUseError(err) {
			return 0, err
		}
	}

	used, err := d.usedMachineRelayPorts(ctx, current.ID, name)
	if err != nil {
		return 0, err
	}
	if existingPort != 0 {
		used[existingPort] = struct{}{}
	}
	for hostPort := minPort; hostPort <= maxPort; hostPort++ {
		if _, exists := used[hostPort]; exists {
			continue
		}
		if err := d.startMachineRelayProxy(current.ID, name, hostPort, runtimeHost, guestPort); err != nil {
			if isAddrInUseError(err) {
				continue
			}
			return 0, err
		}
		return hostPort, nil
	}
	return 0, fmt.Errorf("no relay ports are available in range %d-%d", minPort, maxPort)
}

func (d *Daemon) ensureMachineRelays(ctx context.Context, record *model.MachineRecord) error {
	if record == nil {
		return fmt.Errorf("machine record is required")
	}
	if record.Phase != contracthost.MachinePhaseRunning || strings.TrimSpace(record.RuntimeHost) == "" {
		return nil
	}

	d.relayAllocMu.Lock()
	sshRelayPort, err := d.allocateMachineRelayProxy(ctx, *record, contracthost.MachinePortNameSSH, record.RuntimeHost, machineRelayGuestPort(*record, contracthost.MachinePortNameSSH), minMachineSSHRelayPort, maxMachineSSHRelayPort)
	var vncRelayPort uint16
	if err == nil {
		vncRelayPort, err = d.allocateMachineRelayProxy(ctx, *record, contracthost.MachinePortNameVNC, record.RuntimeHost, machineRelayGuestPort(*record, contracthost.MachinePortNameVNC), minMachineVNCRelayPort, maxMachineVNCRelayPort)
	}
	d.relayAllocMu.Unlock()
	if err != nil {
		d.stopMachineRelays(record.ID)
		return err
	}

	record.Ports = buildMachinePorts(sshRelayPort, vncRelayPort)
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		d.stopMachineRelays(record.ID)
		return err
	}
	return nil
}

func (d *Daemon) startMachineRelayProxy(machineID contracthost.MachineID, name contracthost.MachinePortName, hostPort uint16, runtimeHost string, guestPort uint16) error {
	targetHost := strings.TrimSpace(runtimeHost)
	if targetHost == "" {
		return fmt.Errorf("runtime host is required for machine relay %q", machineID)
	}

	key := machineRelayListenerKey(machineID, name)

	d.machineRelaysMu.Lock()
	if _, exists := d.machineRelayListeners[key]; exists {
		d.machineRelaysMu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(int(hostPort)))
	if err != nil {
		d.machineRelaysMu.Unlock()
		return fmt.Errorf("listen on machine relay port %d: %w", hostPort, err)
	}
	d.machineRelayListeners[key] = listener
	d.machineRelaysMu.Unlock()

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(int(guestPort)))
	go serveTCPProxy(listener, targetAddr)
	return nil
}

func (d *Daemon) stopMachineRelayProxy(machineID contracthost.MachineID, name contracthost.MachinePortName) {
	key := machineRelayListenerKey(machineID, name)

	d.machineRelaysMu.Lock()
	listener, ok := d.machineRelayListeners[key]
	if ok {
		delete(d.machineRelayListeners, key)
	}
	d.machineRelaysMu.Unlock()
	if ok {
		_ = listener.Close()
	}
}

func (d *Daemon) stopMachineRelays(machineID contracthost.MachineID) {
	d.stopMachineRelayProxy(machineID, contracthost.MachinePortNameSSH)
	d.stopMachineRelayProxy(machineID, contracthost.MachinePortNameVNC)
}

func isAddrInUseError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}
