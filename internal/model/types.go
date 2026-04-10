package model

import (
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type StoragePool string

const (
	StoragePoolArtifacts      StoragePool = "artifacts"
	StoragePoolMachineDisks   StoragePool = "machine-disks"
	StoragePoolPublishedPorts StoragePool = "published-ports"
	StoragePoolSnapshots      StoragePool = "snapshots"
	StoragePoolState          StoragePool = "state"
	StoragePoolUserVolumes    StoragePool = "user-volumes"
)

type ArtifactRecord struct {
	Ref             contracthost.ArtifactRef
	LocalKey        string
	LocalDir        string
	KernelImagePath string
	RootFSPath      string
	CreatedAt       time.Time
}

type MachineRecord struct {
	ID                contracthost.MachineID
	Artifact          contracthost.ArtifactRef
	GuestConfig       *contracthost.GuestConfig
	SystemVolumeID    contracthost.VolumeID
	UserVolumeIDs     []contracthost.VolumeID
	RuntimeHost       string
	TapDevice         string
	Ports             []contracthost.MachinePort
	GuestSSHPublicKey string
	Phase             contracthost.MachinePhase
	Error             string
	PID               int
	SocketPath        string
	CreatedAt         time.Time
	StartedAt         *time.Time
}

type VolumeRecord struct {
	ID                contracthost.VolumeID
	Kind              contracthost.VolumeKind
	AttachedMachineID *contracthost.MachineID
	SourceArtifact    *contracthost.ArtifactRef
	Pool              StoragePool
	Path              string
	CreatedAt         time.Time
}

type MachineOperation string

const (
	MachineOperationCreate   MachineOperation = "create"
	MachineOperationStart    MachineOperation = "start"
	MachineOperationStop     MachineOperation = "stop"
	MachineOperationDelete   MachineOperation = "delete"
	MachineOperationSnapshot MachineOperation = "snapshot"
	MachineOperationRestore  MachineOperation = "restore"
)

type SnapshotRecord struct {
	ID                contracthost.SnapshotID
	MachineID         contracthost.MachineID
	Artifact          contracthost.ArtifactRef
	MemFilePath       string
	StateFilePath     string
	DiskPaths         []string
	Artifacts         []SnapshotArtifactRecord
	SourceRuntimeHost string
	SourceTapDevice   string
	CreatedAt         time.Time
}

type SnapshotArtifactRecord struct {
	ID        string
	Kind      contracthost.SnapshotArtifactKind
	Name      string
	LocalPath string
	SizeBytes int64
	SHA256Hex string
}

type PublishedPortRecord struct {
	ID        contracthost.PublishedPortID
	MachineID contracthost.MachineID
	Name      string
	Port      uint16
	HostPort  uint16
	Protocol  contracthost.PortProtocol
	CreatedAt time.Time
}

type OperationRecord struct {
	MachineID  contracthost.MachineID
	Type       MachineOperation
	StartedAt  time.Time
	SnapshotID *contracthost.SnapshotID `json:"snapshot_id,omitempty"`
}
