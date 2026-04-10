package firecracker

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

type capturedRequest struct {
	Method string
	Path   string
	Body   string
}

func TestConfigureMachineEnablesEntropyAndSerialBeforeStart(t *testing.T) {
	var requests []capturedRequest

	socketPath, shutdown := startUnixSocketServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}

		requests = append(requests, capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   string(body),
		})
		w.WriteHeader(http.StatusNoContent)
	})
	defer shutdown()

	client := newAPIClient(socketPath)
	spec := MachineSpec{
		ID:              "vm-1",
		VCPUs:           1,
		MemoryMiB:       512,
		KernelImagePath: "/kernel",
		RootFSPath:      "/rootfs",
	}
	paths := machinePaths{
		JailedSerialLogPath: "/logs/serial.log",
	}
	network := NetworkAllocation{
		InterfaceID: defaultInterfaceID,
		TapName:     "fctap0",
		GuestMAC:    "06:00:ac:10:00:02",
	}

	if err := configureMachine(context.Background(), client, paths, spec, network); err != nil {
		t.Fatalf("configure machine: %v", err)
	}

	gotPaths := make([]string, 0, len(requests))
	for _, request := range requests {
		gotPaths = append(gotPaths, request.Path)
	}
	wantPaths := []string{
		"/machine-config",
		"/boot-source",
		"/drives/root_drive",
		"/network-interfaces/net0",
		"/entropy",
		"/serial",
		"/actions",
	}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("request count mismatch: got %d want %d (%v)", len(gotPaths), len(wantPaths), gotPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Fatalf("request %d mismatch: got %q want %q", i, gotPaths[i], wantPaths[i])
		}
	}
	if requests[4].Body != "{}" {
		t.Fatalf("entropy body mismatch: got %q", requests[4].Body)
	}
	if requests[5].Body != "{\"serial_out_path\":\"/logs/serial.log\"}" {
		t.Fatalf("serial body mismatch: got %q", requests[5].Body)
	}
}

func TestConfigureMachineConfiguresMMDSBeforeStart(t *testing.T) {
	var requests []capturedRequest

	socketPath, shutdown := startUnixSocketServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests = append(requests, capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   string(body),
		})
		w.WriteHeader(http.StatusNoContent)
	})
	defer shutdown()

	client := newAPIClient(socketPath)
	spec := MachineSpec{
		ID:              "vm-2",
		VCPUs:           1,
		MemoryMiB:       512,
		KernelImagePath: "/kernel",
		RootFSPath:      "/rootfs",
		RootDrive: DriveSpec{
			ID:        "root_drive",
			Path:      "/rootfs",
			CacheType: DriveCacheTypeUnsafe,
			IOEngine:  DriveIOEngineSync,
		},
		MMDS: &MMDSSpec{
			NetworkInterfaces: []string{"net0"},
			Version:           MMDSVersionV2,
			IPv4Address:       "169.254.169.254",
			Data: map[string]any{
				"latest": map[string]any{
					"meta-data": map[string]any{
						"microagent": map[string]any{"hostname": "vm-2"},
					},
				},
			},
		},
	}
	paths := machinePaths{JailedSerialLogPath: "/logs/serial.log"}
	network := NetworkAllocation{
		InterfaceID: defaultInterfaceID,
		TapName:     "fctap0",
		GuestMAC:    "06:00:ac:10:00:02",
	}

	if err := configureMachine(context.Background(), client, paths, spec, network); err != nil {
		t.Fatalf("configure machine: %v", err)
	}

	gotPaths := make([]string, 0, len(requests))
	for _, request := range requests {
		gotPaths = append(gotPaths, request.Path)
	}
	wantPaths := []string{
		"/machine-config",
		"/boot-source",
		"/drives/root_drive",
		"/network-interfaces/net0",
		"/mmds/config",
		"/mmds",
		"/entropy",
		"/serial",
		"/actions",
	}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("request count mismatch: got %d want %d (%v)", len(gotPaths), len(wantPaths), gotPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Fatalf("request %d mismatch: got %q want %q", i, gotPaths[i], wantPaths[i])
		}
	}
	if requests[2].Body != "{\"drive_id\":\"root_drive\",\"is_read_only\":false,\"is_root_device\":true,\"path_on_host\":\"/rootfs\",\"cache_type\":\"Unsafe\",\"io_engine\":\"Sync\"}" {
		t.Fatalf("root drive body mismatch: got %q", requests[2].Body)
	}
	if requests[4].Body != "{\"ipv4_address\":\"169.254.169.254\",\"network_interfaces\":[\"net0\"],\"version\":\"V2\"}" {
		t.Fatalf("mmds config body mismatch: got %q", requests[4].Body)
	}
}

func startUnixSocketServer(t *testing.T, handler http.HandlerFunc) (string, func()) {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "firecracker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()

	return socketPath, func() {
		_ = server.Shutdown(context.Background())
		_ = listener.Close()
	}
}
