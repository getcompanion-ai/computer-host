package firecracker

import "time"

// Phase represents the lifecycle phase of a local microVM.
type Phase string

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
