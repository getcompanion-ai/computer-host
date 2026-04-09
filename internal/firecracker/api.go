package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type actionRequest struct {
	ActionType string `json:"action_type"`
}

type apiClient struct {
	httpClient *http.Client
	socketPath string
}

type bootSourceRequest struct {
	BootArgs        string `json:"boot_args,omitempty"`
	KernelImagePath string `json:"kernel_image_path"`
}

type driveRequest struct {
	DriveID      string `json:"drive_id"`
	IsReadOnly   bool   `json:"is_read_only"`
	IsRootDevice bool   `json:"is_root_device"`
	PathOnHost   string `json:"path_on_host"`
}

type entropyRequest struct{}

type faultResponse struct {
	FaultMessage string `json:"fault_message"`
}

type instanceInfo struct {
	AppName    string `json:"app_name,omitempty"`
	ID         string `json:"id,omitempty"`
	State      string `json:"state,omitempty"`
	VMMVersion string `json:"vmm_version,omitempty"`
}

type machineConfigRequest struct {
	MemSizeMib int64 `json:"mem_size_mib"`
	Smt        bool  `json:"smt,omitempty"`
	VcpuCount  int64 `json:"vcpu_count"`
}

type networkInterfaceRequest struct {
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name"`
	IfaceID     string `json:"iface_id"`
}

type serialRequest struct {
	SerialOutPath string `json:"serial_out_path"`
}

type vsockRequest struct {
	GuestCID int64  `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

const defaultStartAction = "InstanceStart"

func newAPIClient(socketPath string) *apiClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}

	return &apiClient{
		httpClient: &http.Client{Transport: transport},
		socketPath: socketPath,
	}
}

func (c *apiClient) Ping(ctx context.Context) error {
	var info instanceInfo
	return c.do(ctx, http.MethodGet, "/", nil, &info, http.StatusOK)
}

func (c *apiClient) PutAction(ctx context.Context, action string) error {
	return c.do(ctx, http.MethodPut, "/actions", actionRequest{ActionType: action}, nil, http.StatusNoContent)
}

func (c *apiClient) PutBootSource(ctx context.Context, spec MachineSpec) error {
	body := bootSourceRequest{KernelImagePath: spec.KernelImagePath}
	if value := strings.TrimSpace(spec.KernelArgs); value != "" {
		body.BootArgs = value
	}
	return c.do(ctx, http.MethodPut, "/boot-source", body, nil, http.StatusNoContent)
}

func (c *apiClient) PutDrive(ctx context.Context, drive driveRequest) error {
	endpoint := "/drives/" + url.PathEscape(drive.DriveID)
	return c.do(ctx, http.MethodPut, endpoint, drive, nil, http.StatusNoContent)
}

func (c *apiClient) PutEntropy(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/entropy", entropyRequest{}, nil, http.StatusNoContent)
}

func (c *apiClient) PutMachineConfig(ctx context.Context, spec MachineSpec) error {
	body := machineConfigRequest{
		MemSizeMib: spec.MemoryMiB,
		Smt:        false,
		VcpuCount:  spec.VCPUs,
	}
	return c.do(ctx, http.MethodPut, "/machine-config", body, nil, http.StatusNoContent)
}

func (c *apiClient) PutNetworkInterface(ctx context.Context, network NetworkAllocation) error {
	body := networkInterfaceRequest{
		GuestMAC:    network.GuestMAC,
		HostDevName: network.TapName,
		IfaceID:     network.InterfaceID,
	}
	endpoint := "/network-interfaces/" + url.PathEscape(network.InterfaceID)
	return c.do(ctx, http.MethodPut, endpoint, body, nil, http.StatusNoContent)
}

func (c *apiClient) PutSerial(ctx context.Context, serialOutPath string) error {
	return c.do(
		ctx,
		http.MethodPut,
		"/serial",
		serialRequest{SerialOutPath: serialOutPath},
		nil,
		http.StatusNoContent,
	)
}

func (c *apiClient) PutVsock(ctx context.Context, spec VsockSpec) error {
	body := vsockRequest{
		GuestCID: int64(spec.CID),
		UDSPath:  spec.Path,
	}
	return c.do(ctx, http.MethodPut, "/vsock", body, nil, http.StatusNoContent)
}

type VmState string

const (
	VmStatePaused  VmState = "Paused"
	VmStateResumed VmState = "Resumed"
)

type vmRequest struct {
	State VmState `json:"state"`
}

type vmResponse struct {
	State string `json:"state"`
}

type SnapshotCreateParams struct {
	MemFilePath  string `json:"mem_file_path"`
	SnapshotPath string `json:"snapshot_path"`
	SnapshotType string `json:"snapshot_type"`
}

type SnapshotLoadParams struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       *MemBackend       `json:"mem_backend,omitempty"`
	ResumeVm         bool              `json:"resume_vm"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *VsockOverride    `json:"vsock_override,omitempty"`
}

type MemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

func (c *apiClient) PatchVm(ctx context.Context, state VmState) error {
	return c.do(ctx, http.MethodPatch, "/vm", vmRequest{State: state}, nil, http.StatusNoContent)
}

func (c *apiClient) GetVm(ctx context.Context) (*vmResponse, error) {
	var response vmResponse
	if err := c.do(ctx, http.MethodGet, "/vm", nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *apiClient) PutSnapshotCreate(ctx context.Context, params SnapshotCreateParams) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", params, nil, http.StatusNoContent)
}

func (c *apiClient) PutSnapshotLoad(ctx context.Context, params SnapshotLoadParams) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", params, nil, http.StatusNoContent)
}

func (c *apiClient) do(ctx context.Context, method string, endpoint string, input any, output any, wantStatus int) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("marshal %s %s request: %w", method, endpoint, err)
		}
		body = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+endpoint, body)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, endpoint, err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("do %s %s via %q: %w", method, endpoint, c.socketPath, err)
	}
	defer response.Body.Close()

	if response.StatusCode != wantStatus {
		return decodeFirecrackerError(method, endpoint, response)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
	}
	return nil
}

func decodeFirecrackerError(method string, endpoint string, response *http.Response) error {
	payload, _ := io.ReadAll(response.Body)
	var fault faultResponse
	if err := json.Unmarshal(payload, &fault); err == nil && strings.TrimSpace(fault.FaultMessage) != "" {
		return fmt.Errorf("%s %s: status %d: %s", method, endpoint, response.StatusCode, strings.TrimSpace(fault.FaultMessage))
	}

	message := strings.TrimSpace(string(payload))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("%s %s: status %d: %s", method, endpoint, response.StatusCode, message)
}
