package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AgentComputerAI/computer-host/internal/firecracker"
	"github.com/AgentComputerAI/computer-host/internal/model"
	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

const (
	defaultGuestPersonalizationVsockID   = "microagent-personalizer"
	defaultGuestPersonalizationVsockName = "microagent-personalizer.vsock"
	defaultGuestPersonalizationVsockPort = uint32(1024)
	defaultGuestPersonalizationTimeout   = 30 * time.Second
	guestPersonalizationRetryInterval    = 100 * time.Millisecond
	minGuestVsockCID                     = uint32(3)
	maxGuestVsockCID                     = uint32(1<<31 - 1)
)

type guestPersonalizationResponse struct {
	Status            string `json:"status"`
	ReadyNonce        string `json:"ready_nonce,omitempty"`
	GuestSSHPublicKey string `json:"guest_ssh_public_key,omitempty"`
	Error             string `json:"error,omitempty"`
}

type guestReadyRequest struct {
	ReadyNonce string `json:"ready_nonce,omitempty"`
}

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

func (d *Daemon) personalizeGuestConfig(ctx context.Context, record *model.MachineRecord, state firecracker.MachineState) (*guestReadyResult, error) {
	if record == nil {
		return nil, fmt.Errorf("machine record is required")
	}

	personalizeCtx, cancel := context.WithTimeout(ctx, defaultGuestPersonalizationTimeout)
	defer cancel()

	response, err := sendGuestPersonalization(personalizeCtx, state, guestReadyRequest{
		ReadyNonce: strings.TrimSpace(record.GuestReadyNonce),
	})
	if err != nil {
		return nil, fmt.Errorf("wait for guest ready over vsock: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(response.Status), "ok") {
		message := strings.TrimSpace(response.Error)
		if message == "" {
			message = fmt.Sprintf("unexpected guest personalization status %q", strings.TrimSpace(response.Status))
		}
		return nil, errors.New(message)
	}
	return &guestReadyResult{
		ReadyNonce:        strings.TrimSpace(response.ReadyNonce),
		GuestSSHPublicKey: strings.TrimSpace(response.GuestSSHPublicKey),
	}, nil
}

func sendGuestPersonalization(ctx context.Context, state firecracker.MachineState, payload guestReadyRequest) (*guestPersonalizationResponse, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal guest personalization payload: %w", err)
	}

	vsockPath, err := guestVsockHostPath(state)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ctx.Err()
		}

		resp, err := tryGuestPersonalization(ctx, vsockPath, payloadBytes)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(guestPersonalizationRetryInterval):
		}
	}
}

func tryGuestPersonalization(ctx context.Context, vsockPath string, payloadBytes []byte) (*guestPersonalizationResponse, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", vsockPath)
	if err != nil {
		return nil, fmt.Errorf("dial guest personalization vsock %q: %w", vsockPath, err)
	}
	defer func() {
		_ = connection.Close()
	}()
	setConnectionDeadline(ctx, connection)

	reader := bufio.NewReader(connection)
	if _, err := fmt.Fprintf(connection, "CONNECT %d\n", defaultGuestPersonalizationVsockPort); err != nil {
		return nil, fmt.Errorf("write vsock connect request: %w", err)
	}
	response, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read vsock connect response: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(response), "OK ") {
		return nil, fmt.Errorf("unexpected vsock connect response %q", strings.TrimSpace(response))
	}

	if _, err := connection.Write(append(payloadBytes, '\n')); err != nil {
		return nil, fmt.Errorf("write guest personalization payload: %w", err)
	}
	response, err = reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read guest personalization response: %w", err)
	}
	var payloadResponse guestPersonalizationResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(response)), &payloadResponse); err != nil {
		return nil, fmt.Errorf("decode guest personalization response %q: %w", strings.TrimSpace(response), err)
	}
	return &payloadResponse, nil
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
