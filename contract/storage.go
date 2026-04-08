package host

import "time"

type ArtifactRef struct {
	KernelImageURL string `json:"kernel_image_url"`
	RootFSURL      string `json:"rootfs_url"`
}

type Volume struct {
	ID                VolumeID   `json:"id"`
	Kind              VolumeKind `json:"kind"`
	AttachedMachineID *MachineID `json:"attached_machine_id,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}
