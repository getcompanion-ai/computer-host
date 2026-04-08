package model

import (
	"time"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type MachineRecord struct {
	ID          contracthost.MachineID
	Phase       contracthost.MachinePhase
	RuntimeHost string
	Error       string
	CreatedAt   time.Time
	StartedAt   *time.Time
}

type StoragePool string

type VolumeRecord struct {
	ID        contracthost.VolumeID
	MachineID contracthost.MachineID
	Kind      contracthost.VolumeKind
	Pool      StoragePool
	Path      string
	CreatedAt time.Time
}
