package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) systemVolumeID(machineID contracthost.MachineID) contracthost.VolumeID {
	return contracthost.VolumeID(fmt.Sprintf("%s-system", machineID))
}

func (d *Daemon) systemVolumePath(machineID contracthost.MachineID) string {
	return filepath.Join(d.config.MachineDisksDir, string(machineID), "system.img")
}

func (d *Daemon) machineRuntimeBaseDir(machineID contracthost.MachineID) string {
	return filepath.Join(d.config.RuntimeDir, "machines", string(machineID))
}

func (d *Daemon) backendSSHPrivateKeyPath() string {
	return filepath.Join(d.config.RootDir, "state", "ssh", "backend_ed25519")
}

func (d *Daemon) backendSSHPublicKeyPath() string {
	return d.backendSSHPrivateKeyPath() + ".pub"
}

func artifactKey(ref contracthost.ArtifactRef) string {
	sum := sha256.Sum256([]byte(ref.KernelImageURL + "\n" + ref.RootFSURL))
	return hex.EncodeToString(sum[:])
}

func cloneFile(source string, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create target dir for %q: %w", target, err)
	}

	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source file %q: %w", source, err)
	}
	defer sourceFile.Close()

	sourceInfo, err := sourceFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source file %q: %w", source, err)
	}

	tmpPath := target + ".tmp"
	targetFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open target file %q: %w", tmpPath, err)
	}

	if _, err := writeSparseFile(targetFile, sourceFile); err != nil {
		targetFile.Close()
		return fmt.Errorf("copy %q to %q: %w", source, tmpPath, err)
	}
	if err := targetFile.Truncate(sourceInfo.Size()); err != nil {
		targetFile.Close()
		return fmt.Errorf("truncate target file %q: %w", tmpPath, err)
	}
	if err := targetFile.Sync(); err != nil {
		targetFile.Close()
		return fmt.Errorf("sync target file %q: %w", tmpPath, err)
	}
	if err := targetFile.Close(); err != nil {
		return fmt.Errorf("close target file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename target file %q to %q: %w", tmpPath, target, err)
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func downloadFile(ctx context.Context, rawURL string, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat download target %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create download dir for %q: %w", path, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build download request for %q: %w", rawURL, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("download %q: %w", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %q: status %d", rawURL, response.StatusCode)
	}

	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open download target %q: %w", tmpPath, err)
	}

	size, err := writeSparseFile(file, response.Body)
	if err != nil {
		file.Close()
		return fmt.Errorf("write download target %q: %w", tmpPath, err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		return fmt.Errorf("truncate download target %q: %w", tmpPath, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync download target %q: %w", tmpPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close download target %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename download target %q to %q: %w", tmpPath, path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func writeSparseFile(targetFile *os.File, source io.Reader) (int64, error) {
	buffer := make([]byte, defaultCopyBufferSize)
	var size int64

	for {
		count, err := source.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			if isZeroChunk(chunk) {
				if _, seekErr := targetFile.Seek(int64(count), io.SeekCurrent); seekErr != nil {
					return size, seekErr
				}
			} else {
				if _, writeErr := targetFile.Write(chunk); writeErr != nil {
					return size, writeErr
				}
			}
			size += int64(count)
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			return size, nil
		}
		return size, err
	}
}

func isZeroChunk(chunk []byte) bool {
	for _, value := range chunk {
		if value != 0 {
			return false
		}
	}
	return true
}

func defaultMachinePorts() []contracthost.MachinePort {
	return []contracthost.MachinePort{
		{Name: contracthost.MachinePortNameSSH, Port: defaultSSHPort, Protocol: contracthost.PortProtocolTCP},
		{Name: contracthost.MachinePortNameVNC, Port: defaultVNCPort, Protocol: contracthost.PortProtocolTCP},
	}
}

func (d *Daemon) ensureBackendSSHKeyPair() error {
	privateKeyPath := d.backendSSHPrivateKeyPath()
	publicKeyPath := d.backendSSHPublicKeyPath()
	if err := os.MkdirAll(filepath.Dir(privateKeyPath), 0o700); err != nil {
		return fmt.Errorf("create backend ssh dir: %w", err)
	}
	privateExists := fileExists(privateKeyPath)
	publicExists := fileExists(publicKeyPath)
	switch {
	case privateExists && publicExists:
		return nil
	case privateExists && !publicExists:
		return d.writeBackendSSHPublicKey(privateKeyPath, publicKeyPath)
	case !privateExists && publicExists:
		return fmt.Errorf("backend ssh private key %q is missing while public key exists", privateKeyPath)
	}

	command := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKeyPath)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generate backend ssh keypair: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *Daemon) writeBackendSSHPublicKey(privateKeyPath string, publicKeyPath string) error {
	command := exec.Command("ssh-keygen", "-y", "-f", privateKeyPath)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("derive backend ssh public key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	payload := strings.TrimSpace(string(output)) + "\n"
	if err := os.WriteFile(publicKeyPath, []byte(payload), 0o644); err != nil {
		return fmt.Errorf("write backend ssh public key %q: %w", publicKeyPath, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (d *Daemon) mergedGuestConfig(config *contracthost.GuestConfig) (*contracthost.GuestConfig, error) {
	hostAuthorizedKey, err := os.ReadFile(d.backendSSHPublicKeyPath())
	if err != nil {
		return nil, fmt.Errorf("read backend ssh public key: %w", err)
	}
	authorizedKeys := []string{strings.TrimSpace(string(hostAuthorizedKey))}
	if config != nil {
		authorizedKeys = append(authorizedKeys, config.AuthorizedKeys...)
	}

	merged := &contracthost.GuestConfig{
		AuthorizedKeys: authorizedKeys,
	}
	if config != nil && config.LoginWebhook != nil {
		loginWebhook := *config.LoginWebhook
		merged.LoginWebhook = &loginWebhook
	}
	return merged, nil
}

func hasGuestConfig(config *contracthost.GuestConfig) bool {
	if config == nil {
		return false
	}
	return len(config.AuthorizedKeys) > 0 || config.LoginWebhook != nil
}

func injectGuestConfig(ctx context.Context, imagePath string, config *contracthost.GuestConfig) error {
	if !hasGuestConfig(config) {
		return nil
	}
	stagingDir, err := os.MkdirTemp(filepath.Dir(imagePath), "guest-config-*")
	if err != nil {
		return fmt.Errorf("create guest config staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	if len(config.AuthorizedKeys) > 0 {
		authorizedKeysPath := filepath.Join(stagingDir, "authorized_keys")
		payload := []byte(strings.Join(config.AuthorizedKeys, "\n") + "\n")
		if err := os.WriteFile(authorizedKeysPath, payload, 0o600); err != nil {
			return fmt.Errorf("write authorized_keys staging file: %w", err)
		}
		if err := replaceExt4File(ctx, imagePath, authorizedKeysPath, "/etc/microagent/authorized_keys"); err != nil {
			return err
		}
	}

	if config.LoginWebhook != nil {
		guestConfigPath := filepath.Join(stagingDir, "guest-config.json")
		payload, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("marshal guest config: %w", err)
		}
		if err := os.WriteFile(guestConfigPath, append(payload, '\n'), 0o600); err != nil {
			return fmt.Errorf("write guest config staging file: %w", err)
		}
		if err := replaceExt4File(ctx, imagePath, guestConfigPath, "/etc/microagent/guest-config.json"); err != nil {
			return err
		}
	}
	return nil
}

func replaceExt4File(ctx context.Context, imagePath string, sourcePath string, targetPath string) error {
	_ = runDebugFS(ctx, imagePath, fmt.Sprintf("rm %s", targetPath))
	if err := runDebugFS(ctx, imagePath, fmt.Sprintf("write %s %s", sourcePath, targetPath)); err != nil {
		return fmt.Errorf("inject %q into %q: %w", targetPath, imagePath, err)
	}
	return nil
}

func runDebugFS(ctx context.Context, imagePath string, command string) error {
	cmd := exec.CommandContext(ctx, "debugfs", "-w", "-R", command, imagePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs %q on %q: %w: %s", command, imagePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func machineIDPtr(machineID contracthost.MachineID) *contracthost.MachineID {
	value := machineID
	return &value
}

func machineToContract(record model.MachineRecord) contracthost.Machine {
	return contracthost.Machine{
		ID:             record.ID,
		Artifact:       record.Artifact,
		SystemVolumeID: record.SystemVolumeID,
		UserVolumeIDs:  append([]contracthost.VolumeID(nil), record.UserVolumeIDs...),
		RuntimeHost:    record.RuntimeHost,
		Ports:          append([]contracthost.MachinePort(nil), record.Ports...),
		Phase:          record.Phase,
		Error:          record.Error,
		CreatedAt:      record.CreatedAt,
		StartedAt:      record.StartedAt,
	}
}

func machineToRuntimeState(record model.MachineRecord) firecracker.MachineState {
	phase := firecracker.PhaseStopped
	switch record.Phase {
	case contracthost.MachinePhaseRunning:
		phase = firecracker.PhaseRunning
	case contracthost.MachinePhaseFailed:
		phase = firecracker.PhaseFailed
	}
	return firecracker.MachineState{
		ID:          firecracker.MachineID(record.ID),
		Phase:       phase,
		PID:         record.PID,
		RuntimeHost: record.RuntimeHost,
		SocketPath:  record.SocketPath,
		TapName:     record.TapDevice,
		StartedAt:   record.StartedAt,
		Error:       record.Error,
	}
}

func validateArtifactRef(ref contracthost.ArtifactRef) error {
	if err := validateDownloadURL("artifact.kernel_image_url", ref.KernelImageURL); err != nil {
		return err
	}
	if err := validateDownloadURL("artifact.rootfs_url", ref.RootFSURL); err != nil {
		return err
	}
	return nil
}

func validateGuestConfig(config *contracthost.GuestConfig) error {
	if config == nil {
		return nil
	}
	for i, key := range config.AuthorizedKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("guest_config.authorized_keys[%d] is required", i)
		}
	}
	if config.LoginWebhook != nil {
		if err := validateDownloadURL("guest_config.login_webhook.url", config.LoginWebhook.URL); err != nil {
			return err
		}
	}
	return nil
}

func validateMachineID(machineID contracthost.MachineID) error {
	value := strings.TrimSpace(string(machineID))
	if value == "" {
		return fmt.Errorf("machine_id is required")
	}
	if filepath.Base(value) != value {
		return fmt.Errorf("machine_id %q must not contain path separators", machineID)
	}
	return nil
}

func validateDownloadURL(field string, raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s is invalid: %w", field, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", field)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("%s host is required", field)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open dir %q: %w", path, err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync dir %q: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close dir %q: %w", path, err)
	}
	return nil
}
