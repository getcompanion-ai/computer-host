package store

import (
	"context"
	"errors"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

var ErrNotFound = errors.New("store: not found")

type Store interface {
	PutArtifact(context.Context, model.ArtifactRecord) error
	GetArtifact(context.Context, contracthost.ArtifactRef) (*model.ArtifactRecord, error)
	ListArtifacts(context.Context) ([]model.ArtifactRecord, error)
	CreateMachine(context.Context, model.MachineRecord) error
	GetMachine(context.Context, contracthost.MachineID) (*model.MachineRecord, error)
	ListMachines(context.Context) ([]model.MachineRecord, error)
	UpdateMachine(context.Context, model.MachineRecord) error
	DeleteMachine(context.Context, contracthost.MachineID) error
	CreateVolume(context.Context, model.VolumeRecord) error
	GetVolume(context.Context, contracthost.VolumeID) (*model.VolumeRecord, error)
	ListVolumes(context.Context) ([]model.VolumeRecord, error)
	UpdateVolume(context.Context, model.VolumeRecord) error
	DeleteVolume(context.Context, contracthost.VolumeID) error
	UpsertOperation(context.Context, model.OperationRecord) error
	ListOperations(context.Context) ([]model.OperationRecord, error)
	DeleteOperation(context.Context, contracthost.MachineID) error
	CreateSnapshot(context.Context, model.SnapshotRecord) error
	GetSnapshot(context.Context, contracthost.SnapshotID) (*model.SnapshotRecord, error)
	ListSnapshots(context.Context) ([]model.SnapshotRecord, error)
	ListSnapshotsByMachine(context.Context, contracthost.MachineID) ([]model.SnapshotRecord, error)
	DeleteSnapshot(context.Context, contracthost.SnapshotID) error
	CreatePublishedPort(context.Context, model.PublishedPortRecord) error
	GetPublishedPort(context.Context, contracthost.PublishedPortID) (*model.PublishedPortRecord, error)
	ListPublishedPorts(context.Context, contracthost.MachineID) ([]model.PublishedPortRecord, error)
	DeletePublishedPort(context.Context, contracthost.PublishedPortID) error
	CreateMount(context.Context, model.MountRecord) error
	GetMount(context.Context, contracthost.MountID) (*model.MountRecord, error)
	ListMounts(context.Context, contracthost.MachineID) ([]model.MountRecord, error)
	UpdateMount(context.Context, model.MountRecord) error
	DeleteMount(context.Context, contracthost.MountID) error
}
