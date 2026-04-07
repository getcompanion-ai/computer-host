package firecracker

import "time"

// Phase represents the lifecycle phase of a local microVM.
type Phase string

const (
	// PhaseProvisioning means host-local resources are still being prepared.
	PhaseProvisioning Phase = "provisioning"
	// PhaseRunning means the Firecracker process is live.
	PhaseRunning Phase = "running"
	// PhaseStopped means the VM is no longer running.
	PhaseStopped Phase = "stopped"
	// PhaseMissing means the machine is not known to the runtime.
	PhaseMissing Phase = "missing"
	// PhaseError means the runtime observed a terminal failure.
	PhaseError Phase = "error"
)

// MachineState describes the current host-local state for a machine.
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
