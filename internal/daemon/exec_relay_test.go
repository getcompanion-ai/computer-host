package daemon

import (
	"context"
	"net"
	"testing"

	"github.com/getcompanion-ai/computer-host/internal/model"
	"github.com/getcompanion-ai/computer-host/internal/store"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func TestEnsureExecRelayAllocatesRelayLazily(t *testing.T) {
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

	upstream, err := net.Listen("tcp", "127.0.0.1:49983")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer func() { _ = upstream.Close() }()

	record := model.MachineRecord{
		ID:             "vm-exec",
		Artifact:       contracthost.ArtifactRef{KernelImageURL: "https://example.com/kernel", RootFSURL: "https://example.com/rootfs"},
		SystemVolumeID: "vm-exec-system",
		RuntimeHost:    "127.0.0.1",
		Ports:          defaultMachinePorts(),
		Phase:          contracthost.MachinePhaseRunning,
		GuestConfig:    &contracthost.GuestConfig{},
	}
	if err := fileStore.CreateMachine(context.Background(), record); err != nil {
		t.Fatalf("create machine record: %v", err)
	}

	response, err := hostDaemon.EnsureExecRelay(context.Background(), "vm-exec")
	if err != nil {
		t.Fatalf("ensure exec relay: %v", err)
	}
	defer hostDaemon.stopMachineRelays("vm-exec")

	var execPort contracthost.MachinePort
	found := false
	for _, port := range response.Machine.Ports {
		if port.Name == contracthost.MachinePortNameExec {
			execPort = port
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("exec port not found in machine ports: %#v", response.Machine.Ports)
	}
	if execPort.Port != defaultGuestdPort {
		t.Fatalf("exec guest port = %d, want %d", execPort.Port, defaultGuestdPort)
	}
	if execPort.HostPort < minMachineExecRelayPort || execPort.HostPort > maxMachineExecRelayPort {
		t.Fatalf("exec host port = %d, want range %d-%d", execPort.HostPort, minMachineExecRelayPort, maxMachineExecRelayPort)
	}

	stored, err := fileStore.GetMachine(context.Background(), "vm-exec")
	if err != nil {
		t.Fatalf("get stored machine: %v", err)
	}
	hasStoredExecPort := false
	for _, port := range stored.Ports {
		if port.Name == contracthost.MachinePortNameExec && port.HostPort == execPort.HostPort {
			hasStoredExecPort = true
			break
		}
	}
	if !hasStoredExecPort {
		t.Fatalf("stored machine missing exec relay port: %#v", stored.Ports)
	}
}
