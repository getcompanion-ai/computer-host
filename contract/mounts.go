package host

import "time"

type MountID string

type MountKind string

const (
	MountKindR2    MountKind = "r2"
	MountKindS3    MountKind = "s3"
	MountKindGCS   MountKind = "gcs"
	MountKindWebDAV MountKind = "webdav"
)

type MountStatus string

const (
	MountStatusPending  MountStatus = "pending"
	MountStatusMounting MountStatus = "mounting"
	MountStatusMounted  MountStatus = "mounted"
	MountStatusFailed   MountStatus = "failed"
)

type Mount struct {
	ID            MountID     `json:"id"`
	MachineID     MachineID   `json:"machine_id"`
	Kind          MountKind   `json:"kind"`
	TargetPath    string      `json:"target_path"`
	ReadOnly      bool        `json:"read_only"`
	Config        MountConfig `json:"config"`
	Status        MountStatus `json:"status"`
	StatusMessage string      `json:"status_message,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
}

type MountConfig struct {
	Bucket          string            `json:"bucket"`
	Endpoint        string            `json:"endpoint,omitempty"`
	Region          string            `json:"region,omitempty"`
	AccessKeyID     string            `json:"access_key_id"`
	SecretAccessKey  string            `json:"secret_access_key"`
	VFSCacheMode    string            `json:"vfs_cache_mode,omitempty"`
	ExtraFlags      []string          `json:"extra_flags,omitempty"`
}

type CreateMountRequest struct {
	MountID    MountID     `json:"mount_id"`
	Kind       MountKind   `json:"kind"`
	TargetPath string      `json:"target_path"`
	ReadOnly   bool        `json:"read_only"`
	Config     MountConfig `json:"config"`
}

type CreateMountResponse struct {
	Mount Mount `json:"mount"`
}

type ListMountsResponse struct {
	Mounts []Mount `json:"mounts"`
}
