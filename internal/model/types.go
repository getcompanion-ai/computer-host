package model

import (
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type StoragePool string

const (
	StoragePoolArtifacts    StoragePool = "artifacts"
	StoragePoolMachineDisks StoragePool = "machine-disks"
	StoragePoolState        StoragePool = "state"
	StoragePoolUserVolumes  StoragePool = "user-volumes"
)

type ArtifactRecord struct {
	Ref             contracthost.ArtifactRef
	KernelImagePath string
	RootFSPath      string
	CreatedAt       time.Time
}

type MachineRecord struct {
	ID             contracthost.MachineID
	Artifact       contracthost.ArtifactRef
	SystemVolumeID contracthost.VolumeID
	UserVolumeIDs  []contracthost.VolumeID
	Phase          contracthost.MachinePhase
	RuntimeHost    string
	Error          string
	CreatedAt      time.Time
	StartedAt      *time.Time
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
