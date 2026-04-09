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

type StoragePool string

const (
	StoragePoolArtifacts     StoragePool = "artifacts"
	StoragePoolMachineDisks  StoragePool = "machine-disks"
	StoragePoolPublishedPort StoragePool = "published-ports"
	StoragePoolSnapshots     StoragePool = "snapshots"
	StoragePoolState         StoragePool = "state"
)

type StoragePoolUsage struct {
	Pool  StoragePool `json:"pool"`
	Bytes int64       `json:"bytes"`
}

type MachineStorageUsage struct {
	MachineID    MachineID `json:"machine_id"`
	SystemBytes  int64     `json:"system_bytes"`
	UserBytes    int64     `json:"user_bytes"`
	RuntimeBytes int64     `json:"runtime_bytes"`
}

type SnapshotStorageUsage struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
	Bytes      int64      `json:"bytes"`
}

type StorageReport struct {
	GeneratedAt    time.Time              `json:"generated_at"`
	TotalBytes     int64                  `json:"total_bytes"`
	Pools          []StoragePoolUsage     `json:"pools,omitempty"`
	Machines       []MachineStorageUsage  `json:"machines,omitempty"`
	Snapshots      []SnapshotStorageUsage `json:"snapshots,omitempty"`
	PublishedPorts int64                  `json:"published_ports"`
}

type GetStorageReportResponse struct {
	Report StorageReport `json:"report"`
}
