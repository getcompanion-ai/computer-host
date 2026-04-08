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
	dialer := net.Dialer{Timeout: defaultGuestDialTimeout}
	ticker := time.NewTicker(defaultGuestReadyPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		connection, err := dialer.DialContext(ctx, string(port.Protocol), address)
		if err == nil {
			_ = connection.Close()
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
