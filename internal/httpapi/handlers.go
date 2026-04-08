package httpapi

import (
	"context"

	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type Service interface {
	CreateMachine(context.Context, contracthost.CreateMachineRequest) (*contracthost.CreateMachineResponse, error)
	GetMachine(context.Context, contracthost.MachineID) (*contracthost.GetMachineResponse, error)
	ListMachines(context.Context) (*contracthost.ListMachinesResponse, error)
	StopMachine(context.Context, contracthost.MachineID) error
	DeleteMachine(context.Context, contracthost.MachineID) error
	Health(context.Context) (*contracthost.HealthResponse, error)
}
