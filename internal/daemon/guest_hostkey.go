package daemon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

func readGuestSSHPublicKey(ctx context.Context, runtimeHost string) (string, error) {
	host := strings.TrimSpace(runtimeHost)
	if host == "" {
		return "", fmt.Errorf("runtime host is required")
	}

	probeCtx, cancel := context.WithTimeout(ctx, defaultGuestDialTimeout)
	defer cancel()

	targetAddr := net.JoinHostPort(host, strconv.Itoa(int(defaultSSHPort)))
	netConn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", targetAddr)
	if err != nil {
		return "", fmt.Errorf("dial guest ssh for host key: %w", err)
	}
	defer func() {
		_ = netConn.Close()
	}()

	var captured ssh.PublicKey
	clientConfig := &ssh.ClientConfig{
		User:              "host-key-probe",
		Auth:              []ssh.AuthMethod{ssh.Password("invalid")},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return fmt.Errorf("guest ssh host key captured")
		},
		Timeout:       defaultGuestDialTimeout,
		ClientVersion: "SSH-2.0-agentcomputer-firecracker-host",
	}
	_, _, _, err = ssh.NewClientConn(netConn, targetAddr, clientConfig)
	if captured == nil {
		if err == nil {
			return "", fmt.Errorf("guest ssh host key probe returned without a host key")
		}
		return "", fmt.Errorf("handshake guest ssh for host key: %w", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(captured))), nil
}
