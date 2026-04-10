package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCgroupVersion           = "2"
	defaultFirecrackerInitTimeout  = 10 * time.Second
	defaultFirecrackerLogLevel     = "Warning"
	defaultFirecrackerPollInterval = 10 * time.Millisecond
	defaultRootDriveID             = "root_drive"
	defaultVSockRunDir             = "/run"
)

func configureMachine(ctx context.Context, client *apiClient, paths machinePaths, spec MachineSpec, network NetworkAllocation) error {
	if err := client.PutMachineConfig(ctx, spec); err != nil {
		return fmt.Errorf("put machine config: %w", err)
	}
	if err := client.PutBootSource(ctx, spec); err != nil {
		return fmt.Errorf("put boot source: %w", err)
	}
	for _, drive := range additionalDriveRequests(spec) {
		if err := client.PutDrive(ctx, drive); err != nil {
			return fmt.Errorf("put drive %q: %w", drive.DriveID, err)
		}
	}
	if err := client.PutDrive(ctx, rootDriveRequest(spec)); err != nil {
		return fmt.Errorf("put root drive: %w", err)
	}
	if err := client.PutNetworkInterface(ctx, network); err != nil {
		return fmt.Errorf("put network interface: %w", err)
	}
	if spec.MMDS != nil {
		if err := client.PutMMDSConfig(ctx, *spec.MMDS); err != nil {
			return fmt.Errorf("put mmds config: %w", err)
		}
		if spec.MMDS.Data != nil {
			if err := client.PutMMDS(ctx, spec.MMDS.Data); err != nil {
				return fmt.Errorf("put mmds payload: %w", err)
			}
		}
	}
	if err := client.PutEntropy(ctx); err != nil {
		return fmt.Errorf("put entropy device: %w", err)
	}
	if err := client.PutSerial(ctx, paths.JailedSerialLogPath); err != nil {
		return fmt.Errorf("put serial device: %w", err)
	}
	if spec.Vsock != nil {
		if err := client.PutVsock(ctx, *spec.Vsock); err != nil {
			return fmt.Errorf("put vsock: %w", err)
		}
	}
	if err := client.PutAction(ctx, defaultStartAction); err != nil {
		return fmt.Errorf("start instance: %w", err)
	}
	return nil
}

func launchJailedFirecracker(paths machinePaths, machineID MachineID, firecrackerBinaryPath string, jailerBinaryPath string, enablePCI bool) (*exec.Cmd, error) {
	args := []string{
		"--id", string(machineID),
		"--uid", strconv.Itoa(os.Getuid()),
		"--gid", strconv.Itoa(os.Getgid()),
		"--exec-file", firecrackerBinaryPath,
		"--cgroup-version", defaultCgroupVersion,
		"--chroot-base-dir", paths.JailerBaseDir,
		"--daemonize",
		"--new-pid-ns",
		"--",
		"--api-sock", defaultFirecrackerSocketPath,
		"--log-path", paths.JailedFirecrackerLogPath,
		"--level", defaultFirecrackerLogLevel,
		"--show-level",
		"--show-log-origin",
	}
	if enablePCI {
		args = append(args, "--enable-pci")
	}
	command := exec.Command(jailerBinaryPath, args...)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start jailer: %w", err)
	}
	go func() {
		_ = command.Wait()
	}()
	return command, nil
}

func stageMachineFiles(spec MachineSpec, paths machinePaths) (MachineSpec, error) {
	staged := spec

	kernelImagePath, err := stagedFileName(spec.KernelImagePath)
	if err != nil {
		return MachineSpec{}, fmt.Errorf("kernel image path: %w", err)
	}
	if err := linkMachineFile(spec.KernelImagePath, filepath.Join(paths.ChrootRootDir, kernelImagePath)); err != nil {
		return MachineSpec{}, fmt.Errorf("link kernel image into jail: %w", err)
	}
	staged.KernelImagePath = kernelImagePath

	rootFSPath, err := stagedFileName(spec.RootFSPath)
	if err != nil {
		return MachineSpec{}, fmt.Errorf("root drive path: %w", err)
	}
	if err := linkMachineFile(spec.RootFSPath, filepath.Join(paths.ChrootRootDir, rootFSPath)); err != nil {
		return MachineSpec{}, fmt.Errorf("link root drive into jail: %w", err)
	}
	staged.RootFSPath = rootFSPath
	staged.RootDrive = spec.rootDrive()
	staged.RootDrive.Path = rootFSPath

	staged.Drives = make([]DriveSpec, len(spec.Drives))
	for i, drive := range spec.Drives {
		stagedDrive := drive
		stagedDrivePath, err := stagedFileName(drive.Path)
		if err != nil {
			return MachineSpec{}, fmt.Errorf("drive %q path: %w", drive.ID, err)
		}
		if err := linkMachineFile(drive.Path, filepath.Join(paths.ChrootRootDir, stagedDrivePath)); err != nil {
			return MachineSpec{}, fmt.Errorf("link drive %q into jail: %w", drive.ID, err)
		}
		stagedDrive.Path = stagedDrivePath
		staged.Drives[i] = stagedDrive
	}

	if spec.Vsock != nil {
		vsock := *spec.Vsock
		vsock.Path = jailedVSockDevicePath(*spec.Vsock)
		staged.Vsock = &vsock
	}

	return staged, nil
}

func waitForSocket(ctx context.Context, client *apiClient, socketPath string) error {
	waitContext, cancel := context.WithTimeout(ctx, defaultFirecrackerInitTimeout)
	defer cancel()

	ticker := time.NewTicker(defaultFirecrackerPollInterval)
	defer ticker.Stop()

	var lastStatErr error
	var lastPingErr error

	for {
		select {
		case <-waitContext.Done():
			switch {
			case lastPingErr != nil:
				return fmt.Errorf("%w (socket=%q last_ping_err=%v)", waitContext.Err(), socketPath, lastPingErr)
			case lastStatErr != nil:
				return fmt.Errorf("%w (socket=%q last_stat_err=%v)", waitContext.Err(), socketPath, lastStatErr)
			default:
				return fmt.Errorf("%w (socket=%q)", waitContext.Err(), socketPath)
			}
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err != nil {
				if os.IsNotExist(err) {
					lastStatErr = err
					continue
				}
				return fmt.Errorf("stat socket %q: %w", socketPath, err)
			}
			lastStatErr = nil
			if err := client.Ping(waitContext); err != nil {
				lastPingErr = err
				continue
			}
			return nil
		}
	}
}

func additionalDriveRequests(spec MachineSpec) []driveRequest {
	requests := make([]driveRequest, 0, len(spec.Drives))
	for _, drive := range spec.Drives {
		requests = append(requests, driveRequest{
			DriveID:      drive.ID,
			IsReadOnly:   drive.ReadOnly,
			IsRootDevice: false,
			PathOnHost:   drive.Path,
			CacheType:    drive.CacheType,
			IOEngine:     drive.IOEngine,
		})
	}
	return requests
}

func cleanupStartedProcess(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
}

func readPIDFile(pidFilePath string) (int, error) {
	payload, err := os.ReadFile(pidFilePath)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		return 0, fmt.Errorf("parse pid file %q: %w", pidFilePath, err)
	}
	if pid < 1 {
		return 0, fmt.Errorf("pid file %q must contain a positive pid", pidFilePath)
	}
	return pid, nil
}

func waitForPIDFile(ctx context.Context, pidFilePath string) (int, error) {
	waitContext, cancel := context.WithTimeout(ctx, defaultFirecrackerInitTimeout)
	defer cancel()

	ticker := time.NewTicker(defaultFirecrackerPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-waitContext.Done():
			if lastErr != nil {
				return 0, fmt.Errorf("%w (pid_file=%q last_err=%v)", waitContext.Err(), pidFilePath, lastErr)
			}
			return 0, fmt.Errorf("%w (pid_file=%q)", waitContext.Err(), pidFilePath)
		case <-ticker.C:
			pid, err := readPIDFile(pidFilePath)
			if err == nil {
				return pid, nil
			}
			lastErr = err
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
	}
}

func jailedVSockDevicePath(spec VsockSpec) string {
	return path.Join(defaultVSockRunDir, filepath.Base(strings.TrimSpace(spec.Path)))
}

func linkMachineFile(source string, target string) error {
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	if err := os.Link(resolvedSource, target); err != nil {
		return err
	}
	return nil
}

func rootDriveRequest(spec MachineSpec) driveRequest {
	root := spec.rootDrive()
	return driveRequest{
		DriveID:      root.ID,
		IsReadOnly:   root.ReadOnly,
		IsRootDevice: true,
		PathOnHost:   root.Path,
		CacheType:    root.CacheType,
		IOEngine:     root.IOEngine,
	}
}

func stagedFileName(filePath string) (string, error) {
	name := filepath.Base(strings.TrimSpace(filePath))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("file path is required")
	}
	return name, nil
}

func stageSnapshotFile(sourcePath string, chrootRootDir string, name string) (string, error) {
	target := filepath.Join(chrootRootDir, name)
	if err := linkMachineFile(sourcePath, target); err != nil {
		return "", err
	}
	return name, nil
}
