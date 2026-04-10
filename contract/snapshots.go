package host

import "time"

type SnapshotID string

type SnapshotArtifactKind string

const (
	SnapshotArtifactKindMemory   SnapshotArtifactKind = "memory"
	SnapshotArtifactKindVMState  SnapshotArtifactKind = "vmstate"
	SnapshotArtifactKindDisk     SnapshotArtifactKind = "disk"
	SnapshotArtifactKindManifest SnapshotArtifactKind = "manifest"
)

type Snapshot struct {
	ID                SnapshotID `json:"id"`
	MachineID         MachineID  `json:"machine_id"`
	SourceRuntimeHost string     `json:"source_runtime_host,omitempty"`
	SourceTapDevice   string     `json:"source_tap_device,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type SnapshotArtifact struct {
	ID          string               `json:"id"`
	Kind        SnapshotArtifactKind `json:"kind"`
	Name        string               `json:"name"`
	SizeBytes   int64                `json:"size_bytes"`
	SHA256Hex   string               `json:"sha256_hex,omitempty"`
	ObjectKey   string               `json:"object_key,omitempty"`
	DownloadURL string               `json:"download_url,omitempty"`
}

type CreateSnapshotRequest struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
}

type CreateSnapshotResponse struct {
	Snapshot  Snapshot           `json:"snapshot"`
	Artifacts []SnapshotArtifact `json:"artifacts,omitempty"`
}

type GetSnapshotResponse struct {
	Snapshot Snapshot `json:"snapshot"`
}

type ListSnapshotsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

type SnapshotUploadPart struct {
	PartNumber  int32  `json:"part_number"`
	OffsetBytes int64  `json:"offset_bytes"`
	SizeBytes   int64  `json:"size_bytes"`
	UploadURL   string `json:"upload_url"`
}

type SnapshotArtifactUploadSession struct {
	ArtifactID string               `json:"artifact_id"`
	ObjectKey  string               `json:"object_key"`
	UploadID   string               `json:"upload_id"`
	Parts      []SnapshotUploadPart `json:"parts"`
}

type UploadSnapshotRequest struct {
	Artifacts []SnapshotArtifactUploadSession `json:"artifacts"`
}

type UploadedSnapshotPart struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}

type UploadedSnapshotArtifact struct {
	ArtifactID     string                 `json:"artifact_id"`
	CompletedParts []UploadedSnapshotPart `json:"completed_parts"`
}

type UploadSnapshotResponse struct {
	Artifacts []UploadedSnapshotArtifact `json:"artifacts"`
}

type RestoreSnapshotRequest struct {
	MachineID     MachineID            `json:"machine_id"`
	Artifact      ArtifactRef          `json:"artifact"`
	LocalSnapshot *LocalSnapshotSpec   `json:"local_snapshot,omitempty"`
	Snapshot      *DurableSnapshotSpec `json:"snapshot,omitempty"`
	GuestConfig   *GuestConfig         `json:"guest_config,omitempty"`
}

type LocalSnapshotSpec struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
}

type DurableSnapshotSpec struct {
	SnapshotID        SnapshotID         `json:"snapshot_id"`
	MachineID         MachineID          `json:"machine_id"`
	ImageID           string             `json:"image_id"`
	SourceRuntimeHost string             `json:"source_runtime_host,omitempty"`
	SourceTapDevice   string             `json:"source_tap_device,omitempty"`
	Artifacts         []SnapshotArtifact `json:"artifacts"`
}

type RestoreSnapshotResponse struct {
	Machine Machine `json:"machine"`
}
