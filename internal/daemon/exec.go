package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const (
	defaultExecTimeout = 30 * time.Second
	maxExecTimeout     = 5 * time.Minute
	maxExecOutputBytes = 10 * 1024 * 1024
)

// guestd Connect RPC request/response types (mirrors the protobuf spec).
type guestdProcessConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  *string           `json:"cwd,omitempty"`
}

type guestdStartRequest struct {
	Process *guestdProcessConfig `json:"process"`
}

type guestdStartResponse struct {
	Event *guestdProcessEvent `json:"event,omitempty"`
}

type guestdProcessEvent struct {
	Start     *guestdStartEvent `json:"start,omitempty"`
	Data      *guestdDataEvent  `json:"data,omitempty"`
	End       *guestdEndEvent   `json:"end,omitempty"`
	Keepalive *struct{}         `json:"keepalive,omitempty"`
}

type guestdStartEvent struct {
	Pid int32 `json:"pid,omitempty"`
}

type guestdDataEvent struct {
	Stdout []byte `json:"stdout,omitempty"`
	Stderr []byte `json:"stderr,omitempty"`
}

type guestdEndEvent struct {
	ExitCode int32  `json:"exitCode,omitempty"`
	Exited   bool   `json:"exited,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (d *Daemon) ExecCommand(ctx context.Context, id contracthost.MachineID, req contracthost.ExecRequest) (*contracthost.ExecResponse, error) {
	if len(req.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}

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

	timeout := defaultExecTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
		if timeout > maxExecTimeout {
			timeout = maxExecTimeout
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := req.Command[0]
	args := req.Command[1:]
	var cwd *string
	if req.Cwd != "" {
		cwd = &req.Cwd
	}
	envs := req.Env
	if envs == nil {
		envs = map[string]string{}
	}
	if _, hasPath := envs["PATH"]; !hasPath {
		envs["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	user := req.User
	if user == "" {
		user = "node"
	}

	startReq := guestdStartRequest{
		Process: &guestdProcessConfig{
			Cmd:  cmd,
			Args: args,
			Cwd:  cwd,
			Envs: envs,
		},
	}

	start := time.Now()
	stdout, stderr, exitCode, err := d.callGuestdStart(execCtx, record.RuntimeHost, user, timeout, startReq)
	if err != nil {
		return nil, fmt.Errorf("exec on machine %q: %w", id, err)
	}

	return &contracthost.ExecResponse{
		ExitCode:   exitCode,
		Stdout:     stdout,
		Stderr:     stderr,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (d *Daemon) callGuestdStart(ctx context.Context, runtimeHost, user string, timeout time.Duration, req guestdStartRequest) (string, string, int, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("marshal start request: %w", err)
	}

	// Connect RPC server-stream: 5-byte envelope prefix (flags=0, big-endian length)
	envelope := make([]byte, 5+len(payload))
	envelope[0] = 0 // flags: uncompressed
	binary.BigEndian.PutUint32(envelope[1:5], uint32(len(payload)))
	copy(envelope[5:], payload)

	guestdURL := fmt.Sprintf("http://%s:%d/process.Process/Start", runtimeHost, defaultGuestdPort)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, guestdURL, bytes.NewReader(envelope))
	if err != nil {
		return "", "", 0, fmt.Errorf("build guestd request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/connect+json")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Connect-Timeout-Ms", fmt.Sprintf("%d", timeout.Milliseconds()))
	applyGuestdUserAuth(httpReq, user)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", "", 0, fmt.Errorf("guestd request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", 0, fmt.Errorf("guestd returned %d: %s", resp.StatusCode, string(body))
	}

	// Read Connect RPC server-stream response: sequence of enveloped JSON messages
	var stdout, stderr bytes.Buffer
	var exitCode int

	for {
		// Read 5-byte envelope header
		header := make([]byte, 5)
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return "", "", 0, fmt.Errorf("read envelope header: %w", err)
		}
		flags := header[0]
		length := binary.BigEndian.Uint32(header[1:5])
		if length > 10*1024*1024 {
			return "", "", 0, fmt.Errorf("envelope too large: %d bytes", length)
		}

		msgBytes := make([]byte, length)
		if _, err := io.ReadFull(resp.Body, msgBytes); err != nil {
			return "", "", 0, fmt.Errorf("read envelope body: %w", err)
		}

		// flags & 0x02 = end-of-stream trailer
		if flags&0x02 != 0 {
			break
		}

		var msg guestdStartResponse
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			// May be protobuf, not JSON - try to continue
			continue
		}
		if msg.Event == nil {
			continue
		}
		if msg.Event.Data != nil {
			if len(msg.Event.Data.Stdout) > 0 && stdout.Len() < maxExecOutputBytes {
				stdout.Write(msg.Event.Data.Stdout)
			}
			if len(msg.Event.Data.Stderr) > 0 && stderr.Len() < maxExecOutputBytes {
				stderr.Write(msg.Event.Data.Stderr)
			}
		}
		if msg.Event.End != nil {
			exitCode = int(msg.Event.End.ExitCode)
		}
	}

	return stdout.String(), stderr.String(), exitCode, nil
}

func applyGuestdUserAuth(req *http.Request, user string) {
	req.SetBasicAuth(user, "")
	// Kept for compatibility with older guestd builds or diagnostics that still
	// look at the explicit username header.
	req.Header.Set("X-Username", user)
}
