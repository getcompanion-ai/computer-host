package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) reconfigureGuestIdentityOverSSH(ctx context.Context, runtimeHost string, machineID contracthost.MachineID, guestConfig *contracthost.GuestConfig) error {
	runtimeHost = strings.TrimSpace(runtimeHost)
	machineName := guestHostname(machineID, guestConfig)
	if runtimeHost == "" {
		return fmt.Errorf("guest runtime host is required")
	}
	if machineName == "" {
		return fmt.Errorf("machine id is required")
	}

	privateKeyPath := d.backendSSHPrivateKeyPath()
	remoteScript := fmt.Sprintf(`set -euo pipefail
machine_name=%s
printf '%%s\n' "$machine_name" >/etc/microagent/machine-name
printf '%%s\n' "$machine_name" >/etc/hostname
cat >/etc/hosts <<EOF
127.0.0.1 localhost
127.0.1.1 $machine_name
::1 localhost ip6-localhost ip6-loopback
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters
EOF
hostname "$machine_name" >/dev/null 2>&1 || true
`, strconv.Quote(machineName))

	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-i", privateKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(int(defaultSSHPort)),
		"node@"+runtimeHost,
		"sudo bash -lc "+shellSingleQuote(remoteScript),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reconfigure guest identity over ssh: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *Daemon) syncGuestFilesystemOverSSH(ctx context.Context, runtimeHost string) error {
	runtimeHost = strings.TrimSpace(runtimeHost)
	if runtimeHost == "" {
		return fmt.Errorf("guest runtime host is required")
	}

	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-i", d.backendSSHPrivateKeyPath(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(int(defaultSSHPort)),
		"node@"+runtimeHost,
		"sudo bash -lc "+shellSingleQuote("sync"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sync guest filesystem over ssh: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
