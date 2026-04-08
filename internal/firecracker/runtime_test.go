package firecracker

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const testHelperProcessEnv = "MICROAGENT_TEST_HELPER_PROCESS"
const testHelperReadyFileEnv = "MICROAGENT_TEST_HELPER_READY_FILE"

func TestStopEscalatesToSIGKILL(t *testing.T) {
	if os.Getenv(testHelperProcessEnv) == "ignore-term" {
		runIgnoreTERMHelper()
		return
	}

	readyFile := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=TestStopEscalatesToSIGKILL")
	command.Env = append(
		os.Environ(),
		testHelperProcessEnv+"=ignore-term",
		testHelperReadyFileEnv+"="+readyFile,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	waitForHelperReady(t, readyFile)

	restore := setStopTimings(20*time.Millisecond, 5*time.Millisecond)
	defer restore()

	runtime := &Runtime{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := runtime.Stop(ctx, MachineState{ID: "vm-1", PID: command.Process.Pid}); err != nil {
		t.Fatalf("stop machine: %v", err)
	}
	if processExists(command.Process.Pid) {
		t.Fatalf("process %d still running after stop", command.Process.Pid)
	}
}

func TestDeleteRemovesRuntimeDirAfterForcedKill(t *testing.T) {
	if os.Getenv(testHelperProcessEnv) == "ignore-term" {
		runIgnoreTERMHelper()
		return
	}

	root := t.TempDir()
	paths, err := buildMachinePaths(root, "vm-1", "/usr/bin/firecracker")
	if err != nil {
		t.Fatalf("build machine paths: %v", err)
	}
	if err := os.MkdirAll(paths.BaseDir, 0o755); err != nil {
		t.Fatalf("create machine base dir: %v", err)
	}
	sentinelPath := filepath.Join(paths.BaseDir, "sentinel")
	if err := os.WriteFile(sentinelPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write sentinel file: %v", err)
	}

	readyFile := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=TestDeleteRemovesRuntimeDirAfterForcedKill")
	command.Env = append(
		os.Environ(),
		testHelperProcessEnv+"=ignore-term",
		testHelperReadyFileEnv+"="+readyFile,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	waitForHelperReady(t, readyFile)

	restore := setStopTimings(20*time.Millisecond, 5*time.Millisecond)
	defer restore()

	runtime := &Runtime{
		rootDir:               root,
		firecrackerBinaryPath: "/usr/bin/firecracker",
		networkProvisioner: &IPTapProvisioner{
			runCommand: func(context.Context, string, ...string) error { return nil },
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	state := MachineState{
		ID:  "vm-1",
		PID: command.Process.Pid,
	}
	if err := runtime.Delete(ctx, state); err != nil {
		t.Fatalf("delete machine: %v", err)
	}
	if _, err := os.Stat(paths.BaseDir); !os.IsNotExist(err) {
		t.Fatalf("machine dir still exists after delete: %v", err)
	}
}

func runIgnoreTERMHelper() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	if readyFile := os.Getenv(testHelperReadyFileEnv); readyFile != "" {
		_ = os.WriteFile(readyFile, []byte("ready"), 0o644)
	}
	for {
		<-signals
	}
}

func setStopTimings(grace time.Duration, poll time.Duration) func() {
	previousGrace := stopGracePeriod
	previousPoll := stopPollInterval
	stopGracePeriod = grace
	stopPollInterval = poll
	return func() {
		stopGracePeriod = previousGrace
		stopPollInterval = previousPoll
	}
}

func waitForHelperReady(t *testing.T, readyFile string) {
	t.Helper()

	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(readyFile); err == nil {
			return
		}
		select {
		case <-timeout.C:
			t.Fatalf("timed out waiting for helper ready file %q", readyFile)
		case <-ticker.C:
		}
	}
}
