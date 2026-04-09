package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const (
	minPublishedHostPort = uint16(20000)
	maxPublishedHostPort = uint16(39999)
)

func (d *Daemon) CreatePublishedPort(ctx context.Context, machineID contracthost.MachineID, req contracthost.CreatePublishedPortRequest) (*contracthost.CreatePublishedPortResponse, error) {
	if strings.TrimSpace(string(req.PublishedPortID)) == "" {
		return nil, fmt.Errorf("published_port_id is required")
	}
	if req.Port == 0 {
		return nil, fmt.Errorf("port must be greater than zero")
	}
	if req.Protocol == "" {
		req.Protocol = contracthost.PortProtocolTCP
	}
	if req.Protocol != contracthost.PortProtocolTCP {
		return nil, fmt.Errorf("unsupported protocol %q", req.Protocol)
	}

	unlock := d.lockMachine(machineID)
	defer unlock()

	record, err := d.store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if record.Phase != contracthost.MachinePhaseRunning {
		return nil, fmt.Errorf("machine %q is not running", machineID)
	}
	if _, err := d.store.GetPublishedPort(ctx, req.PublishedPortID); err == nil {
		return nil, fmt.Errorf("published port %q already exists", req.PublishedPortID)
	} else if err != nil && err != store.ErrNotFound {
		return nil, err
	}

	d.publishedPortAllocMu.Lock()
	defer d.publishedPortAllocMu.Unlock()

	hostPort, err := d.allocatePublishedHostPort(ctx)
	if err != nil {
		return nil, err
	}

	published := model.PublishedPortRecord{
		ID:        req.PublishedPortID,
		MachineID: machineID,
		Name:      strings.TrimSpace(req.Name),
		Port:      req.Port,
		HostPort:  hostPort,
		Protocol:  req.Protocol,
		CreatedAt: time.Now().UTC(),
	}
	if err := d.startPublishedPortProxy(published, record.RuntimeHost); err != nil {
		return nil, err
	}
	storeCreated := false
	defer func() {
		if !storeCreated {
			d.stopPublishedPortProxy(req.PublishedPortID)
		}
	}()

	if err := d.store.CreatePublishedPort(ctx, published); err != nil {
		return nil, err
	}
	storeCreated = true
	return &contracthost.CreatePublishedPortResponse{Port: publishedPortToContract(published)}, nil
}

func (d *Daemon) ListPublishedPorts(ctx context.Context, machineID contracthost.MachineID) (*contracthost.ListPublishedPortsResponse, error) {
	ports, err := d.store.ListPublishedPorts(ctx, machineID)
	if err != nil {
		return nil, err
	}
	response := &contracthost.ListPublishedPortsResponse{Ports: make([]contracthost.PublishedPort, 0, len(ports))}
	for _, port := range ports {
		response.Ports = append(response.Ports, publishedPortToContract(port))
	}
	return response, nil
}

func (d *Daemon) DeletePublishedPort(ctx context.Context, machineID contracthost.MachineID, portID contracthost.PublishedPortID) error {
	unlock := d.lockMachine(machineID)
	defer unlock()

	record, err := d.store.GetPublishedPort(ctx, portID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}
	if record.MachineID != machineID {
		return fmt.Errorf("published port %q does not belong to machine %q", portID, machineID)
	}
	d.stopPublishedPortProxy(portID)
	return d.store.DeletePublishedPort(ctx, portID)
}

func (d *Daemon) ensurePublishedPortsForMachine(ctx context.Context, machine model.MachineRecord) error {
	if machine.Phase != contracthost.MachinePhaseRunning || strings.TrimSpace(machine.RuntimeHost) == "" {
		return nil
	}
	ports, err := d.store.ListPublishedPorts(ctx, machine.ID)
	if err != nil {
		return err
	}
	for _, port := range ports {
		if err := d.startPublishedPortProxy(port, machine.RuntimeHost); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) stopPublishedPortsForMachine(machineID contracthost.MachineID) {
	ports, err := d.store.ListPublishedPorts(context.Background(), machineID)
	if err != nil {
		return
	}
	for _, port := range ports {
		d.stopPublishedPortProxy(port.ID)
	}
}

func (d *Daemon) allocatePublishedHostPort(ctx context.Context) (uint16, error) {
	ports, err := d.store.ListPublishedPorts(ctx, "")
	if err != nil {
		return 0, err
	}
	used := make(map[uint16]struct{}, len(ports))
	for _, port := range ports {
		used[port.HostPort] = struct{}{}
	}
	for hostPort := minPublishedHostPort; hostPort <= maxPublishedHostPort; hostPort++ {
		if _, exists := used[hostPort]; exists {
			continue
		}
		return hostPort, nil
	}
	return 0, fmt.Errorf("no published host ports are available")
}

func (d *Daemon) startPublishedPortProxy(port model.PublishedPortRecord, runtimeHost string) error {
	targetHost := strings.TrimSpace(runtimeHost)
	if targetHost == "" {
		return fmt.Errorf("runtime host is required for published port %q", port.ID)
	}

	d.publishedPortsMu.Lock()
	if _, exists := d.publishedPortListeners[port.ID]; exists {
		d.publishedPortsMu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(int(port.HostPort)))
	if err != nil {
		d.publishedPortsMu.Unlock()
		return fmt.Errorf("listen on host port %d: %w", port.HostPort, err)
	}
	d.publishedPortListeners[port.ID] = listener
	d.publishedPortsMu.Unlock()

	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(int(port.Port)))
	go d.servePublishedPortProxy(port.ID, listener, targetAddr)
	return nil
}

func (d *Daemon) servePublishedPortProxy(portID contracthost.PublishedPortID, listener net.Listener, targetAddr string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if isClosedNetworkError(err) {
				return
			}
			continue
		}
		go proxyPublishedPortConnection(conn, targetAddr)
	}
}

func proxyPublishedPortConnection(source net.Conn, targetAddr string) {
	defer func() {
		_ = source.Close()
	}()

	target, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer func() {
		_ = target.Close()
	}()

	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(target, source)
		closeWrite(target)
		done <- struct{}{}
	}()

	_, _ = io.Copy(source, target)
	closeWrite(source)
	<-done
}

func closeWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if value, ok := conn.(closeWriter); ok {
		_ = value.CloseWrite()
	}
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "use of closed network connection")
}

func (d *Daemon) stopPublishedPortProxy(portID contracthost.PublishedPortID) {
	d.publishedPortsMu.Lock()
	listener, ok := d.publishedPortListeners[portID]
	if ok {
		delete(d.publishedPortListeners, portID)
	}
	d.publishedPortsMu.Unlock()
	if ok {
		_ = listener.Close()
	}
}
