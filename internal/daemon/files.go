package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	appconfig "github.com/AgentComputerAI/computer-host/internal/config"
	"github.com/AgentComputerAI/computer-host/internal/firecracker"
	"github.com/AgentComputerAI/computer-host/internal/model"
	"github.com/AgentComputerAI/computer-host/internal/store"
	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

func (d *Daemon) systemVolumeID(machineID contracthost.MachineID) contracthost.VolumeID {
	return contracthost.VolumeID(fmt.Sprintf("%s-system", machineID))
}

func (d *Daemon) systemVolumePath(machineID contracthost.MachineID) string {
	return filepath.Join(d.config.MachineDisksDir, string(machineID), "system.img")
}

const workspaceVolumeLabel = "microagent-ws"

func (d *Daemon) workspaceVolumeID(machineID contracthost.MachineID) contracthost.VolumeID {
	return contracthost.VolumeID(fmt.Sprintf("%s-workspace", machineID))
}

func (d *Daemon) workspaceVolumePath(machineID contracthost.MachineID) string {
	return filepath.Join(d.config.MachineDisksDir, string(machineID), "workspace.img")
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
	defer func() {
		_ = sourceFile.Close()
	}()

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
		_ = targetFile.Close()
		return fmt.Errorf("copy %q to %q: %w", source, tmpPath, err)
	}
	if err := targetFile.Truncate(sourceInfo.Size()); err != nil {
		_ = targetFile.Close()
		return fmt.Errorf("truncate target file %q: %w", tmpPath, err)
	}
	if err := targetFile.Sync(); err != nil {
		_ = targetFile.Close()
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

func cloneDiskFile(source string, target string, mode appconfig.DiskCloneMode) error {
	switch mode {
	case appconfig.DiskCloneModeReflink:
		if err := reflinkFile(source, target); err != nil {
			return reflinkRequiredError(err)
		}
		return nil
	case appconfig.DiskCloneModeCopy:
		return cloneFile(source, target)
	default:
		return fmt.Errorf("unsupported disk clone mode %q", mode)
	}
}

func createExt4VolumeImage(ctx context.Context, path string, sizeBytes int64, label string) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("ext4 volume size must be greater than zero")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create volume dir for %q: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing volume %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("create volume file %q: %w", path, err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate volume file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close volume file %q: %w", path, err)
	}

	args := []string{"-q", "-F"}
	if strings.TrimSpace(label) != "" {
		args = append(args, "-L", label)
	}
	args = append(args, path)
	command := exec.CommandContext(ctx, "mkfs.ext4", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %q: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return syncDir(filepath.Dir(path))
}

func volumeLabel(ctx context.Context, path string) (string, error) {
	command := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "LABEL", path)
	output, err := command.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", nil
		}
		return "", fmt.Errorf("blkid label for %q: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func detectVolumePurpose(ctx context.Context, path string) (model.VolumePurpose, bool, error) {
	label, err := volumeLabel(ctx, path)
	if err != nil {
		return "", false, err
	}
	switch label {
	case workspaceVolumeLabel:
		return model.VolumePurposeWorkspace, true, nil
	default:
		return "", false, nil
	}
}

func (d *Daemon) createWorkspaceVolume(ctx context.Context, machineID contracthost.MachineID, sizeBytes int64) (model.VolumeRecord, error) {
	path := d.workspaceVolumePath(machineID)
	if err := createExt4VolumeImage(ctx, path, sizeBytes, workspaceVolumeLabel); err != nil {
		return model.VolumeRecord{}, err
	}
	if err := d.ensureOfflineWorkspaceRoot(ctx, path); err != nil {
		_ = os.Remove(path)
		return model.VolumeRecord{}, fmt.Errorf("prepare workspace volume %q: %w", machineID, err)
	}
	now := time.Now().UTC()
	return model.VolumeRecord{
		ID:                    d.workspaceVolumeID(machineID),
		Kind:                  contracthost.VolumeKindUser,
		AttachedMachineID:     machineIDPtr(machineID),
		Pool:                  model.StoragePoolUserVolumes,
		Path:                  path,
		Purpose:               model.VolumePurposeWorkspace,
		DeleteOnMachineDelete: true,
		CreatedAt:             now,
	}, nil
}

func (d *Daemon) workspaceVolumeForMachine(ctx context.Context, record model.MachineRecord) (*model.VolumeRecord, error) {
	volumes := make([]*model.VolumeRecord, 0, len(record.UserVolumeIDs))
	for _, volumeID := range record.UserVolumeIDs {
		volume, err := d.store.GetVolume(ctx, volumeID)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
		if volume.Purpose == model.VolumePurposeWorkspace {
			return volume, nil
		}
	}

	for _, volume := range volumes {
		purpose, deleteOnMachineDelete, err := detectVolumePurpose(ctx, volume.Path)
		if err != nil {
			return nil, err
		}
		if purpose != model.VolumePurposeWorkspace {
			continue
		}
		if volume.Purpose != purpose || volume.DeleteOnMachineDelete != deleteOnMachineDelete {
			volume.Purpose = purpose
			volume.DeleteOnMachineDelete = deleteOnMachineDelete
			if err := d.store.UpdateVolume(ctx, *volume); err != nil {
				return nil, err
			}
		}
		return volume, nil
	}

	return nil, store.ErrNotFound
}

func validateDiskCloneBackend(cfg appconfig.Config) error {
	if cfg.DiskCloneMode != appconfig.DiskCloneModeReflink {
		return nil
	}

	sourceFile, err := os.CreateTemp(cfg.ArtifactsDir, ".reflink-probe-source-*")
	if err != nil {
		return fmt.Errorf("create reflink probe source in %q: %w", cfg.ArtifactsDir, err)
	}
	sourcePath := sourceFile.Name()
	defer func() {
		_ = os.Remove(sourcePath)
	}()

	if _, err := sourceFile.WriteString("reflink-probe"); err != nil {
		_ = sourceFile.Close()
		return fmt.Errorf("write reflink probe source %q: %w", sourcePath, err)
	}
	if err := sourceFile.Close(); err != nil {
		return fmt.Errorf("close reflink probe source %q: %w", sourcePath, err)
	}

	targetPath := filepath.Join(cfg.MachineDisksDir, "."+filepath.Base(sourcePath)+".target")
	defer func() {
		_ = os.Remove(targetPath)
	}()
	if err := cloneDiskFile(sourcePath, targetPath, cfg.DiskCloneMode); err != nil {
		return fmt.Errorf("validate disk clone backend from artifacts dir %q to machine disks dir %q: %w", cfg.ArtifactsDir, cfg.MachineDisksDir, err)
	}

	snapshotProbePath := filepath.Join(cfg.SnapshotsDir, "."+filepath.Base(sourcePath)+".snapshot-target")
	defer func() {
		_ = os.Remove(snapshotProbePath)
	}()
	if err := cloneDiskFile(targetPath, snapshotProbePath, cfg.DiskCloneMode); err != nil {
		return fmt.Errorf("validate disk clone backend from machine disks dir %q to snapshots dir %q: %w", cfg.MachineDisksDir, cfg.SnapshotsDir, err)
	}
	return nil
}

func reflinkRequiredError(err error) error {
	return fmt.Errorf("FIRECRACKER_HOST_DISK_CLONE_MODE=reflink requires a CoW filesystem with reflink support across artifacts, machine-disks, and snapshots; mount FIRECRACKER_HOST_ROOT_DIR on XFS with reflink=1 or Btrfs, preferably on local NVMe, or set FIRECRACKER_HOST_DISK_CLONE_MODE=copy for the slow full-copy fallback: %w", err)
}

func reflinkFile(source string, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create target dir for %q: %w", target, err)
	}

	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source file %q: %w", source, err)
	}
	defer func() {
		_ = sourceFile.Close()
	}()

	sourceInfo, err := sourceFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source file %q: %w", source, err)
	}

	tmpPath := target + ".tmp"
	targetFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, sourceInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("open target file %q: %w", tmpPath, err)
	}

	if err := ioctlFileClone(targetFile, sourceFile); err != nil {
		_ = targetFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("reflink clone %q to %q: %w", source, tmpPath, err)
	}
	if err := targetFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close target file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename target file %q to %q: %w", tmpPath, target, err)
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func ioctlFileClone(targetFile *os.File, sourceFile *os.File) error {
	const ficlone = 0x40049409

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, targetFile.Fd(), ficlone, sourceFile.Fd())
	if errno != 0 {
		return errno
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
	defer func() {
		_ = response.Body.Close()
	}()
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
		_ = file.Close()
		return fmt.Errorf("write download target %q: %w", tmpPath, err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate download target %q: %w", tmpPath, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
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
	return buildMachinePorts(0, 0, 0)
}

func buildMachinePorts(sshRelayPort, vncRelayPort, execRelayPort uint16) []contracthost.MachinePort {
	return []contracthost.MachinePort{
		{Name: contracthost.MachinePortNameSSH, Port: defaultSSHPort, HostPort: sshRelayPort, Protocol: contracthost.PortProtocolTCP},
		{Name: contracthost.MachinePortNameVNC, Port: defaultVNCPort, HostPort: vncRelayPort, Protocol: contracthost.PortProtocolTCP},
		{Name: contracthost.MachinePortNameExec, Port: defaultGuestdPort, HostPort: execRelayPort, Protocol: contracthost.PortProtocolTCP},
	}
}

func setMachineExecRelayPort(ports []contracthost.MachinePort, relayPort uint16) []contracthost.MachinePort {
	updated := make([]contracthost.MachinePort, 0, len(ports))
	replaced := false
	for _, port := range ports {
		if port.Name == contracthost.MachinePortNameExec {
			port.HostPort = relayPort
			if port.Port == 0 {
				port.Port = defaultGuestdPort
			}
			port.Protocol = contracthost.PortProtocolTCP
			replaced = true
		}
		updated = append(updated, port)
	}
	if !replaced {
		updated = append(updated, contracthost.MachinePort{
			Name:     contracthost.MachinePortNameExec,
			Port:     defaultGuestdPort,
			HostPort: relayPort,
			Protocol: contracthost.PortProtocolTCP,
		})
	}
	return updated
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

type guestSSHHostKeyPair struct {
	PrivateKey []byte
	PublicKey  string
}

func generateGuestSSHHostKeyPair(ctx context.Context) (*guestSSHHostKeyPair, error) {
	stagingDir, err := os.MkdirTemp("", "guest-ssh-hostkey-*")
	if err != nil {
		return nil, fmt.Errorf("create guest ssh host key staging dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()

	privateKeyPath := filepath.Join(stagingDir, "ssh_host_ed25519_key")
	command := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", privateKeyPath)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("generate guest ssh host keypair: %w: %s", err, strings.TrimSpace(string(output)))
	}

	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read guest ssh host private key %q: %w", privateKeyPath, err)
	}
	publicKey, err := os.ReadFile(privateKeyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("read guest ssh host public key %q: %w", privateKeyPath+".pub", err)
	}

	return &guestSSHHostKeyPair{
		PrivateKey: privateKey,
		PublicKey:  strings.TrimSpace(string(publicKey)),
	}, nil
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
		Hostname:          "",
		AuthorizedKeys:    authorizedKeys,
		TrustedUserCAKeys: nil,
	}
	if config != nil {
		merged.Hostname = strings.TrimSpace(config.Hostname)
	}
	if strings.TrimSpace(d.config.GuestLoginCAPublicKey) != "" {
		merged.TrustedUserCAKeys = append(merged.TrustedUserCAKeys, d.config.GuestLoginCAPublicKey)
	}
	if config != nil {
		merged.TrustedUserCAKeys = append(merged.TrustedUserCAKeys, config.TrustedUserCAKeys...)
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
	return len(config.AuthorizedKeys) > 0 || len(config.TrustedUserCAKeys) > 0 || config.LoginWebhook != nil
}

func injectGuestConfig(ctx context.Context, imagePath string, config *contracthost.GuestConfig) error {
	if !hasGuestConfig(config) {
		return nil
	}
	stagingDir, err := os.MkdirTemp(filepath.Dir(imagePath), "guest-config-*")
	if err != nil {
		return fmt.Errorf("create guest config staging dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()

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

	if len(config.TrustedUserCAKeys) > 0 {
		trustedCAPath := filepath.Join(stagingDir, "trusted_user_ca_keys")
		payload := []byte(strings.Join(config.TrustedUserCAKeys, "\n") + "\n")
		if err := os.WriteFile(trustedCAPath, payload, 0o644); err != nil {
			return fmt.Errorf("write trusted_user_ca_keys staging file: %w", err)
		}
		if err := replaceExt4File(ctx, imagePath, trustedCAPath, "/etc/microagent/trusted_user_ca_keys"); err != nil {
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

func injectGuestSSHHostKey(ctx context.Context, imagePath string, keyPair *guestSSHHostKeyPair) error {
	if keyPair == nil {
		return fmt.Errorf("guest ssh host keypair is required")
	}
	if strings.TrimSpace(keyPair.PublicKey) == "" {
		return fmt.Errorf("guest ssh host public key is required")
	}

	stagingDir, err := os.MkdirTemp(filepath.Dir(imagePath), "guest-ssh-hostkey-*")
	if err != nil {
		return fmt.Errorf("create guest ssh host key staging dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()

	privateKeyPath := filepath.Join(stagingDir, "ssh_host_ed25519_key")
	if err := os.WriteFile(privateKeyPath, keyPair.PrivateKey, 0o600); err != nil {
		return fmt.Errorf("write guest ssh host private key staging file: %w", err)
	}
	if err := replaceExt4FileMode(ctx, imagePath, privateKeyPath, "/etc/ssh/ssh_host_ed25519_key", "100600"); err != nil {
		return err
	}

	publicKeyPath := privateKeyPath + ".pub"
	if err := os.WriteFile(publicKeyPath, []byte(strings.TrimSpace(keyPair.PublicKey)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write guest ssh host public key staging file: %w", err)
	}
	if err := replaceExt4FileMode(ctx, imagePath, publicKeyPath, "/etc/ssh/ssh_host_ed25519_key.pub", "100644"); err != nil {
		return err
	}

	return nil
}

func injectMachineIdentity(ctx context.Context, imagePath string, machineID contracthost.MachineID) error {
	machineName := strings.TrimSpace(string(machineID))
	if machineName == "" {
		return fmt.Errorf("machine_id is required")
	}
	hostname := "agentcomputer"

	stagingDir, err := os.MkdirTemp(filepath.Dir(imagePath), "machine-identity-*")
	if err != nil {
		return fmt.Errorf("create machine identity staging dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()

	identityFiles := map[string]string{
		"/etc/microagent/machine-name": hostname + "\n",
		"/etc/hostname":                hostname + "\n",
		"/etc/hosts": fmt.Sprintf(
			"127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\nff02::1 ip6-allnodes\nff02::2 ip6-allrouters\n",
			hostname,
		),
	}

	for targetPath, payload := range identityFiles {
		sourceName := strings.TrimPrefix(strings.ReplaceAll(targetPath, "/", "_"), "_")
		sourcePath := filepath.Join(stagingDir, sourceName)
		if err := os.WriteFile(sourcePath, []byte(payload), 0o644); err != nil {
			return fmt.Errorf("write machine identity staging file for %q: %w", targetPath, err)
		}
		if err := replaceExt4File(ctx, imagePath, sourcePath, targetPath); err != nil {
			return err
		}
	}

	return nil
}

func replaceExt4File(ctx context.Context, imagePath string, sourcePath string, targetPath string) error {
	return replaceExt4FileMode(ctx, imagePath, sourcePath, targetPath, "")
}

func replaceExt4FileMode(ctx context.Context, imagePath string, sourcePath string, targetPath string, mode string) error {
	quotedSource := debugFSQuote(sourcePath)
	quotedTarget := debugFSQuote(targetPath)
	_ = runDebugFS(ctx, imagePath, fmt.Sprintf("rm %s", quotedTarget))
	if err := runDebugFS(ctx, imagePath, fmt.Sprintf("write %s %s", quotedSource, quotedTarget)); err != nil {
		return fmt.Errorf("inject %q into %q: %w", targetPath, imagePath, err)
	}
	if mode != "" {
		if err := runDebugFS(ctx, imagePath, fmt.Sprintf("set_inode_field %s mode 0%s", quotedTarget, mode)); err != nil {
			return fmt.Errorf("set mode on %q in %q: %w", targetPath, imagePath, err)
		}
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
		ID:                record.ID,
		Artifact:          record.Artifact,
		SystemVolumeID:    record.SystemVolumeID,
		UserVolumeIDs:     append([]contracthost.VolumeID(nil), record.UserVolumeIDs...),
		RuntimeHost:       record.RuntimeHost,
		Ports:             append([]contracthost.MachinePort(nil), record.Ports...),
		GuestSSHPublicKey: record.GuestSSHPublicKey,
		Phase:             record.Phase,
		Error:             record.Error,
		CreatedAt:         record.CreatedAt,
		StartedAt:         record.StartedAt,
	}
}

func publishedPortToContract(record model.PublishedPortRecord) contracthost.PublishedPort {
	return contracthost.PublishedPort{
		ID:        record.ID,
		MachineID: record.MachineID,
		Name:      record.Name,
		Port:      record.Port,
		HostPort:  record.HostPort,
		Protocol:  record.Protocol,
		CreatedAt: record.CreatedAt,
	}
}

func machineToRuntimeState(record model.MachineRecord) firecracker.MachineState {
	phase := firecracker.PhaseStopped
	switch record.Phase {
	case contracthost.MachinePhaseStarting:
		phase = firecracker.PhaseRunning
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
	if config.Hostname != "" {
		hostname := strings.TrimSpace(config.Hostname)
		if hostname == "" {
			return fmt.Errorf("guest_config.hostname is required")
		}
		if len(hostname) > 63 {
			return fmt.Errorf("guest_config.hostname must be 63 characters or fewer")
		}
		if strings.ContainsAny(hostname, "/\\") {
			return fmt.Errorf("guest_config.hostname must not contain path separators")
		}
		if strings.ContainsAny(hostname, " \t\r\n") {
			return fmt.Errorf("guest_config.hostname must not contain whitespace")
		}
	}
	for i, key := range config.AuthorizedKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("guest_config.authorized_keys[%d] is required", i)
		}
	}
	for i, key := range config.TrustedUserCAKeys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("guest_config.trusted_user_ca_keys[%d] is required", i)
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

func validateSnapshotID(snapshotID contracthost.SnapshotID) error {
	if strings.TrimSpace(string(snapshotID)) == "" {
		return fmt.Errorf("snapshot_id is required")
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
		_ = dir.Close()
		return fmt.Errorf("sync dir %q: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close dir %q: %w", path, err)
	}
	return nil
}

func repairDirtyFilesystem(diskPath string) {
	_ = exec.Command("/usr/sbin/e2fsck", "-fy", diskPath).Run()
}
