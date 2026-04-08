package host

import "time"

type ArtifactRef struct {
	ID      ArtifactID      `json:"id"`
	Version ArtifactVersion `json:"version"`
}

type Volume struct {
	ID                VolumeID   `json:"id"`
	Kind              VolumeKind `json:"kind"`
	AttachedMachineID *MachineID `json:"attached_machine_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}
