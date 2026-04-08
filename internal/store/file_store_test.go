package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

func TestFileStorePersistsStateAndOperations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	statePath := filepath.Join(root, "state", "state.json")
	opsPath := filepath.Join(root, "state", "ops.json")

	ctx := context.Background()
	first, err := NewFileStore(statePath, opsPath)
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}

	artifact := model.ArtifactRecord{
		Ref: contracthost.ArtifactRef{
			KernelImageURL: "https://example.com/kernel",
			RootFSURL:      "https://example.com/rootfs",
		},
		LocalKey:        "artifact-key",
		LocalDir:        filepath.Join(root, "artifacts", "artifact-key"),
		KernelImagePath: filepath.Join(root, "artifacts", "artifact-key", "kernel"),
		RootFSPath:      filepath.Join(root, "artifacts", "artifact-key", "rootfs"),
		CreatedAt:       time.Unix(1700000000, 0).UTC(),
	}
	if err := first.PutArtifact(ctx, artifact); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	machine := model.MachineRecord{
		ID:             "vm-1",
		Artifact:       artifact.Ref,
		SystemVolumeID: "vm-1-system",
		RuntimeHost:    "172.16.0.2",
		TapDevice:      "fctap0",
		Ports: []contracthost.MachinePort{
			{Name: contracthost.MachinePortNameSSH, Port: 22, Protocol: contracthost.PortProtocolTCP},
		},
		Phase:      contracthost.MachinePhaseRunning,
		PID:        1234,
		SocketPath: filepath.Join(root, "runtime", "machine.sock"),
		CreatedAt:  time.Unix(1700000001, 0).UTC(),
		StartedAt:  timePtr(time.Unix(1700000002, 0).UTC()),
	}
	if err := first.CreateMachine(ctx, machine); err != nil {
		t.Fatalf("create machine: %v", err)
	}

	volume := model.VolumeRecord{
		ID:                "vm-1-system",
		Kind:              contracthost.VolumeKindSystem,
		AttachedMachineID: machineIDPtr("vm-1"),
		Path:              filepath.Join(root, "machine-disks", "vm-1", "system.img"),
		CreatedAt:         time.Unix(1700000003, 0).UTC(),
	}
	if err := first.CreateVolume(ctx, volume); err != nil {
		t.Fatalf("create volume: %v", err)
	}

	operation := model.OperationRecord{
		MachineID: "vm-1",
		Type:      model.MachineOperationCreate,
		StartedAt: time.Unix(1700000004, 0).UTC(),
	}
	if err := first.UpsertOperation(ctx, operation); err != nil {
		t.Fatalf("upsert operation: %v", err)
	}

	second, err := NewFileStore(statePath, opsPath)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}

	gotArtifact, err := second.GetArtifact(ctx, artifact.Ref)
	if err != nil {
		t.Fatalf("get artifact after reopen: %v", err)
	}
	if gotArtifact.LocalKey != artifact.LocalKey {
		t.Fatalf("artifact local key mismatch: got %q want %q", gotArtifact.LocalKey, artifact.LocalKey)
	}

	gotMachine, err := second.GetMachine(ctx, machine.ID)
	if err != nil {
		t.Fatalf("get machine after reopen: %v", err)
	}
	if gotMachine.Phase != contracthost.MachinePhaseRunning {
		t.Fatalf("machine phase mismatch: got %q", gotMachine.Phase)
	}
	if gotMachine.RuntimeHost != machine.RuntimeHost {
		t.Fatalf("runtime host mismatch: got %q want %q", gotMachine.RuntimeHost, machine.RuntimeHost)
	}

	gotVolume, err := second.GetVolume(ctx, volume.ID)
	if err != nil {
		t.Fatalf("get volume after reopen: %v", err)
	}
	if gotVolume.AttachedMachineID == nil || *gotVolume.AttachedMachineID != "vm-1" {
		t.Fatalf("attached machine mismatch: got %#v", gotVolume.AttachedMachineID)
	}

	operations, err := second.ListOperations(ctx)
	if err != nil {
		t.Fatalf("list operations after reopen: %v", err)
	}
	if len(operations) != 1 {
		t.Fatalf("operation count mismatch: got %d want 1", len(operations))
	}
	if operations[0].Type != model.MachineOperationCreate {
		t.Fatalf("operation type mismatch: got %q", operations[0].Type)
	}
}

func timePtr(value time.Time) *time.Time {
	return &value
}

func machineIDPtr(value contracthost.MachineID) *contracthost.MachineID {
	return &value
}
