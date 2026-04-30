package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	contracthost "github.com/AgentComputerAI/computer-host/contract"
)

type Service interface {
	CreateMachine(context.Context, contracthost.CreateMachineRequest) (*contracthost.CreateMachineResponse, error)
	GetMachine(context.Context, contracthost.MachineID) (*contracthost.GetMachineResponse, error)
	ListMachines(context.Context) (*contracthost.ListMachinesResponse, error)
	StartMachine(context.Context, contracthost.MachineID) (*contracthost.GetMachineResponse, error)
	ResizeMachine(context.Context, contracthost.MachineID, contracthost.ResizeMachineRequest) (*contracthost.ResizeMachineResponse, error)
	EnsureExecRelay(context.Context, contracthost.MachineID) (*contracthost.GetMachineResponse, error)
	ExecCommand(context.Context, contracthost.MachineID, contracthost.ExecRequest) (*contracthost.ExecResponse, error)
	FileOperation(context.Context, contracthost.MachineID, contracthost.FileOperationRequest) (*contracthost.FileOperationResponse, error)
	StopMachine(context.Context, contracthost.MachineID) error
	DeleteMachine(context.Context, contracthost.MachineID) error
	Health(context.Context) (*contracthost.HealthResponse, error)
	GetStorageReport(context.Context) (*contracthost.GetStorageReportResponse, error)
	CreateSnapshot(context.Context, contracthost.MachineID, contracthost.CreateSnapshotRequest) (*contracthost.CreateSnapshotResponse, error)
	UploadSnapshot(context.Context, contracthost.SnapshotID, contracthost.UploadSnapshotRequest) (*contracthost.UploadSnapshotResponse, error)
	ListSnapshots(context.Context, contracthost.MachineID) (*contracthost.ListSnapshotsResponse, error)
	GetSnapshot(context.Context, contracthost.SnapshotID) (*contracthost.GetSnapshotResponse, error)
	GetSnapshotArtifact(context.Context, contracthost.SnapshotID, string) (*SnapshotArtifactContent, error)
	DeleteSnapshotByID(context.Context, contracthost.SnapshotID) error
	RestoreSnapshot(context.Context, contracthost.SnapshotID, contracthost.RestoreSnapshotRequest) (*contracthost.RestoreSnapshotResponse, error)
	CreatePublishedPort(context.Context, contracthost.MachineID, contracthost.CreatePublishedPortRequest) (*contracthost.CreatePublishedPortResponse, error)
	ListPublishedPorts(context.Context, contracthost.MachineID) (*contracthost.ListPublishedPortsResponse, error)
	DeletePublishedPort(context.Context, contracthost.MachineID, contracthost.PublishedPortID) error
	CreateMount(context.Context, contracthost.MachineID, contracthost.CreateMountRequest) (*contracthost.CreateMountResponse, error)
	ListMounts(context.Context, contracthost.MachineID) (*contracthost.ListMountsResponse, error)
	DeleteMount(context.Context, contracthost.MachineID, contracthost.MountID) error
}

type SnapshotArtifactContent struct {
	Name string
	Path string
}

type Handler struct {
	service Service
}

func New(service Service) (*Handler, error) {
	if service == nil {
		return nil, fmt.Errorf("service is required")
	}
	return &Handler{service: service}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/storage/report", h.handleStorageReport)
	mux.HandleFunc("/machines", h.handleMachines)
	mux.HandleFunc("/machines/", h.handleMachine)
	mux.HandleFunc("/snapshots/", h.handleSnapshot)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	response, err := h.service.Health(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleStorageReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	response, err := h.service.GetStorageReport(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleMachines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		response, err := h.service.ListMachines(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodPost:
		var request contracthost.CreateMachineRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.CreateMachine(r.Context(), request)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	default:
		writeMethodNotAllowed(w)
	}
}

func (h *Handler) handleMachine(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/machines/")
	if path == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("machine id is required"))
		return
	}
	parts := strings.Split(path, "/")
	machineID := contracthost.MachineID(parts[0])

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			response, err := h.service.GetMachine(r.Context(), machineID)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case http.MethodDelete:
			if err := h.service.DeleteMachine(r.Context(), machineID); err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeMethodNotAllowed(w)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "stop" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		if err := h.service.StopMachine(r.Context(), machineID); err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(parts) == 2 && parts[1] == "start" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		response, err := h.service.StartMachine(r.Context(), machineID)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 2 && parts[1] == "resize" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		var request contracthost.ResizeMachineRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.ResizeMachine(r.Context(), machineID, request)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 2 && parts[1] == "exec" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		var request contracthost.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.ExecCommand(r.Context(), machineID, request)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 3 && parts[1] == "files" && parts[2] == "ops" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		var request contracthost.FileOperationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.FileOperation(r.Context(), machineID, request)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 2 && parts[1] == "exec-relay" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		response, err := h.service.EnsureExecRelay(r.Context(), machineID)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 2 && parts[1] == "snapshots" {
		switch r.Method {
		case http.MethodGet:
			response, err := h.service.ListSnapshots(r.Context(), machineID)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case http.MethodPost:
			var request contracthost.CreateSnapshotRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			response, err := h.service.CreateSnapshot(r.Context(), machineID, request)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusCreated, response)
		default:
			writeMethodNotAllowed(w)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "published-ports" {
		switch r.Method {
		case http.MethodGet:
			response, err := h.service.ListPublishedPorts(r.Context(), machineID)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case http.MethodPost:
			var request contracthost.CreatePublishedPortRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			response, err := h.service.CreatePublishedPort(r.Context(), machineID, request)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusCreated, response)
		default:
			writeMethodNotAllowed(w)
		}
		return
	}

	if len(parts) == 3 && parts[1] == "published-ports" {
		if r.Method != http.MethodDelete {
			writeMethodNotAllowed(w)
			return
		}
		if err := h.service.DeletePublishedPort(r.Context(), machineID, contracthost.PublishedPortID(parts[2])); err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(parts) == 2 && parts[1] == "mounts" {
		switch r.Method {
		case http.MethodGet:
			response, err := h.service.ListMounts(r.Context(), machineID)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case http.MethodPost:
			var request contracthost.CreateMountRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			response, err := h.service.CreateMount(r.Context(), machineID, request)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusCreated, response)
		default:
			writeMethodNotAllowed(w)
		}
		return
	}

	if len(parts) == 3 && parts[1] == "mounts" {
		if r.Method != http.MethodDelete {
			writeMethodNotAllowed(w)
			return
		}
		if err := h.service.DeleteMount(r.Context(), machineID, contracthost.MountID(parts[2])); err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writeError(w, http.StatusNotFound, fmt.Errorf("route not found"))
}

func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/snapshots/")
	if path == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("snapshot id is required"))
		return
	}
	parts := strings.Split(path, "/")
	snapshotID := contracthost.SnapshotID(parts[0])

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			response, err := h.service.GetSnapshot(r.Context(), snapshotID)
			if err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case http.MethodDelete:
			if err := h.service.DeleteSnapshotByID(r.Context(), snapshotID); err != nil {
				writeError(w, statusForError(err), err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeMethodNotAllowed(w)
		}
		return
	}

	if len(parts) == 2 && parts[1] == "restore" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		var req contracthost.RestoreSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.RestoreSnapshot(r.Context(), snapshotID, req)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
		return
	}

	if len(parts) == 2 && parts[1] == "upload" {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		var req contracthost.UploadSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		response, err := h.service.UploadSnapshot(r.Context(), snapshotID, req)
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	if len(parts) == 3 && parts[1] == "artifacts" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		artifact, err := h.service.GetSnapshotArtifact(r.Context(), snapshotID, parts[2])
		if err != nil {
			writeError(w, statusForError(err), err)
			return
		}
		if artifact == nil {
			writeError(w, http.StatusNotFound, fmt.Errorf("snapshot artifact %q not found", parts[2]))
			return
		}
		if artifact.Name != "" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", artifact.Name))
		}
		http.ServeFile(w, r, artifact.Path)
		return
	}

	writeError(w, http.StatusNotFound, fmt.Errorf("route not found"))
}

func statusForError(err error) int {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"):
		return http.StatusNotFound
	case strings.Contains(message, "already exists"):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}
