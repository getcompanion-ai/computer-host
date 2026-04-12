package daemon

import (
	"context"
	"encoding/json"
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
	mmds, err := d.guestMetadataSpec(machineID, guestConfig, "")
	if err != nil {
		return err
	}
	envelope, ok := mmds.Data.(guestMetadataEnvelope)
	if !ok {
		return fmt.Errorf("guest metadata payload has unexpected type %T", mmds.Data)
	}
	payloadBytes, err := json.Marshal(envelope.Latest.MetaData)
	if err != nil {
		return fmt.Errorf("marshal guest metadata payload: %w", err)
	}

	privateKeyPath := d.backendSSHPrivateKeyPath()
	remoteScript := fmt.Sprintf(`set -euo pipefail
payload=%s
install -d -m 0755 /etc/microagent
machine_name="$(printf '%%s' "$payload" | jq -r '.hostname // .machine_id // empty')"
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
if printf '%%s' "$payload" | jq -e '.authorized_keys | length > 0' >/dev/null 2>&1; then
  install -d -m 0700 -o node -g node /home/node/.ssh
  printf '%%s' "$payload" | jq -r '.authorized_keys[]' >/home/node/.ssh/authorized_keys
  chmod 0600 /home/node/.ssh/authorized_keys
  chown node:node /home/node/.ssh/authorized_keys
  printf '%%s' "$payload" | jq -r '.authorized_keys[]' >/etc/microagent/authorized_keys
  chmod 0600 /etc/microagent/authorized_keys
else
  rm -f /home/node/.ssh/authorized_keys /etc/microagent/authorized_keys
fi
if printf '%%s' "$payload" | jq -e '.trusted_user_ca_keys | length > 0' >/dev/null 2>&1; then
  printf '%%s' "$payload" | jq -r '.trusted_user_ca_keys[]' >/etc/microagent/trusted_user_ca_keys
  chmod 0644 /etc/microagent/trusted_user_ca_keys
else
  rm -f /etc/microagent/trusted_user_ca_keys
fi
printf '%%s' "$payload" | jq '{authorized_keys, trusted_user_ca_keys, login_webhook}' >/etc/microagent/guest-config.json
chmod 0600 /etc/microagent/guest-config.json
`, strconv.Quote(string(payloadBytes)))

	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-i", privateKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
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
		"-o", "ConnectTimeout=2",
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
