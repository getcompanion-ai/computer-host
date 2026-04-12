package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type guestReadyResult struct {
	ReadyNonce        string
	GuestSSHPublicKey string
}

func newGuestReadyNonce() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate guest ready nonce: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (d *Daemon) completeMachineStartup(ctx context.Context, record *model.MachineRecord, state firecracker.MachineState) (*model.MachineRecord, error) {
	if record == nil {
		return nil, fmt.Errorf("machine record is required")
	}
	if state.Phase != firecracker.PhaseRunning {
		failureReason := strings.TrimSpace(state.Error)
		if failureReason == "" {
			failureReason = "machine did not reach running phase"
		}
		return d.failMachineStartup(ctx, record, failureReason)
	}

	ready, err := d.personalizeGuest(ctx, record, state)
	if err != nil {
		return d.failMachineStartup(ctx, record, err.Error())
	}

	expectedNonce := strings.TrimSpace(record.GuestReadyNonce)
	receivedNonce := strings.TrimSpace(ready.ReadyNonce)
	if expectedNonce != "" && receivedNonce != expectedNonce {
		return d.failMachineStartup(ctx, record, "guest ready nonce mismatch")
	}

	expectedGuestSSHPublicKey := strings.TrimSpace(record.GuestSSHPublicKey)
	guestSSHPublicKey := strings.TrimSpace(ready.GuestSSHPublicKey)
	if guestSSHPublicKey == "" {
		if expectedGuestSSHPublicKey == "" {
			return d.failMachineStartup(ctx, record, "guest ready response missing ssh host key")
		}
		guestSSHPublicKey = expectedGuestSSHPublicKey
	}
	if expectedGuestSSHPublicKey != "" && guestSSHPublicKey != expectedGuestSSHPublicKey {
		return d.failMachineStartup(ctx, record, "guest ssh host key mismatch")
	}

	record.RuntimeHost = state.RuntimeHost
	record.TapDevice = state.TapName
	record.Ports = defaultMachinePorts()
	record.GuestSSHPublicKey = guestSSHPublicKey
	record.GuestReadyNonce = ""
	record.Phase = contracthost.MachinePhaseRunning
	record.Error = ""
	record.PID = state.PID
	record.SocketPath = state.SocketPath
	record.StartedAt = state.StartedAt
	if err := d.store.UpdateMachine(ctx, *record); err != nil {
		return nil, err
	}
	return record, nil
}
