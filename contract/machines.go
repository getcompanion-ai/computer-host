package host

import "time"

type Machine struct {
	ID                MachineID     `json:"id"`
	Artifact          ArtifactRef   `json:"artifact"`
	SystemVolumeID    VolumeID      `json:"system_volume_id,omitempty"`
	UserVolumeIDs     []VolumeID    `json:"user_volume_ids,omitempty"`
	RuntimeHost       string        `json:"runtime_host,omitempty"`
	Ports             []MachinePort `json:"ports,omitempty"`
	GuestSSHPublicKey string        `json:"guest_ssh_host_public_key,omitempty"`
	Phase             MachinePhase  `json:"phase"`
	Error             string        `json:"error,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
}

type GuestConfig struct {
	Hostname          string             `json:"hostname,omitempty"`
	AuthorizedKeys    []string           `json:"authorized_keys,omitempty"`
	TrustedUserCAKeys []string           `json:"trusted_user_ca_keys,omitempty"`
	LoginWebhook      *GuestLoginWebhook `json:"login_webhook,omitempty"`
}

type GuestLoginWebhook struct {
	URL         string `json:"url"`
	BearerToken string `json:"bearer_token,omitempty"`
}

type CreateMachineRequest struct {
	MachineID     MachineID    `json:"machine_id"`
	Artifact      ArtifactRef  `json:"artifact"`
	UserVolumeIDs []VolumeID   `json:"user_volume_ids,omitempty"`
	GuestConfig   *GuestConfig `json:"guest_config,omitempty"`
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
