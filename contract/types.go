package host

type ArtifactID string

type ArtifactVersion string

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
