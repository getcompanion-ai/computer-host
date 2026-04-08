package host

import "time"

type Machine struct {
	ID             MachineID     `json:"id"`
	Artifact       ArtifactRef   `json:"artifact"`
	SystemVolumeID VolumeID      `json:"system_volume_id,omitempty"`
	UserVolumeIDs  []VolumeID    `json:"user_volume_ids,omitempty"`
	RuntimeHost    string        `json:"runtime_host,omitempty"`
	Ports          []MachinePort `json:"ports,omitempty"`
	Phase          MachinePhase  `json:"phase"`
	Error          string        `json:"error,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	StartedAt      *time.Time    `json:"started_at,omitempty"`
}

type CreateMachineRequest struct {
	MachineID     MachineID   `json:"machine_id"`
	Artifact      ArtifactRef `json:"artifact"`
	UserVolumeIDs []VolumeID  `json:"user_volume_ids,omitempty"`
}

type CreateMachineResponse struct {
	Machine Machine `json:"machine"`
}

type GetMachineResponse struct {
	Machine Machine `json:"machine"`
}

type ListMachinesResponse struct {
	Machines []Machine `json:"machines"`
}
