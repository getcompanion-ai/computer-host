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
}
