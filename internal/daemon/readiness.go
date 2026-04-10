package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func guestPortsReady(ctx context.Context, host string, ports []contracthost.MachinePort) (bool, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return false, fmt.Errorf("guest runtime host is required")
	}

	for _, port := range ports {
		probeCtx, cancel := context.WithTimeout(ctx, defaultGuestDialTimeout)
		ready, err := guestPortReady(probeCtx, host, port)
		cancel()
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}

func guestPortReady(ctx context.Context, host string, port contracthost.MachinePort) (bool, error) {
	address := net.JoinHostPort(host, strconv.Itoa(int(port.Port)))
	dialer := net.Dialer{Timeout: defaultGuestDialTimeout}

	connection, err := dialer.DialContext(ctx, string(port.Protocol), address)
	if err == nil {
		_ = connection.Close()
		return true, nil
	}
	if ctx.Err() != nil {
		return false, nil
	}
	return false, nil
}
