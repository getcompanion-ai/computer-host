package firecracker

import "time"

// Phase represents the lifecycle phase of a local microVM.
type Phase string

// SnapshotPaths holds the file paths for a VM snapshot.
type SnapshotPaths struct {
	MemFilePath   string
	StateFilePath string
}

// SnapshotLoadSpec describes what is needed to restore a VM from a snapshot.
type SnapshotLoadSpec struct {
	ID              MachineID
	SnapshotPath    string
	MemFilePath     string
	DiskPaths       map[string]string // drive ID -> host path
	RootFSPath      string
	KernelImagePath string
	VCPUs           int64
	MemoryMiB       int64
	KernelArgs      string
	Vsock           *VsockSpec
	Network         *NetworkAllocation
}

// MachineState describes the current host local state for a machine.
type MachineState struct {
	ID          MachineID
	Phase       Phase
	PID         int
	RuntimeHost string
	SocketPath  string
	TapName     string
	StartedAt   *time.Time
	Error       string
}

const (
	// PhaseRunning means the Firecracker process is live.
	PhaseRunning Phase = "running"
	// PhaseStopped means the VM is no longer running.
	PhaseStopped Phase = "stopped"
	// PhaseFailed means the runtime observed a terminal failure.
	PhaseFailed Phase = "failed"
)
