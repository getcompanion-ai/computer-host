package store

import (
	"context"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type Store interface {
	CreateMachine(context.Context, model.MachineRecord) error
	GetMachine(context.Context, contracthost.MachineID) (*model.MachineRecord, error)
	ListMachines(context.Context) ([]model.MachineRecord, error)
	UpdateMachine(context.Context, model.MachineRecord) error
	DeleteMachine(context.Context, contracthost.MachineID) error
}
