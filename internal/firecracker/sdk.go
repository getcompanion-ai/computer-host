package firecracker

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

func buildSDKConfig(spec MachineSpec, paths machinePaths, network NetworkAllocation, runtime RuntimeConfig) (sdk.Config, error) {
	if err := spec.Validate(); err != nil {
		return sdk.Config{}, err
	}

	if runtime.FirecrackerBinaryPath == "" {
		return sdk.Config{}, fmt.Errorf("firecracker binary path is required")
	}

	drives := sdk.NewDrivesBuilder(spec.RootFSPath)
	for _, drive := range spec.Drives {
		drives = drives.AddDrive(
			drive.Path,
			drive.ReadOnly,
			sdk.WithDriveID(strings.TrimSpace(drive.ID)),
		)
	}

	cfg := sdk.Config{
		SocketPath:      paths.SocketName,
		KernelImagePath: spec.KernelImagePath,
		KernelArgs:      strings.TrimSpace(spec.KernelArgs),
		Drives:          drives.Build(),
		NetworkInterfaces: sdk.NetworkInterfaces{{
			StaticConfiguration: &sdk.StaticNetworkConfiguration{
				HostDevName: network.TapName,
				MacAddress:  network.GuestMAC,
				IPConfiguration: &sdk.IPConfiguration{
					IPAddr:      toIPNet(network.GuestCIDR),
					Gateway:     net.ParseIP(network.GatewayIP.String()),
					Nameservers: nil,
				},
			},
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  sdk.Int64(spec.VCPUs),
			MemSizeMib: sdk.Int64(spec.MemoryMiB),
			Smt:        sdk.Bool(false),
		},
		JailerCfg: &sdk.JailerConfig{
			GID:            sdk.Int(runtime.JailerGID),
			UID:            sdk.Int(runtime.JailerUID),
			ID:             string(spec.ID),
			NumaNode:       sdk.Int(runtime.NumaNode),
			ExecFile:       runtime.FirecrackerBinaryPath,
			JailerBinary:   runtime.JailerBinaryPath,
			ChrootBaseDir:  paths.JailerBaseDir,
			ChrootStrategy: sdk.NewNaiveChrootStrategy(filepath.Clean(spec.KernelImagePath)),
		},
		VMID: string(spec.ID),
	}

	if spec.Vsock != nil {
		cfg.VsockDevices = []sdk.VsockDevice{{
			ID:   spec.Vsock.ID,
			Path: spec.Vsock.Path,
			CID:  spec.Vsock.CID,
		}}
	}

	return cfg, nil
}

func toIPNet(prefix netip.Prefix) net.IPNet {
	bits := prefix.Bits()
	return net.IPNet{
		IP:   net.ParseIP(prefix.Addr().String()),
		Mask: net.CIDRMask(bits, 32),
	}
}
