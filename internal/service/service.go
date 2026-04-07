package service

import (
	"context"
	"fmt"
	"strings"

	appconfig "github.com/getcompanion-ai/computer-host/internal/config"
	"github.com/getcompanion-ai/computer-host/internal/firecracker"
)

// MachineRuntime is the minimum runtime surface the host-local service needs.
type MachineRuntime interface {
	Boot(context.Context, firecracker.MachineSpec) (*firecracker.MachineState, error)
	Inspect(firecracker.MachineID) (*firecracker.MachineState, error)
	Stop(context.Context, firecracker.MachineID) error
	Delete(context.Context, firecracker.MachineID) error
}

// Service manages local machine lifecycle requests on a single host.
type Service struct {
	config  appconfig.Config
	runtime MachineRuntime
}

// CreateMachineRequest contains the minimum machine creation inputs above the raw runtime layer.
type CreateMachineRequest struct {
	ID firecracker.MachineID
}

const (
	defaultGuestKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off"
	defaultGuestMemoryMiB  = int64(512)
	defaultGuestVCPUs      = int64(1)
)

// New constructs a new host-local service from the app config.
func New(cfg appconfig.Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	runtime, err := firecracker.NewRuntime(cfg.FirecrackerRuntimeConfig())
	if err != nil {
		return nil, err
	}

	return &Service{
		config:  cfg,
		runtime: runtime,
	}, nil
}

// CreateMachine boots a new local machine from the single supported host default shape.
func (s *Service) CreateMachine(ctx context.Context, req CreateMachineRequest) (*firecracker.MachineState, error) {
	spec, err := s.buildMachineSpec(req)
	if err != nil {
		return nil, err
	}
	return s.runtime.Boot(ctx, spec)
}

// GetMachine returns the current local state for a machine.
func (s *Service) GetMachine(id firecracker.MachineID) (*firecracker.MachineState, error) {
	return s.runtime.Inspect(id)
}

// StopMachine stops a running local machine.
func (s *Service) StopMachine(ctx context.Context, id firecracker.MachineID) error {
	return s.runtime.Stop(ctx, id)
}

// DeleteMachine removes a local machine and its host-local resources.
func (s *Service) DeleteMachine(ctx context.Context, id firecracker.MachineID) error {
	return s.runtime.Delete(ctx, id)
}

func (s *Service) buildMachineSpec(req CreateMachineRequest) (firecracker.MachineSpec, error) {
	if s == nil {
		return firecracker.MachineSpec{}, fmt.Errorf("service is required")
	}
	if strings.TrimSpace(string(req.ID)) == "" {
		return firecracker.MachineSpec{}, fmt.Errorf("machine id is required")
	}

	spec := firecracker.MachineSpec{
		ID:              req.ID,
		VCPUs:           defaultGuestVCPUs,
		MemoryMiB:       defaultGuestMemoryMiB,
		KernelImagePath: s.config.KernelImagePath,
		RootFSPath:      s.config.RootFSPath,
		KernelArgs:      defaultGuestKernelArgs,
	}
	if err := spec.Validate(); err != nil {
		return firecracker.MachineSpec{}, err
	}
	return spec, nil
}
