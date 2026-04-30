package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

func TestInjectGuestConfigWritesAuthorizedKeysAndWebhook(t *testing.T) {
	root := t.TempDir()
	imagePath := filepath.Join(root, "rootfs.ext4")
	if err := buildTestExt4Image(root, imagePath); err != nil {
		t.Fatalf("build ext4 image: %v", err)
	}

	config := &contracthost.GuestConfig{
		AuthorizedKeys: []string{
			"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestKeyOne test-1",
			"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestKeyTwo test-2",
		},
		LoginWebhook: &contracthost.GuestLoginWebhook{
			URL:         "https://example.com/login",
			BearerToken: "secret-token",
		},
	}

	if err := injectGuestConfig(context.Background(), imagePath, config); err != nil {
		t.Fatalf("inject guest config: %v", err)
	}

	authorizedKeys, err := readExt4File(imagePath, "/etc/microagent/authorized_keys")
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	wantKeys := strings.Join(config.AuthorizedKeys, "\n") + "\n"
	if authorizedKeys != wantKeys {
		t.Fatalf("authorized_keys mismatch: got %q want %q", authorizedKeys, wantKeys)
	}

	guestConfigPayload, err := readExt4File(imagePath, "/etc/microagent/guest-config.json")
	if err != nil {
		t.Fatalf("read guest-config.json: %v", err)
	}

	var guestConfig contracthost.GuestConfig
	if err := json.Unmarshal([]byte(guestConfigPayload), &guestConfig); err != nil {
		t.Fatalf("unmarshal guest-config.json: %v", err)
	}
	if guestConfig.LoginWebhook == nil || guestConfig.LoginWebhook.URL != config.LoginWebhook.URL || guestConfig.LoginWebhook.BearerToken != config.LoginWebhook.BearerToken {
		t.Fatalf("login webhook mismatch: got %#v want %#v", guestConfig.LoginWebhook, config.LoginWebhook)
	}
}

func TestInjectMachineIdentityWritesHostnameFiles(t *testing.T) {
	root := t.TempDir()
	imagePath := filepath.Join(root, "rootfs.ext4")
	if err := buildTestExt4Image(root, imagePath); err != nil {
		t.Fatalf("build ext4 image: %v", err)
	}

	if err := injectMachineIdentity(context.Background(), imagePath, "kiruru"); err != nil {
		t.Fatalf("inject machine identity: %v", err)
	}

	machineName, err := readExt4File(imagePath, "/etc/microagent/machine-name")
	if err != nil {
		t.Fatalf("read machine-name: %v", err)
	}
	if machineName != "agentcomputer\n" {
		t.Fatalf("machine-name mismatch: got %q want %q", machineName, "agentcomputer\n")
	}

	hostname, err := readExt4File(imagePath, "/etc/hostname")
	if err != nil {
		t.Fatalf("read hostname: %v", err)
	}
	if hostname != "agentcomputer\n" {
		t.Fatalf("hostname mismatch: got %q want %q", hostname, "agentcomputer\n")
	}

	hosts, err := readExt4File(imagePath, "/etc/hosts")
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	if !strings.Contains(hosts, "127.0.1.1 agentcomputer") {
		t.Fatalf("hosts missing machine name: %q", hosts)
	}
}

func buildTestExt4Image(root string, imagePath string) error {
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "etc", "microagent"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(imagePath, nil, 0o644); err != nil {
		return err
	}
	command := exec.Command("truncate", "-s", "16M", imagePath)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("truncate: %w: %s", err, strings.TrimSpace(string(output)))
	}
	command = exec.Command("mkfs.ext4", "-q", "-d", sourceDir, "-L", "microagent-root", "-F", imagePath)
	output, err = command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func readExt4File(imagePath string, targetPath string) (string, error) {
	command := exec.Command("debugfs", "-R", "cat "+targetPath, imagePath)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("debugfs cat %q: %w: %s", targetPath, err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(string(output), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "debugfs ") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimPrefix(strings.Join(filtered, "\n"), "\n"), nil
}
