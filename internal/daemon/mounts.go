package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func (d *Daemon) CreateMount(ctx context.Context, machineID contracthost.MachineID, req contracthost.CreateMountRequest) (*contracthost.CreateMountResponse, error) {
	if strings.TrimSpace(string(req.MountID)) == "" {
		return nil, fmt.Errorf("mount_id is required")
	}
	if req.Kind == "" {
		return nil, fmt.Errorf("kind is required")
	}
	if !isValidMountKind(req.Kind) {
		return nil, fmt.Errorf("unsupported mount kind %q", req.Kind)
	}
	if strings.TrimSpace(req.TargetPath) == "" {
		return nil, fmt.Errorf("target_path is required")
	}
	if !strings.HasPrefix(strings.TrimSpace(req.TargetPath), "/") {
		return nil, fmt.Errorf("target_path must be absolute")
	}
	if strings.TrimSpace(req.Config.Bucket) == "" {
		return nil, fmt.Errorf("config.bucket is required")
	}
	if req.Kind == contracthost.MountKindWebDAV && strings.TrimSpace(req.Config.Endpoint) == "" {
		return nil, fmt.Errorf("config.endpoint is required for webdav mounts")
	}
	if req.Kind == contracthost.MountKindR2 && strings.TrimSpace(req.Config.Endpoint) == "" {
		return nil, fmt.Errorf("config.endpoint is required for r2 mounts")
	}
	if strings.TrimSpace(req.Config.AccessKeyID) == "" {
		return nil, fmt.Errorf("config.access_key_id is required")
	}
	if strings.TrimSpace(req.Config.SecretAccessKey) == "" {
		return nil, fmt.Errorf("config.secret_access_key is required")
	}

	unlock := d.lockMachine(machineID)
	defer unlock()

	record, err := d.store.GetMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if record.Phase != contracthost.MachinePhaseRunning {
		return nil, fmt.Errorf("machine %q is not running", machineID)
	}
	if strings.TrimSpace(record.RuntimeHost) == "" {
		return nil, fmt.Errorf("machine %q runtime host is unavailable", machineID)
	}
	if _, err := d.store.GetMount(ctx, req.MountID); err == nil {
		return nil, fmt.Errorf("mount %q already exists", req.MountID)
	} else if err != nil && err != store.ErrNotFound {
		return nil, err
	}

	mount := model.MountRecord{
		ID:         req.MountID,
		MachineID:  machineID,
		Kind:       req.Kind,
		TargetPath: strings.TrimSpace(req.TargetPath),
		ReadOnly:   req.ReadOnly,
		Config:     req.Config,
		Status:     contracthost.MountStatusPending,
		CreatedAt:  time.Now().UTC(),
	}

	if err := d.store.CreateMount(ctx, mount); err != nil {
		return nil, err
	}

	if err := d.execGuestMountUnlocked(ctx, record.RuntimeHost, mount); err != nil {
		mount.Status = contracthost.MountStatusFailed
		mount.StatusMessage = err.Error()
		if updateErr := d.store.UpdateMount(ctx, mount); updateErr != nil {
			return nil, updateErr
		}
		return &contracthost.CreateMountResponse{Mount: mountToContract(mount)}, nil
	}

	mount.Status = contracthost.MountStatusMounted
	mount.StatusMessage = ""
	if err := d.store.UpdateMount(ctx, mount); err != nil {
		return nil, err
	}

	return &contracthost.CreateMountResponse{Mount: mountToContract(mount)}, nil
}

func (d *Daemon) ListMounts(ctx context.Context, machineID contracthost.MachineID) (*contracthost.ListMountsResponse, error) {
	mounts, err := d.store.ListMounts(ctx, machineID)
	if err != nil {
		return nil, err
	}
	response := &contracthost.ListMountsResponse{Mounts: make([]contracthost.Mount, 0, len(mounts))}
	for _, mount := range mounts {
		response.Mounts = append(response.Mounts, mountToContract(mount))
	}
	return response, nil
}

func (d *Daemon) DeleteMount(ctx context.Context, machineID contracthost.MachineID, mountID contracthost.MountID) error {
	unlock := d.lockMachine(machineID)
	defer unlock()

	record, err := d.store.GetMount(ctx, mountID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}
	if record.MachineID != machineID {
		return fmt.Errorf("mount %q does not belong to machine %q", mountID, machineID)
	}

	machine, err := d.store.GetMachine(ctx, machineID)
	if err != nil {
		return err
	}
	if machine.Phase == contracthost.MachinePhaseRunning && strings.TrimSpace(machine.RuntimeHost) != "" {
		active, err := d.verifyMountActiveUnlocked(ctx, machine.RuntimeHost, record.TargetPath)
		if err != nil {
			return fmt.Errorf("verify mount %q before unmount: %w", mountID, err)
		}
		if active {
			if err := d.execGuestUnmountUnlocked(ctx, machine.RuntimeHost, *record); err != nil {
				return fmt.Errorf("unmount %q: %w", mountID, err)
			}
		}
	}

	return d.store.DeleteMount(ctx, mountID)
}

func (d *Daemon) ensureMountsForMachine(ctx context.Context, machine model.MachineRecord) error {
	if machine.Phase != contracthost.MachinePhaseRunning || strings.TrimSpace(machine.RuntimeHost) == "" {
		return nil
	}
	mounts, err := d.store.ListMounts(ctx, machine.ID)
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if mount.Status == contracthost.MountStatusFailed {
			continue
		}
		if mount.Status == contracthost.MountStatusMounted {
			active, err := d.verifyMountActiveUnlocked(ctx, machine.RuntimeHost, mount.TargetPath)
			if err == nil && active {
				continue
			}
			mount.Status = contracthost.MountStatusPending
			mount.StatusMessage = ""
			if err := d.store.UpdateMount(ctx, mount); err != nil {
				return err
			}
		}
		if err := d.execGuestMountUnlocked(ctx, machine.RuntimeHost, mount); err != nil {
			mount.Status = contracthost.MountStatusFailed
			mount.StatusMessage = err.Error()
			if updateErr := d.store.UpdateMount(ctx, mount); updateErr != nil {
				return updateErr
			}
			continue
		}
		mount.Status = contracthost.MountStatusMounted
		mount.StatusMessage = ""
		if err := d.store.UpdateMount(ctx, mount); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) runGuestCommand(ctx context.Context, runtimeHost, user string, timeout time.Duration, command []string, env map[string]string) (*contracthost.ExecResponse, error) {
	if strings.TrimSpace(runtimeHost) == "" {
		return nil, fmt.Errorf("runtime host is empty")
	}
	cmd := command[0]
	args := command[1:]
	envs := env
	if envs == nil {
		envs = map[string]string{}
	}
	if _, hasPath := envs["PATH"]; !hasPath {
		envs["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	startReq := guestdStartRequest{
		Process: &guestdProcessConfig{
			Cmd:  cmd,
			Args: args,
			Envs: envs,
		},
	}

	start := time.Now()
	stdout, stderr, exitCode, err := d.callGuestdStart(execCtx, runtimeHost, user, timeout, startReq)
	if err != nil {
		return nil, err
	}

	return &contracthost.ExecResponse{
		ExitCode:   exitCode,
		Stdout:     stdout,
		Stderr:     stderr,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func (d *Daemon) execGuestMountUnlocked(ctx context.Context, runtimeHost string, mount model.MountRecord) error {
	if err := d.preflightMountUnlocked(ctx, runtimeHost, mount.TargetPath); err != nil {
		return err
	}

	rcloneArgs := buildRcloneArgs(mount)
	secret := mount.Config.SecretAccessKey
	if mount.Kind == contracthost.MountKindWebDAV {
		obscured, err := d.obscureRcloneSecret(ctx, runtimeHost, secret)
		if err != nil {
			return err
		}
		secret = obscured
	}
	env, err := buildRcloneConfigEnv(mount, secret)
	if err != nil {
		return err
	}

	resp, err := d.runGuestCommand(ctx, runtimeHost, "root", 30*time.Second, rcloneArgs, env)
	if err != nil {
		return fmt.Errorf("exec rclone mount: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("rclone mount failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

func (d *Daemon) execGuestUnmountUnlocked(ctx context.Context, runtimeHost string, mount model.MountRecord) error {
	script := `if command -v fusermount3 >/dev/null 2>&1; then exec fusermount3 -u "$1"; fi; exec fusermount -u "$1"`
	resp, err := d.runGuestCommand(ctx, runtimeHost, "root", 10*time.Second, []string{"sh", "-c", script, "sh", mount.TargetPath}, nil)
	if err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("fusermount failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

func (d *Daemon) obscureRcloneSecret(ctx context.Context, runtimeHost, value string) (string, error) {
	resp, err := d.runGuestCommand(ctx, runtimeHost, "root", 5*time.Second, []string{"rclone", "obscure", value}, nil)
	if err != nil {
		return "", fmt.Errorf("obscure webdav password: %w", err)
	}
	if resp.ExitCode != 0 {
		return "", fmt.Errorf("rclone obscure failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	obscured := strings.TrimSpace(resp.Stdout)
	if obscured == "" {
		return "", fmt.Errorf("rclone obscure returned an empty password")
	}
	return obscured, nil
}

func (d *Daemon) preflightMountUnlocked(ctx context.Context, runtimeHost, targetPath string) error {
	resp, err := d.runGuestCommand(ctx, runtimeHost, "root", 5*time.Second, []string{"test", "-e", "/dev/fuse"}, nil)
	if err != nil {
		return fmt.Errorf("check FUSE support: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("kernel missing FUSE support: /dev/fuse not found")
	}

	resp, err = d.runGuestCommand(ctx, runtimeHost, "root", 5*time.Second, []string{"which", "rclone"}, nil)
	if err != nil {
		return fmt.Errorf("check rclone: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("rclone not installed in guest image")
	}

	resp, err = d.runGuestCommand(ctx, runtimeHost, "root", 5*time.Second, []string{"mkdir", "-p", targetPath}, nil)
	if err != nil {
		return fmt.Errorf("create mount target: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("failed to create mount target %q: %s", targetPath, resp.Stderr)
	}
	return nil
}

func (d *Daemon) verifyMountActiveUnlocked(ctx context.Context, runtimeHost, targetPath string) (bool, error) {
	resp, err := d.runGuestCommand(ctx, runtimeHost, "root", 5*time.Second, []string{"mountpoint", "-q", targetPath}, nil)
	if err != nil {
		return false, err
	}
	return resp.ExitCode == 0, nil
}

func buildRcloneArgs(mount model.MountRecord) []string {
	args := []string{
		"rclone", "mount",
		fmt.Sprintf("remote:%s", strings.TrimSpace(mount.Config.Bucket)),
		mount.TargetPath,
		"--daemon",
		"--allow-other",
	}

	if mount.ReadOnly {
		args = append(args, "--read-only")
	}

	cacheMode := mount.Config.VFSCacheMode
	if cacheMode == "" {
		cacheMode = "writes"
	}
	args = append(args, "--vfs-cache-mode", cacheMode)

	args = append(args, mount.Config.ExtraFlags...)

	return args
}

func buildRcloneConfigEnv(mount model.MountRecord, secret string) (map[string]string, error) {
	env := map[string]string{}
	switch mount.Kind {
	case contracthost.MountKindR2, contracthost.MountKindS3:
		if mount.Kind == contracthost.MountKindR2 && strings.TrimSpace(mount.Config.Endpoint) == "" {
			return nil, fmt.Errorf("config.endpoint is required for r2 mounts")
		}
		env["RCLONE_CONFIG_REMOTE_TYPE"] = "s3"
		env["RCLONE_CONFIG_REMOTE_PROVIDER"] = rcloneS3Provider(mount.Kind)
		env["RCLONE_CONFIG_REMOTE_ACCESS_KEY_ID"] = mount.Config.AccessKeyID
		env["RCLONE_CONFIG_REMOTE_SECRET_ACCESS_KEY"] = secret
		env["RCLONE_CONFIG_REMOTE_ENDPOINT"] = mount.Config.Endpoint
		env["RCLONE_CONFIG_REMOTE_REGION"] = mount.Config.Region
	case contracthost.MountKindGCS:
		env["RCLONE_CONFIG_REMOTE_TYPE"] = "s3"
		env["RCLONE_CONFIG_REMOTE_PROVIDER"] = "GCS"
		env["RCLONE_CONFIG_REMOTE_ACCESS_KEY_ID"] = mount.Config.AccessKeyID
		env["RCLONE_CONFIG_REMOTE_SECRET_ACCESS_KEY"] = secret
		env["RCLONE_CONFIG_REMOTE_ENDPOINT"] = mount.Config.Endpoint
		if env["RCLONE_CONFIG_REMOTE_ENDPOINT"] == "" {
			env["RCLONE_CONFIG_REMOTE_ENDPOINT"] = "https://storage.googleapis.com"
		}
		env["RCLONE_CONFIG_REMOTE_REGION"] = mount.Config.Region
	case contracthost.MountKindWebDAV:
		if strings.TrimSpace(mount.Config.Endpoint) == "" {
			return nil, fmt.Errorf("config.endpoint is required for webdav mounts")
		}
		env["RCLONE_CONFIG_REMOTE_TYPE"] = "webdav"
		env["RCLONE_CONFIG_REMOTE_URL"] = mount.Config.Endpoint
		env["RCLONE_CONFIG_REMOTE_VENDOR"] = "other"
		env["RCLONE_CONFIG_REMOTE_USER"] = mount.Config.AccessKeyID
		env["RCLONE_CONFIG_REMOTE_PASS"] = secret
	default:
		return nil, fmt.Errorf("unsupported mount kind %q", mount.Kind)
	}
	return env, nil
}

func rcloneS3Provider(kind contracthost.MountKind) string {
	switch kind {
	case contracthost.MountKindR2:
		return "Cloudflare"
	case contracthost.MountKindS3:
		return "AWS"
	default:
		return ""
	}
}

func isValidMountKind(kind contracthost.MountKind) bool {
	switch kind {
	case contracthost.MountKindR2, contracthost.MountKindS3, contracthost.MountKindGCS, contracthost.MountKindWebDAV:
		return true
	default:
		return false
	}
}

func mountToContract(record model.MountRecord) contracthost.Mount {
	return contracthost.Mount{
		ID:            record.ID,
		MachineID:     record.MachineID,
		Kind:          record.Kind,
		TargetPath:    record.TargetPath,
		ReadOnly:      record.ReadOnly,
		Config:        record.Config,
		Status:        record.Status,
		StatusMessage: record.StatusMessage,
		CreatedAt:     record.CreatedAt,
	}
}
