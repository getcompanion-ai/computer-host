package host

import "time"

type MachineID string

type MachinePhase string

type VolumeID string

type VolumeKind string

const (
	MachinePhasePending  MachinePhase = "pending"
	MachinePhaseRunning  MachinePhase = "running"
	MachinePhaseStopping MachinePhase = "stopping"
	MachinePhaseStopped  MachinePhase = "stopped"
	MachinePhaseFailed   MachinePhase = "failed"
	MachinePhaseDeleting MachinePhase = "deleting"
)

const (
	VolumeKindSystem VolumeKind = "system"
	VolumeKindUser   VolumeKind = "user"
)

type Machine struct {
	ID          MachineID    `json:"id"`
	Phase       MachinePhase `json:"phase"`
	RuntimeHost string       `json:"runtime_host,omitempty"`
	Error       string       `json:"error,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	StartedAt   *time.Time   `json:"started_at,omitempty"`
}
