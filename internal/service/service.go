package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// CreateMachineRequest contains the minimum machine creation inputs above the
// raw runtime layer.
type CreateMachineRequest struct {
	ID              firecracker.MachineID
	KernelImagePath string
	RootFSPath      string
	KernelArgs      string
	VCPUs           int64
	MemoryMiB       int64
	Drives          []firecracker.DriveSpec
	VSock           *firecracker.VsockSpec
}

// New constructs a new host-local service from the app config.
func New(cfg appconfig.Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.VSock.Enabled {
		if err := os.MkdirAll(cfg.VSock.BaseDir, 0o755); err != nil {
			return nil, fmt.Errorf("create vsock base dir %q: %w", cfg.VSock.BaseDir, err)
		}
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

// CreateMachine boots a new local machine using config defaults plus request
// overrides.
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
	if strings.TrimSpace(string(req.ID)) == "" {
		return firecracker.MachineSpec{}, fmt.Errorf("machine id is required")
	}
	if s == nil {
		return firecracker.MachineSpec{}, fmt.Errorf("service is required")
	}

	spec := firecracker.MachineSpec{
		ID:              req.ID,
		VCPUs:           s.config.Machine.VCPUs,
		MemoryMiB:       s.config.Machine.MemoryMiB,
		KernelImagePath: s.config.Machine.KernelImagePath,
		RootFSPath:      s.config.Machine.RootFSPath,
		KernelArgs:      s.config.Machine.KernelArgs,
		Drives:          append([]firecracker.DriveSpec(nil), req.Drives...),
	}

	if value := strings.TrimSpace(req.KernelImagePath); value != "" {
		spec.KernelImagePath = value
	}
	if value := strings.TrimSpace(req.RootFSPath); value != "" {
		spec.RootFSPath = value
	}
	if value := strings.TrimSpace(req.KernelArgs); value != "" {
		spec.KernelArgs = value
	}
	if req.VCPUs > 0 {
		spec.VCPUs = req.VCPUs
	}
	if req.MemoryMiB > 0 {
		spec.MemoryMiB = req.MemoryMiB
	}
	if req.VSock != nil {
		vsock := *req.VSock
		spec.Vsock = &vsock
	} else if s.config.VSock.Enabled {
		spec.Vsock = &firecracker.VsockSpec{
			ID:   s.config.VSock.ID,
			CID:  s.config.VSock.CID,
			Path: filepath.Join(s.config.VSock.BaseDir, string(req.ID)+".sock"),
		}
	}

	if err := spec.Validate(); err != nil {
		return firecracker.MachineSpec{}, err
	}
	return spec, nil
}
