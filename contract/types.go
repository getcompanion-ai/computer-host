package host

type MachineID string

type MachinePhase string

type VolumeID string

type VolumeKind string

const (
	MachinePhaseStarting MachinePhase = "starting"
	MachinePhaseRunning  MachinePhase = "running"
	MachinePhaseStopped  MachinePhase = "stopped"
	MachinePhaseFailed   MachinePhase = "failed"
)

const (
	VolumeKindSystem VolumeKind = "system"
	VolumeKindUser   VolumeKind = "user"
)
