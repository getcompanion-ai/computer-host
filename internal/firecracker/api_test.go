package firecracker

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestPutSnapshotLoadIncludesNetworkOverrides(t *testing.T) {
	var (
		gotPath string
		gotBody string
	)

	socketPath, shutdown := startUnixSocketServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotPath = r.URL.Path
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	})
	defer shutdown()

	client := newAPIClient(socketPath)
	err := client.PutSnapshotLoad(context.Background(), SnapshotLoadParams{
		SnapshotPath: "vmstate.bin",
		MemBackend: &MemBackend{
			BackendType: "File",
			BackendPath: "memory.bin",
		},
		ResumeVm: false,
		NetworkOverrides: []NetworkOverride{
			{
				IfaceID:     "net0",
				HostDevName: "fctap7",
			},
		},
		VsockOverride: &VsockOverride{UDSPath: "/run/microagent-personalizer.vsock"},
	})
	if err != nil {
		t.Fatalf("put snapshot load: %v", err)
	}

	if gotPath != "/snapshot/load" {
		t.Fatalf("request path mismatch: got %q want %q", gotPath, "/snapshot/load")
	}

	want := "{\"snapshot_path\":\"vmstate.bin\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"memory.bin\"},\"resume_vm\":false,\"network_overrides\":[{\"iface_id\":\"net0\",\"host_dev_name\":\"fctap7\"}],\"vsock_override\":{\"uds_path\":\"/run/microagent-personalizer.vsock\"}}"
	if gotBody != want {
		t.Fatalf("request body mismatch:\n got: %s\nwant: %s", gotBody, want)
	}
}
