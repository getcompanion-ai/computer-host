package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
)

const (
	defaultGuestPersonalizationVsockID   = "microagent-personalizer"
	defaultGuestPersonalizationVsockName = "microagent-personalizer.vsock"
	defaultGuestPersonalizationVsockPort = uint32(1024)
	defaultGuestPersonalizationTimeout   = 2 * time.Second
	minGuestVsockCID                     = uint32(3)
	maxGuestVsockCID                     = uint32(1<<31 - 1)
)

func guestVsockSpec(machineID contracthost.MachineID) *firecracker.VsockSpec {
	return &firecracker.VsockSpec{
		ID:   defaultGuestPersonalizationVsockID,
		CID:  guestVsockCID(machineID),
		Path: defaultGuestPersonalizationVsockName,
	}
}

func guestVsockCID(machineID contracthost.MachineID) uint32 {
	sum := sha256.Sum256([]byte(machineID))
	space := maxGuestVsockCID - minGuestVsockCID + 1
	return minGuestVsockCID + binary.BigEndian.Uint32(sum[:4])%space
}

func (d *Daemon) personalizeGuestConfig(ctx context.Context, record *model.MachineRecord, state firecracker.MachineState) error {
	if record == nil {
		return fmt.Errorf("machine record is required")
	}

	personalizeCtx, cancel := context.WithTimeout(ctx, defaultGuestPersonalizationTimeout)
	defer cancel()

	mmds, err := d.guestMetadataSpec(record.ID, record.GuestConfig)
	if err != nil {
		return err
	}
	envelope, ok := mmds.Data.(guestMetadataEnvelope)
	if !ok {
		return fmt.Errorf("guest metadata payload has unexpected type %T", mmds.Data)
	}

	if err := d.runtime.PutMMDS(personalizeCtx, state, mmds.Data); err != nil {
		return d.personalizeGuestConfigViaSSH(ctx, record, state, fmt.Errorf("reseed guest mmds: %w", err))
	}
	if err := sendGuestPersonalization(personalizeCtx, state, envelope.Latest.MetaData); err != nil {
		return d.personalizeGuestConfigViaSSH(ctx, record, state, fmt.Errorf("apply guest config over vsock: %w", err))
	}
	return nil
}

func (d *Daemon) personalizeGuestConfigViaSSH(ctx context.Context, record *model.MachineRecord, state firecracker.MachineState, primaryErr error) error {
	fallbackErr := d.reconfigureGuestIdentity(ctx, state.RuntimeHost, record.ID, record.GuestConfig)
	if fallbackErr == nil {
		return nil
	}
	return fmt.Errorf("%w; ssh fallback failed: %v", primaryErr, fallbackErr)
}

func sendGuestPersonalization(ctx context.Context, state firecracker.MachineState, payload guestMetadataPayload) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal guest personalization payload: %w", err)
	}

	vsockPath, err := guestVsockHostPath(state)
	if err != nil {
		return err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", vsockPath)
	if err != nil {
		return fmt.Errorf("dial guest personalization vsock %q: %w", vsockPath, err)
	}
	defer func() {
		_ = connection.Close()
	}()
	setConnectionDeadline(ctx, connection)

	reader := bufio.NewReader(connection)
	if _, err := fmt.Fprintf(connection, "CONNECT %d\n", defaultGuestPersonalizationVsockPort); err != nil {
		return fmt.Errorf("write vsock connect request: %w", err)
	}
	response, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read vsock connect response: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(response), "OK ") {
		return fmt.Errorf("unexpected vsock connect response %q", strings.TrimSpace(response))
	}

	if _, err := connection.Write(append(payloadBytes, '\n')); err != nil {
		return fmt.Errorf("write guest personalization payload: %w", err)
	}
	response, err = reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read guest personalization response: %w", err)
	}
	if strings.TrimSpace(response) != "OK" {
		return fmt.Errorf("unexpected guest personalization response %q", strings.TrimSpace(response))
	}
	return nil
}

func guestVsockHostPath(state firecracker.MachineState) (string, error) {
	if state.PID < 1 {
		return "", fmt.Errorf("firecracker pid is required for guest vsock host path")
	}
	return filepath.Join("/proc", strconv.Itoa(state.PID), "root", "run", defaultGuestPersonalizationVsockName), nil
}

func setConnectionDeadline(ctx context.Context, connection net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
		return
	}
	_ = connection.SetDeadline(time.Now().Add(defaultGuestPersonalizationTimeout))
}
