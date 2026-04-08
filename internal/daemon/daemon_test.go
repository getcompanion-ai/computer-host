package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type fakeRuntime struct {
	bootState   firecracker.MachineState
	bootCalls   int
	deleteCalls []firecracker.MachineState
	lastSpec    firecracker.MachineSpec
}

func (f *fakeRuntime) Boot(_ context.Context, spec firecracker.MachineSpec, _ []firecracker.NetworkAllocation) (*firecracker.MachineState, error) {
	f.bootCalls++
	f.lastSpec = spec
	state := f.bootState
	return &state, nil
}

func (f *fakeRuntime) Inspect(state firecracker.MachineState) (*firecracker.MachineState, error) {
	copy := state
	return &copy, nil
}

func (f *fakeRuntime) Delete(_ context.Context, state firecracker.MachineState) error {
	f.deleteCalls = append(f.deleteCalls, state)
	return nil
}

func TestCreateMachineStagesArtifactsAndPersistsState(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	sshListener := listenTestPort(t, int(defaultSSHPort))
	defer sshListener.Close()
	vncListener := listenTestPort(t, int(defaultVNCPort))
	defer vncListener.Close()

	startedAt := time.Unix(1700000005, 0).UTC()
	runtime := &fakeRuntime{
		bootState: firecracker.MachineState{
			ID:          "vm-1",
			Phase:       firecracker.PhaseRunning,
			PID:         4321,
			RuntimeHost: "127.0.0.1",
			SocketPath:  filepath.Join(cfg.RuntimeDir, "machines", "vm-1", "root", "run", "firecracker.sock"),
			TapName:     "fctap0",
			StartedAt:   &startedAt,
		},
	}

	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	kernelPayload := []byte("kernel-image")
	rootFSImagePath := filepath.Join(root, "guest-rootfs.ext4")
	if err := buildTestExt4Image(root, rootFSImagePath); err != nil {
		t.Fatalf("build ext4 image: %v", err)
	}
	rootFSPayload, err := os.ReadFile(rootFSImagePath)
	if err != nil {
		t.Fatalf("read ext4 image: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kernel":
			_, _ = w.Write(kernelPayload)
		case "/rootfs":
			_, _ = w.Write(rootFSPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	response, err := hostDaemon.CreateMachine(context.Background(), contracthost.CreateMachineRequest{
		MachineID: "vm-1",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: server.URL + "/kernel",
			RootFSURL:      server.URL + "/rootfs",
		},
		GuestConfig: &contracthost.GuestConfig{
			AuthorizedKeys: []string{
				"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestOverrideKey daemon-test",
			},
			LoginWebhook: &contracthost.GuestLoginWebhook{
				URL:         "https://example.com/login",
				BearerToken: "token",
			},
		},
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}

	if response.Machine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("machine phase mismatch: got %q", response.Machine.Phase)
	}
	if response.Machine.RuntimeHost != "127.0.0.1" {
		t.Fatalf("runtime host mismatch: got %q", response.Machine.RuntimeHost)
	}
	if len(response.Machine.Ports) != 2 {
		t.Fatalf("machine ports mismatch: got %d want 2", len(response.Machine.Ports))
	}
	if response.Machine.Ports[0].Port != defaultSSHPort || response.Machine.Ports[1].Port != defaultVNCPort {
		t.Fatalf("machine ports mismatch: got %#v", response.Machine.Ports)
	}
	if runtime.bootCalls != 1 {
		t.Fatalf("boot call count mismatch: got %d want 1", runtime.bootCalls)
	}
	if runtime.lastSpec.KernelImagePath == "" || runtime.lastSpec.RootFSPath == "" {
		t.Fatalf("runtime spec paths not populated: %#v", runtime.lastSpec)
	}
	if _, err := os.Stat(runtime.lastSpec.KernelImagePath); err != nil {
		t.Fatalf("kernel artifact not staged: %v", err)
	}
	if _, err := os.Stat(runtime.lastSpec.RootFSPath); err != nil {
		t.Fatalf("system disk not staged: %v", err)
	}
	hostAuthorizedKeyBytes, err := os.ReadFile(hostDaemon.backendSSHPublicKeyPath())
	if err != nil {
		t.Fatalf("read backend ssh public key: %v", err)
	}
	authorizedKeys, err := readExt4File(runtime.lastSpec.RootFSPath, "/etc/microagent/authorized_keys")
	if err != nil {
		t.Fatalf("read injected authorized_keys: %v", err)
	}
	if !strings.Contains(authorizedKeys, strings.TrimSpace(string(hostAuthorizedKeyBytes))) {
		t.Fatalf("authorized_keys missing backend ssh key: %q", authorizedKeys)
	}
	if !strings.Contains(authorizedKeys, "daemon-test") {
		t.Fatalf("authorized_keys missing request override key: %q", authorizedKeys)
	}

	artifact, err := fileStore.GetArtifact(context.Background(), response.Machine.Artifact)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if artifact.KernelImagePath == "" || artifact.RootFSPath == "" {
		t.Fatalf("artifact paths missing: %#v", artifact)
	}
	if payload, err := os.ReadFile(artifact.KernelImagePath); err != nil {
		t.Fatalf("read kernel artifact: %v", err)
	} else if string(payload) != string(kernelPayload) {
		t.Fatalf("kernel artifact payload mismatch: got %q", string(payload))
	}

	machine, err := fileStore.GetMachine(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if machine.SystemVolumeID != "vm-1-system" {
		t.Fatalf("system volume mismatch: got %q", machine.SystemVolumeID)
	}

	operations, err := fileStore.ListOperations(context.Background())
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operation journal should be empty after success: got %d entries", len(operations))
	}
}

func TestNewEnsuresBackendSSHKeyPair(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	if _, err := os.Stat(hostDaemon.backendSSHPrivateKeyPath()); err != nil {
		t.Fatalf("stat backend ssh private key: %v", err)
	}
	publicKeyPayload, err := os.ReadFile(hostDaemon.backendSSHPublicKeyPath())
	if err != nil {
		t.Fatalf("read backend ssh public key: %v", err)
	}
	if !strings.HasPrefix(string(publicKeyPayload), "ssh-ed25519 ") {
		t.Fatalf("unexpected backend ssh public key: %q", string(publicKeyPayload))
	}
}

func TestCreateMachineRejectsNonHTTPArtifactURLs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	hostDaemon, err := New(cfg, fileStore, &fakeRuntime{})
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	_, err = hostDaemon.CreateMachine(context.Background(), contracthost.CreateMachineRequest{
		MachineID: "vm-1",
		Artifact: contracthost.ArtifactRef{
			KernelImageURL: "file:///kernel",
			RootFSURL:      "https://example.com/rootfs",
		},
	})
	if err == nil {
		t.Fatal("expected create machine to fail for non-http artifact url")
	}
	if got := err.Error(); got != "artifact.kernel_image_url must use http or https" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestDeleteMachineMissingIsNoOp(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := testConfig(root)
	fileStore, err := store.NewFileStore(cfg.StatePath, cfg.OperationsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	runtime := &fakeRuntime{}
	hostDaemon, err := New(cfg, fileStore, runtime)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	if err := hostDaemon.DeleteMachine(context.Background(), "missing"); err != nil {
		t.Fatalf("delete missing machine: %v", err)
	}
	if len(runtime.deleteCalls) != 0 {
		t.Fatalf("delete runtime should not be called for missing machine")
	}
}

func testConfig(root string) appconfig.Config {
	return appconfig.Config{
		RootDir:               root,
		StatePath:             filepath.Join(root, "state", "state.json"),
		OperationsPath:        filepath.Join(root, "state", "ops.json"),
		ArtifactsDir:          filepath.Join(root, "artifacts"),
		MachineDisksDir:       filepath.Join(root, "machine-disks"),
		RuntimeDir:            filepath.Join(root, "runtime"),
		SocketPath:            filepath.Join(root, "firecracker-host.sock"),
		EgressInterface:       "eth0",
		FirecrackerBinaryPath: "/usr/bin/firecracker",
		JailerBinaryPath:      "/usr/bin/jailer",
	}
}

func listenTestPort(t *testing.T, port int) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		var bindErr *net.OpError
		if errors.As(err, &bindErr) && strings.Contains(strings.ToLower(err.Error()), "address already in use") {
			t.Skipf("port %d already in use", port)
		}
		t.Fatalf("listen on port %d: %v", port, err)
	}

	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	return listener
}
