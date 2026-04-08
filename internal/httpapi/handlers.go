package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	mux.HandleFunc("/machines", h.handleMachines)
	mux.HandleFunc("/machines/", h.handleMachine)
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
