package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/getcompanion-ai/computer-host/internal/model"
	contracthost "github.com/getcompanion-ai/computer-host/contract"
)

type FileStore struct {
	mu             sync.Mutex
	statePath      string
	operationsPath string
}

type persistedOperations struct {
	Operations []model.OperationRecord `json:"operations"`
}

type persistedState struct {
	Artifacts      []model.ArtifactRecord      `json:"artifacts"`
	Machines       []model.MachineRecord       `json:"machines"`
	Volumes        []model.VolumeRecord        `json:"volumes"`
	Snapshots      []model.SnapshotRecord      `json:"snapshots"`
	PublishedPorts []model.PublishedPortRecord `json:"published_ports"`
}

func NewFileStore(statePath string, operationsPath string) (*FileStore, error) {
	store := &FileStore{
		statePath:      filepath.Clean(statePath),
		operationsPath: filepath.Clean(operationsPath),
	}
	if err := initializeJSONFile(store.statePath, emptyPersistedState()); err != nil {
		return nil, err
	}
	if err := initializeJSONFile(store.operationsPath, emptyPersistedOperations()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FileStore) PutArtifact(_ context.Context, record model.ArtifactRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Artifacts {
			if state.Artifacts[i].Ref == record.Ref {
				state.Artifacts[i] = record
				return nil
			}
		}
		state.Artifacts = append(state.Artifacts, record)
		return nil
	})
}

func (s *FileStore) GetArtifact(_ context.Context, ref contracthost.ArtifactRef) (*model.ArtifactRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	for i := range state.Artifacts {
		if state.Artifacts[i].Ref == ref {
			record := state.Artifacts[i]
			return &record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) ListArtifacts(_ context.Context) ([]model.ArtifactRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return append([]model.ArtifactRecord(nil), state.Artifacts...), nil
}

func (s *FileStore) CreateMachine(_ context.Context, record model.MachineRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for _, machine := range state.Machines {
			if machine.ID == record.ID {
				return fmt.Errorf("store: machine %q already exists", record.ID)
			}
		}
		state.Machines = append(state.Machines, record)
		return nil
	})
}

func (s *FileStore) GetMachine(_ context.Context, id contracthost.MachineID) (*model.MachineRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	for i := range state.Machines {
		if state.Machines[i].ID == id {
			record := state.Machines[i]
			return &record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) ListMachines(_ context.Context) ([]model.MachineRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return append([]model.MachineRecord(nil), state.Machines...), nil
}

func (s *FileStore) UpdateMachine(_ context.Context, record model.MachineRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Machines {
			if state.Machines[i].ID == record.ID {
				state.Machines[i] = record
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) DeleteMachine(_ context.Context, id contracthost.MachineID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Machines {
			if state.Machines[i].ID == id {
				state.Machines = append(state.Machines[:i], state.Machines[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) CreateVolume(_ context.Context, record model.VolumeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for _, volume := range state.Volumes {
			if volume.ID == record.ID {
				return fmt.Errorf("store: volume %q already exists", record.ID)
			}
		}
		state.Volumes = append(state.Volumes, record)
		return nil
	})
}

func (s *FileStore) GetVolume(_ context.Context, id contracthost.VolumeID) (*model.VolumeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	for i := range state.Volumes {
		if state.Volumes[i].ID == id {
			record := state.Volumes[i]
			return &record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) ListVolumes(_ context.Context) ([]model.VolumeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return append([]model.VolumeRecord(nil), state.Volumes...), nil
}

func (s *FileStore) UpdateVolume(_ context.Context, record model.VolumeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Volumes {
			if state.Volumes[i].ID == record.ID {
				state.Volumes[i] = record
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) DeleteVolume(_ context.Context, id contracthost.VolumeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Volumes {
			if state.Volumes[i].ID == id {
				state.Volumes = append(state.Volumes[:i], state.Volumes[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) UpsertOperation(_ context.Context, record model.OperationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateOperations(func(operations *persistedOperations) error {
		for i := range operations.Operations {
			if operations.Operations[i].MachineID == record.MachineID {
				operations.Operations[i] = record
				return nil
			}
		}
		operations.Operations = append(operations.Operations, record)
		return nil
	})
}

func (s *FileStore) ListOperations(_ context.Context) ([]model.OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	operations, err := s.readOperations()
	if err != nil {
		return nil, err
	}
	return append([]model.OperationRecord(nil), operations.Operations...), nil
}

func (s *FileStore) DeleteOperation(_ context.Context, machineID contracthost.MachineID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateOperations(func(operations *persistedOperations) error {
		for i := range operations.Operations {
			if operations.Operations[i].MachineID == machineID {
				operations.Operations = append(operations.Operations[:i], operations.Operations[i+1:]...)
				return nil
			}
		}
		return nil
	})
}

func (s *FileStore) CreateSnapshot(_ context.Context, record model.SnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for _, snap := range state.Snapshots {
			if snap.ID == record.ID {
				return fmt.Errorf("store: snapshot %q already exists", record.ID)
			}
		}
		state.Snapshots = append(state.Snapshots, record)
		return nil
	})
}

func (s *FileStore) GetSnapshot(_ context.Context, id contracthost.SnapshotID) (*model.SnapshotRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	for i := range state.Snapshots {
		if state.Snapshots[i].ID == id {
			record := state.Snapshots[i]
			return &record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) ListSnapshotsByMachine(_ context.Context, machineID contracthost.MachineID) ([]model.SnapshotRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	var result []model.SnapshotRecord
	for _, snap := range state.Snapshots {
		if snap.MachineID == machineID {
			result = append(result, snap)
		}
	}
	if result == nil {
		result = []model.SnapshotRecord{}
	}
	return result, nil
}

func (s *FileStore) ListSnapshots(_ context.Context) ([]model.SnapshotRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return append([]model.SnapshotRecord(nil), state.Snapshots...), nil
}

func (s *FileStore) DeleteSnapshot(_ context.Context, id contracthost.SnapshotID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.Snapshots {
			if state.Snapshots[i].ID == id {
				state.Snapshots = append(state.Snapshots[:i], state.Snapshots[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) CreatePublishedPort(_ context.Context, record model.PublishedPortRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for _, port := range state.PublishedPorts {
			if port.ID == record.ID {
				return fmt.Errorf("store: published port %q already exists", record.ID)
			}
			if port.HostPort == record.HostPort {
				return fmt.Errorf("store: host port %d already exists", record.HostPort)
			}
		}
		state.PublishedPorts = append(state.PublishedPorts, record)
		return nil
	})
}

func (s *FileStore) GetPublishedPort(_ context.Context, id contracthost.PublishedPortID) (*model.PublishedPortRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	for i := range state.PublishedPorts {
		if state.PublishedPorts[i].ID == id {
			record := state.PublishedPorts[i]
			return &record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) ListPublishedPorts(_ context.Context, machineID contracthost.MachineID) ([]model.PublishedPortRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	result := make([]model.PublishedPortRecord, 0, len(state.PublishedPorts))
	for _, port := range state.PublishedPorts {
		if machineID != "" && port.MachineID != machineID {
			continue
		}
		result = append(result, port)
	}
	return result, nil
}

func (s *FileStore) DeletePublishedPort(_ context.Context, id contracthost.PublishedPortID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateState(func(state *persistedState) error {
		for i := range state.PublishedPorts {
			if state.PublishedPorts[i].ID == id {
				state.PublishedPorts = append(state.PublishedPorts[:i], state.PublishedPorts[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *FileStore) readOperations() (*persistedOperations, error) {
	var operations persistedOperations
	if err := readJSONFile(s.operationsPath, &operations); err != nil {
		return nil, err
	}
	normalizeOperations(&operations)
	return &operations, nil
}

func (s *FileStore) readState() (*persistedState, error) {
	var state persistedState
	if err := readJSONFile(s.statePath, &state); err != nil {
		return nil, err
	}
	normalizeState(&state)
	return &state, nil
}

func (s *FileStore) updateOperations(update func(*persistedOperations) error) error {
	operations, err := s.readOperations()
	if err != nil {
		return err
	}
	if err := update(operations); err != nil {
		return err
	}
	return writeJSONFileAtomically(s.operationsPath, operations)
}

func (s *FileStore) updateState(update func(*persistedState) error) error {
	state, err := s.readState()
	if err != nil {
		return err
	}
	if err := update(state); err != nil {
		return err
	}
	return writeJSONFileAtomically(s.statePath, state)
}

func initializeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create store dir for %q: %w", path, err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat store file %q: %w", path, err)
	}
	return writeJSONFileAtomically(path, value)
}

func readJSONFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read store file %q: %w", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode store file %q: %w", path, err)
	}
	return nil
}

func writeJSONFileAtomically(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal store file %q: %w", path, err)
	}
	payload = append(payload, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create store dir for %q: %w", path, err)
	}

	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open temp store file %q: %w", tmpPath, err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temp store file %q: %w", tmpPath, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temp store file %q: %w", tmpPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temp store file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp store file %q to %q: %w", tmpPath, path, err)
	}

	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open store dir for %q: %w", path, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync store dir for %q: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close store dir for %q: %w", path, err)
	}
	return nil
}

func emptyPersistedState() persistedState {
	return persistedState{
		Artifacts:      []model.ArtifactRecord{},
		Machines:       []model.MachineRecord{},
		Volumes:        []model.VolumeRecord{},
		Snapshots:      []model.SnapshotRecord{},
		PublishedPorts: []model.PublishedPortRecord{},
	}
}

func emptyPersistedOperations() persistedOperations {
	return persistedOperations{Operations: []model.OperationRecord{}}
}

func normalizeState(state *persistedState) {
	if state.Artifacts == nil {
		state.Artifacts = []model.ArtifactRecord{}
	}
	if state.Machines == nil {
		state.Machines = []model.MachineRecord{}
	}
	if state.Volumes == nil {
		state.Volumes = []model.VolumeRecord{}
	}
	if state.Snapshots == nil {
		state.Snapshots = []model.SnapshotRecord{}
	}
	if state.PublishedPorts == nil {
		state.PublishedPorts = []model.PublishedPortRecord{}
	}
}

func normalizeOperations(operations *persistedOperations) {
	if operations.Operations == nil {
		operations.Operations = []model.OperationRecord{}
	}
}
