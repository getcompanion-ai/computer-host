package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

const (
	workspaceRootPath = "/home/node/workspace"
	nodeUID           = int64(1000)
	nodeGID           = int64(1000)
	defaultFileMode   = int64(0o644)
	defaultDirMode    = int64(0o755)
)

type workspacePath struct {
	guest   string
	display string
}

type debugFSEntry struct {
	Name      string
	GuestPath string
	Type      contracthost.FileEntryType
	SizeBytes int64
	Mode      int64
}

func (d *Daemon) FileOperation(ctx context.Context, id contracthost.MachineID, req contracthost.FileOperationRequest) (*contracthost.FileOperationResponse, error) {
	if err := validateMachineID(id); err != nil {
		return nil, err
	}

	unlock := d.lockMachine(id)
	defer unlock()

	record, err := d.store.GetMachine(ctx, id)
	if err != nil {
		return nil, err
	}

	switch record.Phase {
	case contracthost.MachinePhaseRunning:
		return d.performRunningFileOperation(ctx, *record, req)
	case contracthost.MachinePhaseStarting:
		return nil, fmt.Errorf("machine %q is starting; retry once runtime is ready", id)
	default:
		return d.performOfflineFileOperation(ctx, *record, req)
	}
}

func (d *Daemon) performRunningFileOperation(ctx context.Context, record model.MachineRecord, req contracthost.FileOperationRequest) (*contracthost.FileOperationResponse, error) {
	if strings.TrimSpace(record.RuntimeHost) == "" {
		return nil, fmt.Errorf("machine %q runtime host is unavailable", record.ID)
	}
	return d.withGuestSSH(ctx, record, func(client *ssh.Client) (*contracthost.FileOperationResponse, error) {
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			return nil, fmt.Errorf("open guest sftp session: %w", err)
		}
		defer func() {
			_ = sftpClient.Close()
		}()
		return d.handleRunningFileOperation(ctx, client, sftpClient, req)
	})
}

func (d *Daemon) handleRunningFileOperation(ctx context.Context, sshClient *ssh.Client, sftpClient *sftp.Client, req contracthost.FileOperationRequest) (*contracthost.FileOperationResponse, error) {
	switch req.Operation {
	case contracthost.FileOperationReadText:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.runningReadFile(sftpClient, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Content: string(payload)}, nil
	case contracthost.FileOperationReadBytes:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.runningReadFile(sftpClient, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{ContentBase64: base64.StdEncoding.EncodeToString(payload)}, nil
	case contracthost.FileOperationWriteText:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.runningWriteFile(sftpClient, target, []byte(req.Content), coalesceMode(req.Mode, defaultFileMode)); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationWriteBytes:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := decodeBase64Payload(req.ContentBase64)
		if err != nil {
			return nil, err
		}
		if err := d.runningWriteFile(sftpClient, target, payload, coalesceMode(req.Mode, defaultFileMode)); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationRemove:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.runningRemovePath(sftpClient, target, req.Recursive); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationList:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		entries, err := d.runningListPath(sftpClient, target, req.Recursive)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Entries: entries}, nil
	case contracthost.FileOperationStat:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		stat, err := d.runningStatPath(sftpClient, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Stat: &stat}, nil
	case contracthost.FileOperationExists:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		exists, err := d.runningExists(sftpClient, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Exists: &exists}, nil
	case contracthost.FileOperationPatch:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		version, err := d.runningPatchFile(sftpClient, target, req)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Version: version}, nil
	case contracthost.FileOperationReadRange:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.runningReadFile(sftpClient, target)
		if err != nil {
			return nil, err
		}
		chunk, err := applyReadRange(payload, req.Offset, req.Length)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{ContentBase64: base64.StdEncoding.EncodeToString(chunk)}, nil
	case contracthost.FileOperationWriteRange:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		existing, err := d.runningReadFileIfExists(sftpClient, target)
		if err != nil {
			return nil, err
		}
		chunk, err := decodeBase64Payload(req.ContentBase64)
		if err != nil {
			return nil, err
		}
		updated, err := applyWriteRange(existing, req.Offset, chunk)
		if err != nil {
			return nil, err
		}
		if err := d.runningWriteFile(sftpClient, target, updated, defaultFileMode); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationMkdir:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.runningMkdir(sftpClient, target, req.Recursive); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationMove:
		from, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		to, err := resolveWorkspacePath(req.To)
		if err != nil {
			return nil, err
		}
		if err := d.runningMovePath(sftpClient, from, to); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationGrep:
		matches, err := d.runningGrep(sshClient, req)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Matches: matches}, nil
	default:
		return nil, fmt.Errorf("unsupported file operation %q", req.Operation)
	}
}

func (d *Daemon) performOfflineFileOperation(ctx context.Context, record model.MachineRecord, req contracthost.FileOperationRequest) (*contracthost.FileOperationResponse, error) {
	imagePath, err := d.offlineWorkspaceImagePath(ctx, record)
	if err != nil {
		return nil, err
	}
	switch req.Operation {
	case contracthost.FileOperationReadText:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.ext4ReadFile(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Content: string(payload)}, nil
	case contracthost.FileOperationReadBytes:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.ext4ReadFile(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{ContentBase64: base64.StdEncoding.EncodeToString(payload)}, nil
	case contracthost.FileOperationWriteText:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.ext4WriteFile(ctx, imagePath, target, []byte(req.Content), coalesceMode(req.Mode, defaultFileMode)); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationWriteBytes:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := decodeBase64Payload(req.ContentBase64)
		if err != nil {
			return nil, err
		}
		if err := d.ext4WriteFile(ctx, imagePath, target, payload, coalesceMode(req.Mode, defaultFileMode)); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationRemove:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.ext4RemovePath(ctx, imagePath, target, req.Recursive); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationList:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		entries, err := d.ext4ListPath(ctx, imagePath, target, req.Recursive)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Entries: entries}, nil
	case contracthost.FileOperationStat:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		stat, err := d.ext4StatPath(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Stat: &stat}, nil
	case contracthost.FileOperationExists:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		exists, err := d.ext4Exists(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Exists: &exists}, nil
	case contracthost.FileOperationPatch:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		version, err := d.ext4PatchFile(ctx, imagePath, target, req)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Version: version}, nil
	case contracthost.FileOperationReadRange:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		payload, err := d.ext4ReadFile(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		chunk, err := applyReadRange(payload, req.Offset, req.Length)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{ContentBase64: base64.StdEncoding.EncodeToString(chunk)}, nil
	case contracthost.FileOperationWriteRange:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		existing, err := d.ext4ReadFileIfExists(ctx, imagePath, target)
		if err != nil {
			return nil, err
		}
		chunk, err := decodeBase64Payload(req.ContentBase64)
		if err != nil {
			return nil, err
		}
		updated, err := applyWriteRange(existing, req.Offset, chunk)
		if err != nil {
			return nil, err
		}
		if err := d.ext4WriteFile(ctx, imagePath, target, updated, defaultFileMode); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationMkdir:
		target, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		if err := d.ext4Mkdir(ctx, imagePath, target, req.Recursive); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationMove:
		from, err := resolveWorkspacePath(req.Path)
		if err != nil {
			return nil, err
		}
		to, err := resolveWorkspacePath(req.To)
		if err != nil {
			return nil, err
		}
		if err := d.ext4MovePath(ctx, imagePath, from, to); err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{}, nil
	case contracthost.FileOperationGrep:
		matches, err := d.ext4Grep(ctx, imagePath, req)
		if err != nil {
			return nil, err
		}
		return &contracthost.FileOperationResponse{Matches: matches}, nil
	default:
		return nil, fmt.Errorf("unsupported file operation %q", req.Operation)
	}
}

func (d *Daemon) offlineWorkspaceImagePath(ctx context.Context, record model.MachineRecord) (string, error) {
	workspaceVolume, err := d.workspaceVolumeForMachine(ctx, record)
	if err == nil {
		return workspaceVolume.Path, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return d.systemVolumePath(record.ID), nil
	}
	return "", err
}

func (d *Daemon) withGuestSSH(ctx context.Context, record model.MachineRecord, fn func(*ssh.Client) (*contracthost.FileOperationResponse, error)) (*contracthost.FileOperationResponse, error) {
	config, err := d.guestSSHClientConfig(record.GuestSSHPublicKey)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(record.RuntimeHost, strconv.Itoa(int(defaultSSHPort))))
	if err != nil {
		return nil, fmt.Errorf("dial guest ssh: %w", err)
	}

	clientConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(record.RuntimeHost, strconv.Itoa(int(defaultSSHPort))), config)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake guest ssh: %w", err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer func() {
		_ = client.Close()
	}()
	return fn(client)
}

func (d *Daemon) guestSSHClientConfig(guestHostKey string) (*ssh.ClientConfig, error) {
	privateKey, err := os.ReadFile(d.backendSSHPrivateKeyPath())
	if err != nil {
		return nil, fmt.Errorf("read backend ssh private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse backend ssh private key: %w", err)
	}

	callback := ssh.InsecureIgnoreHostKey()
	if strings.TrimSpace(guestHostKey) != "" {
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(guestHostKey)))
		if err != nil {
			return nil, fmt.Errorf("parse guest ssh public key: %w", err)
		}
		want := parsed.Marshal()
		callback = func(_ string, _ net.Addr, presented ssh.PublicKey) error {
			if bytesEqual(want, presented.Marshal()) {
				return nil
			}
			return fmt.Errorf("guest ssh host key mismatch")
		}
	}

	return &ssh.ClientConfig{
		User:            "node",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout:         2 * time.Second,
		HostKeyCallback: callback,
	}, nil
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func resolveWorkspacePath(raw string) (workspacePath, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = "."
	}
	if strings.ContainsRune(value, 0) {
		return workspacePath{}, fmt.Errorf("path must not contain NUL bytes")
	}
	if strings.ContainsAny(value, "\n\r") {
		return workspacePath{}, fmt.Errorf("path must not contain newline characters")
	}

	var guest string
	switch {
	case value == ".":
		guest = workspaceRootPath
	case strings.HasPrefix(value, workspaceRootPath):
		guest = path.Clean(value)
	case strings.HasPrefix(value, "/"):
		return workspacePath{}, fmt.Errorf("path %q must stay inside %s", raw, workspaceRootPath)
	default:
		cleaned := path.Clean(value)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return workspacePath{}, fmt.Errorf("path %q escapes the workspace root", raw)
		}
		if cleaned == "." {
			guest = workspaceRootPath
		} else {
			guest = path.Join(workspaceRootPath, cleaned)
		}
	}

	if guest != workspaceRootPath && !strings.HasPrefix(guest, workspaceRootPath+"/") {
		return workspacePath{}, fmt.Errorf("path %q must stay inside %s", raw, workspaceRootPath)
	}
	return workspacePath{guest: guest, display: displayPathFromGuest(guest)}, nil
}

func displayPathFromGuest(guest string) string {
	cleaned := path.Clean(guest)
	if cleaned == workspaceRootPath {
		return "."
	}
	trimmed := strings.TrimPrefix(cleaned, workspaceRootPath+"/")
	if trimmed == cleaned || trimmed == "" {
		return "."
	}
	return trimmed
}

func coalesceMode(mode *int64, fallback int64) int64 {
	if mode == nil || *mode <= 0 {
		return fallback
	}
	return *mode
}

func decodeBase64Payload(payload string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decode base64 payload: %w", err)
	}
	return data, nil
}

func applyReadRange(payload []byte, offset, length int64) ([]byte, error) {
	if offset < 0 {
		return nil, fmt.Errorf("offset must be non-negative")
	}
	if length < 0 {
		return nil, fmt.Errorf("length must be non-negative")
	}
	if offset >= int64(len(payload)) {
		return []byte{}, nil
	}
	if length == 0 {
		return []byte{}, nil
	}
	end := offset + length
	if end > int64(len(payload)) {
		end = int64(len(payload))
	}
	return append([]byte(nil), payload[offset:end]...), nil
}

func applyWriteRange(existing []byte, offset int64, chunk []byte) ([]byte, error) {
	if offset < 0 {
		return nil, fmt.Errorf("offset must be non-negative")
	}
	required := int(offset) + len(chunk)
	if required < 0 {
		return nil, fmt.Errorf("range write exceeds supported size")
	}
	if len(existing) < required {
		expanded := make([]byte, required)
		copy(expanded, existing)
		existing = expanded
	} else {
		existing = append([]byte(nil), existing...)
	}
	copy(existing[int(offset):], chunk)
	return existing, nil
}

func applyStructuredPatch(input string, req contracthost.FileOperationRequest) (string, error) {
	if req.SetContents != nil {
		return *req.SetContents, nil
	}
	updated := input
	for _, edit := range req.Edits {
		if edit.Find == "" {
			return "", fmt.Errorf("patch edits must include non-empty find text")
		}
		if !strings.Contains(updated, edit.Find) {
			return "", fmt.Errorf("patch target %q was not found", edit.Find)
		}
		updated = strings.ReplaceAll(updated, edit.Find, edit.Replace)
	}
	return updated, nil
}

func contentVersion(payload []byte) int64 {
	sum := sha256.Sum256(payload)
	return int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
}

func runningNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file"))
}

func debugfsNotExist(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "file not found") ||
		strings.Contains(message, "not found") ||
		strings.Contains(message, "does not exist")
}

func (d *Daemon) ensureRunningWorkspaceRoot(sftpClient *sftp.Client) error {
	info, err := sftpClient.Stat(workspaceRootPath)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", workspaceRootPath)
		}
		return nil
	}
	if !runningNotExist(err) {
		return fmt.Errorf("stat workspace root: %w", err)
	}
	if err := sftpClient.MkdirAll(workspaceRootPath); err != nil {
		return fmt.Errorf("create workspace root: %w", err)
	}
	return nil
}

func (d *Daemon) runningReadFile(sftpClient *sftp.Client, target workspacePath) ([]byte, error) {
	if target.guest == workspaceRootPath {
		return nil, fmt.Errorf("path %q is a directory", target.display)
	}
	payload, err := d.runningReadFileIfExists(sftpClient, target)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("path %q not found", target.display)
	}
	return payload, nil
}

func (d *Daemon) runningReadFileIfExists(sftpClient *sftp.Client, target workspacePath) ([]byte, error) {
	if target.guest == workspaceRootPath {
		if _, err := d.runningStatPath(sftpClient, target); err != nil {
			return nil, err
		}
		return []byte{}, nil
	}
	file, err := sftpClient.Open(target.guest)
	if err != nil {
		if runningNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %q: %w", target.display, err)
	}
	defer func() {
		_ = file.Close()
	}()
	payload, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", target.display, err)
	}
	return payload, nil
}

func (d *Daemon) runningWriteFile(sftpClient *sftp.Client, target workspacePath, payload []byte, mode int64) error {
	if target.guest == workspaceRootPath {
		return fmt.Errorf("cannot write workspace root directly")
	}
	if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil {
		return err
	}
	if err := sftpClient.MkdirAll(path.Dir(target.guest)); err != nil {
		return fmt.Errorf("create parent directory for %q: %w", target.display, err)
	}
	file, err := sftpClient.OpenFile(target.guest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("open %q for write: %w", target.display, err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %q: %w", target.display, err)
	}
	if err := file.Chmod(os.FileMode(mode)); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod %q: %w", target.display, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %q: %w", target.display, err)
	}
	return nil
}

func (d *Daemon) runningExists(sftpClient *sftp.Client, target workspacePath) (bool, error) {
	if target.guest == workspaceRootPath {
		return true, nil
	}
	_, err := sftpClient.Stat(target.guest)
	if err == nil {
		return true, nil
	}
	if runningNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat %q: %w", target.display, err)
}

func (d *Daemon) runningStatPath(sftpClient *sftp.Client, target workspacePath) (contracthost.FileStat, error) {
	if target.guest == workspaceRootPath {
		if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil && !runningNotExist(err) {
			return contracthost.FileStat{}, err
		}
		return contracthost.FileStat{
			Path: target.display,
			Type: contracthost.FileEntryTypeDirectory,
			Mode: defaultDirMode,
		}, nil
	}
	info, err := sftpClient.Stat(target.guest)
	if err != nil {
		if runningNotExist(err) {
			return contracthost.FileStat{}, fmt.Errorf("path %q not found", target.display)
		}
		return contracthost.FileStat{}, fmt.Errorf("stat %q: %w", target.display, err)
	}
	return contracthost.FileStat{
		Path:      target.display,
		Type:      fileEntryTypeFromMode(info.Mode()),
		SizeBytes: info.Size(),
		Mode:      int64(info.Mode().Perm()),
	}, nil
}

func fileEntryTypeFromMode(mode os.FileMode) contracthost.FileEntryType {
	if mode.IsDir() {
		return contracthost.FileEntryTypeDirectory
	}
	return contracthost.FileEntryTypeFile
}

func (d *Daemon) runningListPath(sftpClient *sftp.Client, target workspacePath, recursive bool) ([]contracthost.FileEntry, error) {
	if recursive {
		return d.runningListRecursive(sftpClient, target)
	}
	if target.guest == workspaceRootPath {
		if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil && !runningNotExist(err) {
			return nil, err
		}
	}
	infos, err := sftpClient.ReadDir(target.guest)
	if err != nil {
		if target.guest == workspaceRootPath && runningNotExist(err) {
			return []contracthost.FileEntry{}, nil
		}
		if runningNotExist(err) {
			return nil, fmt.Errorf("path %q not found", target.display)
		}
		return nil, fmt.Errorf("read directory %q: %w", target.display, err)
	}
	entries := make([]contracthost.FileEntry, 0, len(infos))
	for _, info := range infos {
		guestPath := path.Join(target.guest, info.Name())
		entries = append(entries, contracthost.FileEntry{
			Path:      displayPathFromGuest(guestPath),
			Name:      info.Name(),
			Type:      fileEntryTypeFromMode(info.Mode()),
			SizeBytes: info.Size(),
			Mode:      int64(info.Mode().Perm()),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (d *Daemon) runningListRecursive(sftpClient *sftp.Client, target workspacePath) ([]contracthost.FileEntry, error) {
	if target.guest == workspaceRootPath {
		if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil && !runningNotExist(err) {
			return nil, err
		}
	}
	walker := sftpClient.Walk(target.guest)
	entries := make([]contracthost.FileEntry, 0)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			if runningNotExist(err) && target.guest == workspaceRootPath {
				return []contracthost.FileEntry{}, nil
			}
			return nil, fmt.Errorf("walk %q: %w", target.display, err)
		}
		if walker.Path() == target.guest {
			continue
		}
		info := walker.Stat()
		if info == nil {
			continue
		}
		entries = append(entries, contracthost.FileEntry{
			Path:      displayPathFromGuest(walker.Path()),
			Name:      path.Base(walker.Path()),
			Type:      fileEntryTypeFromMode(info.Mode()),
			SizeBytes: info.Size(),
			Mode:      int64(info.Mode().Perm()),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (d *Daemon) runningRemovePath(sftpClient *sftp.Client, target workspacePath, recursive bool) error {
	if target.guest == workspaceRootPath {
		if !recursive {
			return fmt.Errorf("workspace root can only be removed recursively")
		}
		entries, err := d.runningListRecursive(sftpClient, target)
		if err != nil {
			return err
		}
		for i := len(entries) - 1; i >= 0; i-- {
			entry := entries[i]
			child, _ := resolveWorkspacePath(entry.Path)
			if entry.Type == contracthost.FileEntryTypeDirectory {
				if err := sftpClient.RemoveDirectory(child.guest); err != nil && !runningNotExist(err) {
					return fmt.Errorf("remove directory %q: %w", entry.Path, err)
				}
			} else {
				if err := sftpClient.Remove(child.guest); err != nil && !runningNotExist(err) {
					return fmt.Errorf("remove file %q: %w", entry.Path, err)
				}
			}
		}
		return nil
	}
	info, err := sftpClient.Stat(target.guest)
	if err != nil {
		if runningNotExist(err) {
			return fmt.Errorf("path %q not found", target.display)
		}
		return fmt.Errorf("stat %q: %w", target.display, err)
	}
	if !info.IsDir() {
		if err := sftpClient.Remove(target.guest); err != nil {
			return fmt.Errorf("remove %q: %w", target.display, err)
		}
		return nil
	}
	if !recursive {
		return fmt.Errorf("path %q is a directory; recursive removal is required", target.display)
	}
	entries, err := d.runningListRecursive(sftpClient, target)
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		child, _ := resolveWorkspacePath(entry.Path)
		if entry.Type == contracthost.FileEntryTypeDirectory {
			if err := sftpClient.RemoveDirectory(child.guest); err != nil && !runningNotExist(err) {
				return fmt.Errorf("remove directory %q: %w", entry.Path, err)
			}
		} else {
			if err := sftpClient.Remove(child.guest); err != nil && !runningNotExist(err) {
				return fmt.Errorf("remove file %q: %w", entry.Path, err)
			}
		}
	}
	if err := sftpClient.RemoveDirectory(target.guest); err != nil && !runningNotExist(err) {
		return fmt.Errorf("remove directory %q: %w", target.display, err)
	}
	return nil
}

func (d *Daemon) runningPatchFile(sftpClient *sftp.Client, target workspacePath, req contracthost.FileOperationRequest) (int64, error) {
	current, err := d.runningReadFile(sftpClient, target)
	if err != nil {
		return 0, err
	}
	updated, err := applyStructuredPatch(string(current), req)
	if err != nil {
		return 0, err
	}
	mode := defaultFileMode
	if info, statErr := sftpClient.Stat(target.guest); statErr == nil {
		mode = int64(info.Mode().Perm())
	}
	if err := d.runningWriteFile(sftpClient, target, []byte(updated), mode); err != nil {
		return 0, err
	}
	return contentVersion([]byte(updated)), nil
}

func (d *Daemon) runningMkdir(sftpClient *sftp.Client, target workspacePath, recursive bool) error {
	if target.guest == workspaceRootPath {
		return d.ensureRunningWorkspaceRoot(sftpClient)
	}
	if recursive {
		if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil {
			return err
		}
		if err := sftpClient.MkdirAll(target.guest); err != nil {
			return fmt.Errorf("mkdir -p %q: %w", target.display, err)
		}
		return nil
	}
	if err := sftpClient.Mkdir(target.guest); err != nil {
		return fmt.Errorf("mkdir %q: %w", target.display, err)
	}
	return nil
}

func (d *Daemon) runningMovePath(sftpClient *sftp.Client, from, to workspacePath) error {
	if from.guest == workspaceRootPath {
		return fmt.Errorf("cannot move the workspace root")
	}
	if err := d.ensureRunningWorkspaceRoot(sftpClient); err != nil {
		return err
	}
	if err := sftpClient.MkdirAll(path.Dir(to.guest)); err != nil {
		return fmt.Errorf("create destination parent for %q: %w", to.display, err)
	}
	if err := sftpClient.Rename(from.guest, to.guest); err != nil {
		return fmt.Errorf("move %q to %q: %w", from.display, to.display, err)
	}
	return nil
}

func (d *Daemon) runningGrep(client *ssh.Client, req contracthost.FileOperationRequest) ([]contracthost.FileGrepMatch, error) {
	target, err := resolveWorkspacePath(req.Path)
	if err != nil {
		return nil, err
	}
	commandParts := []string{"cd", shellSingleQuote(workspaceRootPath), "&&", "rg", "--json", "--line-number", "--color", "never"}
	if !req.Regex {
		commandParts = append(commandParts, "-F")
	}
	if req.CaseInsensitive {
		commandParts = append(commandParts, "-i")
	}
	if req.MaxMatches > 0 {
		commandParts = append(commandParts, "-m", strconv.Itoa(req.MaxMatches))
	}
	commandParts = append(commandParts, "--", shellSingleQuote(req.Pattern))
	if target.display != "." {
		commandParts = append(commandParts, shellSingleQuote(target.display))
	} else {
		commandParts = append(commandParts, ".")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open ssh session for grep: %w", err)
	}
	defer func() {
		_ = session.Close()
	}()
	output, runErr := session.CombinedOutput(strings.Join(commandParts, " "))
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitStatus() == 1 {
			return []contracthost.FileGrepMatch{}, nil
		}
		return nil, fmt.Errorf("run grep: %w: %s", runErr, strings.TrimSpace(string(output)))
	}

	type rgEvent struct {
		Type string `json:"type"`
		Data struct {
			Path struct {
				Text string `json:"text"`
			} `json:"path"`
			Lines struct {
				Text string `json:"text"`
			} `json:"lines"`
			LineNumber int `json:"line_number"`
			Submatches []struct {
				Match struct {
					Text string `json:"text"`
				} `json:"match"`
			} `json:"submatches"`
		} `json:"data"`
	}

	matches := make([]contracthost.FileGrepMatch, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var event rgEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type != "match" {
			continue
		}
		matchText := strings.TrimRight(event.Data.Lines.Text, "\n")
		if len(event.Data.Submatches) > 0 && event.Data.Submatches[0].Match.Text != "" {
			matchText = event.Data.Submatches[0].Match.Text
		}
		matches = append(matches, contracthost.FileGrepMatch{
			Path:  path.Clean(event.Data.Path.Text),
			Line:  event.Data.LineNumber,
			Match: matchText,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan grep output: %w", err)
	}
	return matches, nil
}

func (d *Daemon) ensureOfflineWorkspaceRoot(ctx context.Context, imagePath string) error {
	root, err := d.ext4StatRaw(ctx, imagePath, workspaceRootPath)
	if err == nil {
		if root.Type != contracthost.FileEntryTypeDirectory {
			return fmt.Errorf("%s is not a directory", workspaceRootPath)
		}
		return nil
	}
	if !debugfsNotExist(err) {
		return err
	}
	return d.ext4EnsureDirectory(ctx, imagePath, workspaceRootPath)
}

func (d *Daemon) ext4ReadFile(ctx context.Context, imagePath string, target workspacePath) ([]byte, error) {
	if target.guest == workspaceRootPath {
		return nil, fmt.Errorf("path %q is a directory", target.display)
	}
	payload, err := d.ext4ReadFileIfExists(ctx, imagePath, target)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("path %q not found", target.display)
	}
	return payload, nil
}

func (d *Daemon) ext4ReadFileIfExists(ctx context.Context, imagePath string, target workspacePath) ([]byte, error) {
	output, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("cat %s", debugFSQuote(target.guest)), false)
	if err != nil {
		if debugfsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return []byte(output), nil
}

func (d *Daemon) ext4WriteFile(ctx context.Context, imagePath string, target workspacePath, payload []byte, mode int64) error {
	if target.guest == workspaceRootPath {
		return fmt.Errorf("cannot write workspace root directly")
	}
	if err := d.ensureOfflineWorkspaceRoot(ctx, imagePath); err != nil {
		return err
	}
	if err := d.ext4EnsureDirectory(ctx, imagePath, path.Dir(target.guest)); err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp(filepath.Dir(imagePath), "computer-file-*")
	if err != nil {
		return fmt.Errorf("create staging dir for %q: %w", target.display, err)
	}
	defer func() {
		_ = os.RemoveAll(stagingDir)
	}()
	stagingPath := filepath.Join(stagingDir, "payload")
	if err := os.WriteFile(stagingPath, payload, 0o600); err != nil {
		return fmt.Errorf("write staging file for %q: %w", target.display, err)
	}
	if err := replaceExt4FileMode(ctx, imagePath, stagingPath, target.guest, ext4FileModeValue(mode)); err != nil {
		return err
	}
	if err := d.ext4SetOwnership(ctx, imagePath, target.guest, nodeUID, nodeGID); err != nil {
		return err
	}
	return nil
}

func ext4FileModeValue(mode int64) string {
	return fmt.Sprintf("100%03o", mode&0o777)
}

func ext4DirModeValue(mode int64) string {
	return fmt.Sprintf("040%03o", mode&0o777)
}

func (d *Daemon) ext4SetOwnership(ctx context.Context, imagePath, guestPath string, uid, gid int64) error {
	if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("set_inode_field %s uid %d", debugFSQuote(guestPath), uid), true); err != nil {
		return fmt.Errorf("set uid on %q: %w", guestPath, err)
	}
	if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("set_inode_field %s gid %d", debugFSQuote(guestPath), gid), true); err != nil {
		return fmt.Errorf("set gid on %q: %w", guestPath, err)
	}
	return nil
}

func (d *Daemon) ext4EnsureDirectory(ctx context.Context, imagePath, guestPath string) error {
	cleaned := path.Clean(guestPath)
	if cleaned == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	current := ""
	for _, part := range parts {
		current = path.Join(current, "/"+part)
		if _, err := d.ext4StatRaw(ctx, imagePath, current); err == nil {
			continue
		} else if !debugfsNotExist(err) {
			return err
		}
		if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("mkdir %s", debugFSQuote(current)), true); err != nil {
			return fmt.Errorf("mkdir %q: %w", current, err)
		}
		if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("set_inode_field %s mode 0%s", debugFSQuote(current), ext4DirModeValue(defaultDirMode)), true); err != nil {
			return fmt.Errorf("set mode on %q: %w", current, err)
		}
		if err := d.ext4SetOwnership(ctx, imagePath, current, nodeUID, nodeGID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) ext4Exists(ctx context.Context, imagePath string, target workspacePath) (bool, error) {
	if target.guest == workspaceRootPath {
		return true, nil
	}
	_, err := d.ext4StatRaw(ctx, imagePath, target.guest)
	if err == nil {
		return true, nil
	}
	if debugfsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (d *Daemon) ext4StatPath(ctx context.Context, imagePath string, target workspacePath) (contracthost.FileStat, error) {
	if target.guest == workspaceRootPath {
		return contracthost.FileStat{Path: ".", Type: contracthost.FileEntryTypeDirectory, Mode: defaultDirMode}, nil
	}
	entry, err := d.ext4StatRaw(ctx, imagePath, target.guest)
	if err != nil {
		if debugfsNotExist(err) {
			return contracthost.FileStat{}, fmt.Errorf("path %q not found", target.display)
		}
		return contracthost.FileStat{}, err
	}
	return contracthost.FileStat{
		Path:      displayPathFromGuest(entry.GuestPath),
		Type:      entry.Type,
		SizeBytes: entry.SizeBytes,
		Mode:      entry.Mode & 0o777,
	}, nil
}

func (d *Daemon) ext4StatRaw(ctx context.Context, imagePath, guestPath string) (debugFSEntry, error) {
	cleaned := path.Clean(guestPath)
	if cleaned == "/" {
		return debugFSEntry{Name: "/", GuestPath: "/", Type: contracthost.FileEntryTypeDirectory, Mode: defaultDirMode}, nil
	}
	parent := path.Dir(cleaned)
	base := path.Base(cleaned)
	entries, err := d.ext4ReadDir(ctx, imagePath, parent)
	if err != nil {
		return debugFSEntry{}, err
	}
	for _, entry := range entries {
		if entry.Name == base {
			return entry, nil
		}
	}
	return debugFSEntry{}, fmt.Errorf("path %q not found", displayPathFromGuest(cleaned))
}

func (d *Daemon) ext4ReadDir(ctx context.Context, imagePath, guestDir string) ([]debugFSEntry, error) {
	output, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("ls -p %s", debugFSQuote(guestDir)), false)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(output, "\n")
	entries := make([]debugFSEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "/") {
			continue
		}
		entry, ok := parseDebugFSEntry(guestDir, line)
		if !ok {
			continue
		}
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func parseDebugFSEntry(parent, line string) (debugFSEntry, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(line), "/")
	trimmed = strings.TrimSuffix(trimmed, "/")
	parts := strings.SplitN(trimmed, "/", 6)
	if len(parts) != 6 {
		return debugFSEntry{}, false
	}
	mode, err := strconv.ParseInt(parts[1], 8, 64)
	if err != nil {
		return debugFSEntry{}, false
	}
	sizeBytes := int64(0)
	entryType := contracthost.FileEntryTypeDirectory
	if parts[5] != "" {
		sizeBytes, err = strconv.ParseInt(parts[5], 10, 64)
		if err != nil {
			sizeBytes = 0
		}
		entryType = contracthost.FileEntryTypeFile
	}
	name := parts[4]
	guestPath := path.Join(path.Clean(parent), name)
	return debugFSEntry{
		Name:      name,
		GuestPath: guestPath,
		Type:      entryType,
		SizeBytes: sizeBytes,
		Mode:      mode,
	}, true
}

func (d *Daemon) ext4ListPath(ctx context.Context, imagePath string, target workspacePath, recursive bool) ([]contracthost.FileEntry, error) {
	if target.guest == workspaceRootPath {
		if _, err := d.ext4StatRaw(ctx, imagePath, target.guest); err != nil && !debugfsNotExist(err) {
			return nil, err
		}
	}
	if recursive {
		return d.ext4ListRecursive(ctx, imagePath, target)
	}
	entries, err := d.ext4ReadDir(ctx, imagePath, target.guest)
	if err != nil {
		if target.guest == workspaceRootPath && debugfsNotExist(err) {
			return []contracthost.FileEntry{}, nil
		}
		if debugfsNotExist(err) {
			return nil, fmt.Errorf("path %q not found", target.display)
		}
		return nil, err
	}
	response := make([]contracthost.FileEntry, 0, len(entries))
	for _, entry := range entries {
		response = append(response, contracthost.FileEntry{
			Path:      displayPathFromGuest(entry.GuestPath),
			Name:      entry.Name,
			Type:      entry.Type,
			SizeBytes: entry.SizeBytes,
			Mode:      entry.Mode & 0o777,
		})
	}
	sort.Slice(response, func(i, j int) bool { return response[i].Path < response[j].Path })
	return response, nil
}

func (d *Daemon) ext4ListRecursive(ctx context.Context, imagePath string, target workspacePath) ([]contracthost.FileEntry, error) {
	rootEntries, err := d.ext4ReadDir(ctx, imagePath, target.guest)
	if err != nil {
		if target.guest == workspaceRootPath && debugfsNotExist(err) {
			return []contracthost.FileEntry{}, nil
		}
		return nil, err
	}
	results := make([]contracthost.FileEntry, 0)
	var walk func([]debugFSEntry) error
	walk = func(entries []debugFSEntry) error {
		for _, entry := range entries {
			results = append(results, contracthost.FileEntry{
				Path:      displayPathFromGuest(entry.GuestPath),
				Name:      entry.Name,
				Type:      entry.Type,
				SizeBytes: entry.SizeBytes,
				Mode:      entry.Mode & 0o777,
			})
			if entry.Type == contracthost.FileEntryTypeDirectory {
				children, err := d.ext4ReadDir(ctx, imagePath, entry.GuestPath)
				if err != nil {
					return err
				}
				if err := walk(children); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(rootEntries); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	return results, nil
}

func (d *Daemon) ext4RemovePath(ctx context.Context, imagePath string, target workspacePath, recursive bool) error {
	if target.guest == workspaceRootPath {
		if !recursive {
			return fmt.Errorf("workspace root can only be removed recursively")
		}
		entries, err := d.ext4ListRecursive(ctx, imagePath, target)
		if err != nil {
			return err
		}
		for i := len(entries) - 1; i >= 0; i-- {
			entry := entries[i]
			child, _ := resolveWorkspacePath(entry.Path)
			if entry.Type == contracthost.FileEntryTypeDirectory {
				if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rmdir %s", debugFSQuote(child.guest)), true); err != nil && !debugfsNotExist(err) {
					return fmt.Errorf("remove directory %q: %w", entry.Path, err)
				}
			} else {
				if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rm %s", debugFSQuote(child.guest)), true); err != nil && !debugfsNotExist(err) {
					return fmt.Errorf("remove file %q: %w", entry.Path, err)
				}
			}
		}
		return nil
	}
	entry, err := d.ext4StatRaw(ctx, imagePath, target.guest)
	if err != nil {
		if debugfsNotExist(err) {
			return fmt.Errorf("path %q not found", target.display)
		}
		return err
	}
	if entry.Type == contracthost.FileEntryTypeFile {
		if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rm %s", debugFSQuote(target.guest)), true); err != nil {
			return fmt.Errorf("remove file %q: %w", target.display, err)
		}
		return nil
	}
	if !recursive {
		return fmt.Errorf("path %q is a directory; recursive removal is required", target.display)
	}
	entries, err := d.ext4ListRecursive(ctx, imagePath, target)
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		child, _ := resolveWorkspacePath(entries[i].Path)
		if entries[i].Type == contracthost.FileEntryTypeDirectory {
			if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rmdir %s", debugFSQuote(child.guest)), true); err != nil && !debugfsNotExist(err) {
				return fmt.Errorf("remove directory %q: %w", entries[i].Path, err)
			}
		} else {
			if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rm %s", debugFSQuote(child.guest)), true); err != nil && !debugfsNotExist(err) {
				return fmt.Errorf("remove file %q: %w", entries[i].Path, err)
			}
		}
	}
	if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("rmdir %s", debugFSQuote(target.guest)), true); err != nil && !debugfsNotExist(err) {
		return fmt.Errorf("remove directory %q: %w", target.display, err)
	}
	return nil
}

func (d *Daemon) ext4PatchFile(ctx context.Context, imagePath string, target workspacePath, req contracthost.FileOperationRequest) (int64, error) {
	current, err := d.ext4ReadFile(ctx, imagePath, target)
	if err != nil {
		return 0, err
	}
	updated, err := applyStructuredPatch(string(current), req)
	if err != nil {
		return 0, err
	}
	mode := defaultFileMode
	if existing, statErr := d.ext4StatRaw(ctx, imagePath, target.guest); statErr == nil {
		mode = existing.Mode & 0o777
	}
	if err := d.ext4WriteFile(ctx, imagePath, target, []byte(updated), mode); err != nil {
		return 0, err
	}
	return contentVersion([]byte(updated)), nil
}

func (d *Daemon) ext4Mkdir(ctx context.Context, imagePath string, target workspacePath, recursive bool) error {
	if target.guest == workspaceRootPath {
		return d.ensureOfflineWorkspaceRoot(ctx, imagePath)
	}
	if recursive {
		return d.ext4EnsureDirectory(ctx, imagePath, target.guest)
	}
	if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("mkdir %s", debugFSQuote(target.guest)), true); err != nil {
		return fmt.Errorf("mkdir %q: %w", target.display, err)
	}
	if _, err := runDebugFSOutput(ctx, imagePath, fmt.Sprintf("set_inode_field %s mode 0%s", debugFSQuote(target.guest), ext4DirModeValue(defaultDirMode)), true); err != nil {
		return fmt.Errorf("set mode on %q: %w", target.display, err)
	}
	return d.ext4SetOwnership(ctx, imagePath, target.guest, nodeUID, nodeGID)
}

func (d *Daemon) ext4MovePath(ctx context.Context, imagePath string, from, to workspacePath) error {
	if from.guest == workspaceRootPath {
		return fmt.Errorf("cannot move the workspace root")
	}
	if from.guest == to.guest {
		return fmt.Errorf("cannot move %q onto itself", from.display)
	}
	if workspacePathContains(from.guest, to.guest) {
		return fmt.Errorf("cannot move %q into itself", from.display)
	}
	entry, err := d.ext4StatRaw(ctx, imagePath, from.guest)
	if err != nil {
		return err
	}
	if entry.Type == contracthost.FileEntryTypeFile {
		payload, err := d.ext4ReadFile(ctx, imagePath, from)
		if err != nil {
			return err
		}
		if err := d.ext4WriteFile(ctx, imagePath, to, payload, entry.Mode&0o777); err != nil {
			return err
		}
		return d.ext4RemovePath(ctx, imagePath, from, false)
	}
	entries, err := d.ext4ListRecursive(ctx, imagePath, from)
	if err != nil {
		return err
	}
	if err := d.ext4EnsureDirectory(ctx, imagePath, to.guest); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return pathDepth(entries[i].Path) < pathDepth(entries[j].Path) })
	for _, listed := range entries {
		childFrom, _ := resolveWorkspacePath(listed.Path)
		relative := strings.TrimPrefix(childFrom.guest, from.guest+"/")
		childTo := workspacePath{
			guest:   path.Join(to.guest, relative),
			display: displayPathFromGuest(path.Join(to.guest, relative)),
		}
		if listed.Type == contracthost.FileEntryTypeDirectory {
			if err := d.ext4EnsureDirectory(ctx, imagePath, childTo.guest); err != nil {
				return err
			}
			continue
		}
		payload, err := d.ext4ReadFile(ctx, imagePath, childFrom)
		if err != nil {
			return err
		}
		if err := d.ext4WriteFile(ctx, imagePath, childTo, payload, listed.Mode); err != nil {
			return err
		}
	}
	return d.ext4RemovePath(ctx, imagePath, from, true)
}

func pathDepth(value string) int {
	trimmed := strings.Trim(value, "/")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "/") + 1
}

func workspacePathContains(parent, child string) bool {
	parent = path.Clean(parent)
	child = path.Clean(child)
	return strings.HasPrefix(child, parent+"/")
}

func (d *Daemon) ext4Grep(ctx context.Context, imagePath string, req contracthost.FileOperationRequest) ([]contracthost.FileGrepMatch, error) {
	target, err := resolveWorkspacePath(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := d.ext4ListRecursive(ctx, imagePath, target)
	if err != nil {
		if target.guest == workspaceRootPath && debugfsNotExist(err) {
			return []contracthost.FileGrepMatch{}, nil
		}
		return nil, err
	}
	var matcher func(string) bool
	if req.Regex {
		pattern := req.Pattern
		if req.CaseInsensitive {
			pattern = "(?i)" + pattern
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile regex %q: %w", req.Pattern, err)
		}
		matcher = compiled.MatchString
	} else if req.CaseInsensitive {
		needle := strings.ToLower(req.Pattern)
		matcher = func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }
	} else {
		matcher = func(line string) bool { return strings.Contains(line, req.Pattern) }
	}
	matches := make([]contracthost.FileGrepMatch, 0)
	for _, entry := range entries {
		if entry.Type != contracthost.FileEntryTypeFile {
			continue
		}
		child, _ := resolveWorkspacePath(entry.Path)
		payload, err := d.ext4ReadFile(ctx, imagePath, child)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(strings.NewReader(string(payload)))
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if matcher(line) {
				matches = append(matches, contracthost.FileGrepMatch{
					Path:  entry.Path,
					Line:  lineNumber,
					Match: line,
				})
				if req.MaxMatches > 0 && len(matches) >= req.MaxMatches {
					return matches, nil
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scan %q: %w", entry.Path, err)
		}
	}
	return matches, nil
}

func runDebugFSOutput(ctx context.Context, imagePath string, command string, write bool) (string, error) {
	args := []string{}
	if write {
		args = append(args, "-w")
	}
	args = append(args, "-R", command, imagePath)
	cmd := exec.CommandContext(ctx, "debugfs", args...)
	cmd.Env = append(os.Environ(), "DEBUGFS_PAGER=cat", "PAGER=cat")
	output, err := cmd.CombinedOutput()
	filtered := filterDebugFSOutput(string(output))
	if err != nil {
		return "", fmt.Errorf("debugfs %q on %q: %w: %s", command, imagePath, err, strings.TrimSpace(filtered))
	}
	return filtered, nil
}

func filterDebugFSOutput(output string) string {
	lines := strings.Split(output, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "debugfs ") || strings.HasPrefix(trimmed, "debugfs:") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func debugFSQuote(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
