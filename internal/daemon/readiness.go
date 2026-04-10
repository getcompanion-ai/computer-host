package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func waitForGuestReady(ctx context.Context, host string, ports []contracthost.MachinePort) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("guest runtime host is required")
	}

	waitContext, cancel := context.WithTimeout(ctx, defaultGuestReadyTimeout)
	defer cancel()

	for _, port := range ports {
		if err := waitForGuestPort(waitContext, host, port); err != nil {
			return err
		}
	}
	return nil
}

func waitForGuestPort(ctx context.Context, host string, port contracthost.MachinePort) error {
	address := net.JoinHostPort(host, strconv.Itoa(int(port.Port)))
	ticker := time.NewTicker(defaultGuestReadyPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, defaultGuestDialTimeout)
		ready, err := guestPortReady(probeCtx, host, port)
		cancel()
		if err == nil && ready {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for guest port %q on %s: %w (last_err=%v)", port.Name, address, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

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
