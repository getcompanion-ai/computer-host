package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchJailedFirecrackerPassesDaemonAndLoggingFlags(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args.txt")
	jailerPath := filepath.Join(root, "fake-jailer.sh")
	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > " + shellQuote(argsPath) + "\n"
	if err := os.WriteFile(jailerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake jailer: %v", err)
	}

	paths, err := buildMachinePaths(root, "vm-1", "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("build machine paths: %v", err)
	}
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}

	if _, err := launchJailedFirecracker(paths, "vm-1", "/usr/bin/firecracker", jailerPath, false); err != nil {
		t.Fatalf("launch jailed firecracker: %v", err)
	}

	args := waitForFileContents(t, argsPath)
	for _, want := range []string{
		"--daemonize",
		"--new-pid-ns",
		"--log-path",
		paths.JailedFirecrackerLogPath,
		"--show-level",
		"--show-log-origin",
	} {
		if !containsLine(args, want) {
			t.Fatalf("missing launch argument %q in %v", want, args)
		}
	}
}

func TestLaunchJailedFirecrackerPassesEnablePCIWhenConfigured(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args.txt")
	jailerPath := filepath.Join(root, "fake-jailer.sh")
	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > " + shellQuote(argsPath) + "\n"
	if err := os.WriteFile(jailerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake jailer: %v", err)
	}

	paths, err := buildMachinePaths(root, "vm-1", "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("build machine paths: %v", err)
	}
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}

	if _, err := launchJailedFirecracker(paths, "vm-1", "/usr/bin/firecracker", jailerPath, true); err != nil {
		t.Fatalf("launch jailed firecracker: %v", err)
	}

	args := waitForFileContents(t, argsPath)
	if !containsLine(args, "--enable-pci") {
		t.Fatalf("missing launch argument %q in %v", "--enable-pci", args)
	}
}

func TestWaitForPIDFileReadsPID(t *testing.T) {
	pidFilePath := filepath.Join(t.TempDir(), "firecracker.pid")
	if err := os.WriteFile(pidFilePath, []byte("4321\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	pid, err := waitForPIDFile(context.Background(), pidFilePath)
	if err != nil {
		t.Fatalf("wait for pid file: %v", err)
	}
	if pid != 4321 {
		t.Fatalf("pid mismatch: got %d want %d", pid, 4321)
	}
}

func waitForFileContents(t *testing.T, path string) []string {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		payload, err := os.ReadFile(path)
		if err == nil {
			return strings.Split(strings.TrimSpace(string(payload)), "\n")
		}
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for %q", path)
		case <-ticker.C:
		}
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
