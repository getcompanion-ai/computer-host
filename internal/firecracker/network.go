package firecracker

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

const (
	defaultNetworkCIDR       = "172.16.0.0/16"
	defaultNetworkPrefixBits = 30
	defaultInterfaceID       = "net0"
	defaultTapPrefix         = "fctap"
	defaultNFTTableName      = "microagentcomputer"
	defaultNFTPostrouting    = "postrouting"
	defaultNFTForward        = "forward"
	defaultIPForwardPath     = "/proc/sys/net/ipv4/ip_forward"
)

// NetworkAllocation describes the concrete host-local network values assigned to a machine
type NetworkAllocation struct {
	InterfaceID string
	TapName     string
	HostCIDR    netip.Prefix
	GuestCIDR   netip.Prefix
	GatewayIP   netip.Addr
	GuestMAC    string
}

// NetworkAllocator allocates /30 tap networks to machines.
type NetworkAllocator struct {
	basePrefix netip.Prefix
}

// NetworkProvisioner prepares the host-side tap device for a machine.
type NetworkProvisioner interface {
	Ensure(context.Context, NetworkAllocation) error
	Remove(context.Context, NetworkAllocation) error
}

// IPTapProvisioner provisions tap devices through the `ip` CLI.
type IPTapProvisioner struct {
	guestCIDR         string
	egressInterface   string
	runCommand        func(context.Context, string, ...string) error
	readCommandOutput func(context.Context, string, ...string) (string, error)
	readFile          func(string) ([]byte, error)
	writeFile         func(string, []byte, os.FileMode) error
}

// GuestIP returns the guest IP address.
func (n NetworkAllocation) GuestIP() netip.Addr {
	return n.GuestCIDR.Addr()
}

// AllocationFromGuestIP reconstructs the host-side allocation from a guest IP and tap name.
func AllocationFromGuestIP(guestIP string, tapName string) (NetworkAllocation, error) {
	parsed := net.ParseIP(strings.TrimSpace(guestIP))
	if parsed == nil {
		return NetworkAllocation{}, fmt.Errorf("parse guest ip %q", guestIP)
	}
	addr, ok := netip.AddrFromSlice(parsed.To4())
	if !ok {
		return NetworkAllocation{}, fmt.Errorf("guest ip %q must be IPv4", guestIP)
	}

	base := ipv4ToUint32(addr) - 2
	hostIP := uint32ToIPv4(base + 1)
	guest := uint32ToIPv4(base + 2)
	return NetworkAllocation{
		InterfaceID: defaultInterfaceID,
		TapName:     strings.TrimSpace(tapName),
		HostCIDR:    netip.PrefixFrom(hostIP, defaultNetworkPrefixBits),
		GuestCIDR:   netip.PrefixFrom(guest, defaultNetworkPrefixBits),
		GatewayIP:   hostIP,
		GuestMAC:    macForIPv4(guest),
	}, nil
}

// NewNetworkAllocator returns a new /30 allocator rooted at the provided IPv4 prefix.
func NewNetworkAllocator(cidr string) (*NetworkAllocator, error) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		cidr = defaultNetworkCIDR
	}

	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse network cidr %q: %w", cidr, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("network cidr %q must be IPv4", cidr)
	}
	if prefix.Bits() > defaultNetworkPrefixBits {
		return nil, fmt.Errorf("network cidr %q must be no more specific than /%d", cidr, defaultNetworkPrefixBits)
	}

	return &NetworkAllocator{basePrefix: prefix}, nil
}

// Allocate chooses the first free /30 network not present in used.
func (a *NetworkAllocator) Allocate(used []NetworkAllocation) (NetworkAllocation, error) {
	if a == nil {
		return NetworkAllocation{}, fmt.Errorf("network allocator is required")
	}

	allocated := make(map[netip.Addr]struct{}, len(used))
	for _, network := range used {
		if network.GuestIP().IsValid() {
			allocated[network.GuestIP()] = struct{}{}
		}
	}

	totalSubnets := 1 << uint(defaultNetworkPrefixBits-a.basePrefix.Bits())
	for i := range totalSubnets {
		network, err := a.networkForIndex(i)
		if err != nil {
			return NetworkAllocation{}, err
		}
		if _, exists := allocated[network.GuestIP()]; exists {
			continue
		}
		return network, nil
	}

	return NetworkAllocation{}, fmt.Errorf("network cidr %q is exhausted", a.basePrefix)
}

func (a *NetworkAllocator) networkForIndex(index int) (NetworkAllocation, error) {
	if index < 0 {
		return NetworkAllocation{}, fmt.Errorf("network index must be non-negative")
	}

	base := ipv4ToUint32(a.basePrefix.Addr())
	subnetBase := base + uint32(index*4)
	hostIP := uint32ToIPv4(subnetBase + 1)
	guestIP := uint32ToIPv4(subnetBase + 2)

	return NetworkAllocation{
		InterfaceID: defaultInterfaceID,
		TapName:     fmt.Sprintf("%s%d", defaultTapPrefix, index),
		HostCIDR:    netip.PrefixFrom(hostIP, defaultNetworkPrefixBits),
		GuestCIDR:   netip.PrefixFrom(guestIP, defaultNetworkPrefixBits),
		GatewayIP:   hostIP,
		GuestMAC:    macForIPv4(guestIP),
	}, nil
}

// NewIPTapProvisioner returns a provisioner backed by `ip`.
func NewIPTapProvisioner(guestCIDR string, egressInterface string) *IPTapProvisioner {
	return &IPTapProvisioner{
		guestCIDR:       strings.TrimSpace(guestCIDR),
		egressInterface: strings.TrimSpace(egressInterface),
		runCommand: func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
			}
			return nil
		},
		readCommandOutput: func(ctx context.Context, name string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
			}
			return string(output), nil
		},
		readFile:  os.ReadFile,
		writeFile: os.WriteFile,
	}
}

// Ensure creates and brings up the tap device with the host-side address.
func (p *IPTapProvisioner) Ensure(ctx context.Context, network NetworkAllocation) error {
	if p == nil || p.runCommand == nil {
		return fmt.Errorf("network provisioner is required")
	}
	if strings.TrimSpace(network.TapName) == "" {
		return fmt.Errorf("tap name is required")
	}
	if err := p.ensureHostNetworking(ctx); err != nil {
		return err
	}

	err := p.runCommand(ctx, "ip", "tuntap", "add", "dev", network.TapName, "mode", "tap")
	if err != nil {
		lower := strings.ToLower(err.Error())
		if !strings.Contains(lower, "file exists") && !strings.Contains(lower, "device or resource busy") {
			return fmt.Errorf("create tap device %q: %w", network.TapName, err)
		}
		if removeErr := p.Remove(ctx, network); removeErr != nil {
			return fmt.Errorf("remove stale tap device %q: %w", network.TapName, removeErr)
		}
		if err := p.runCommand(ctx, "ip", "tuntap", "add", "dev", network.TapName, "mode", "tap"); err != nil {
			return fmt.Errorf("create tap device %q after cleanup: %w", network.TapName, err)
		}
	}

	if err := p.runCommand(ctx, "ip", "addr", "replace", network.HostCIDR.String(), "dev", network.TapName); err != nil {
		return fmt.Errorf("assign host address to %q: %w", network.TapName, err)
	}
	if err := p.runCommand(ctx, "ip", "link", "set", "dev", network.TapName, "up"); err != nil {
		return fmt.Errorf("bring up tap device %q: %w", network.TapName, err)
	}
	return nil
}

func (p *IPTapProvisioner) ensureHostNetworking(ctx context.Context) error {
	if strings.TrimSpace(p.egressInterface) == "" {
		return fmt.Errorf("egress interface is required")
	}
	if strings.TrimSpace(p.guestCIDR) == "" {
		return fmt.Errorf("guest cidr is required")
	}
	if err := p.runCommand(ctx, "ip", "link", "show", "dev", p.egressInterface); err != nil {
		return fmt.Errorf("validate egress interface %q: %w", p.egressInterface, err)
	}
	if err := p.ensureIPForwarding(); err != nil {
		return err
	}
	if err := p.ensureNFTTable(ctx); err != nil {
		return err
	}
	if err := p.ensureNFTChain(ctx, defaultNFTPostrouting, "{ type nat hook postrouting priority srcnat; policy accept; }"); err != nil {
		return err
	}
	if err := p.ensureNFTChain(ctx, defaultNFTForward, "{ type filter hook forward priority filter; policy accept; }"); err != nil {
		return err
	}
	if err := p.ensureNFTRule(ctx, defaultNFTPostrouting, "microagentcomputer-postrouting", "ip saddr %s oifname %q counter masquerade comment %q", p.guestCIDR, p.egressInterface, "microagentcomputer-postrouting"); err != nil {
		return err
	}
	if err := p.ensureNFTRule(ctx, defaultNFTForward, "microagentcomputer-forward-out", "iifname %q oifname %q counter accept comment %q", defaultTapPrefix+"*", p.egressInterface, "microagentcomputer-forward-out"); err != nil {
		return err
	}
	if err := p.ensureNFTRule(ctx, defaultNFTForward, "microagentcomputer-forward-in", "iifname %q oifname %q ct state related,established counter accept comment %q", p.egressInterface, defaultTapPrefix+"*", "microagentcomputer-forward-in"); err != nil {
		return err
	}
	return nil
}

func (p *IPTapProvisioner) ensureIPForwarding() error {
	if p.readFile == nil || p.writeFile == nil {
		return fmt.Errorf("ip forwarding helpers are required")
	}
	payload, err := p.readFile(defaultIPForwardPath)
	if err != nil {
		return fmt.Errorf("read ip forwarding state: %w", err)
	}
	if strings.TrimSpace(string(payload)) == "1" {
		return nil
	}
	if err := p.writeFile(defaultIPForwardPath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	return nil
}

func (p *IPTapProvisioner) ensureNFTTable(ctx context.Context) error {
	if _, err := p.readCommandOutput(ctx, "nft", "list", "table", "ip", defaultNFTTableName); err == nil {
		return nil
	}
	if err := p.runCommand(ctx, "nft", "add", "table", "ip", defaultNFTTableName); err != nil {
		return fmt.Errorf("ensure nft table %q: %w", defaultNFTTableName, err)
	}
	return nil
}

func (p *IPTapProvisioner) ensureNFTChain(ctx context.Context, chain string, definition string) error {
	if _, err := p.readCommandOutput(ctx, "nft", "list", "chain", "ip", defaultNFTTableName, chain); err == nil {
		return nil
	}
	if err := p.runCommand(ctx, "nft", "add", "chain", "ip", defaultNFTTableName, chain, definition); err != nil {
		return fmt.Errorf("ensure nft chain %q: %w", chain, err)
	}
	return nil
}

func (p *IPTapProvisioner) ensureNFTRule(ctx context.Context, chain string, comment string, format string, args ...any) error {
	output, err := p.readCommandOutput(ctx, "nft", "list", "chain", "ip", defaultNFTTableName, chain)
	if err != nil {
		return fmt.Errorf("list nft chain %q: %w", chain, err)
	}
	if strings.Contains(output, fmt.Sprintf("comment \"%s\"", comment)) {
		return nil
	}
	rule := fmt.Sprintf(format, args...)
	if err := p.runCommand(ctx, "nft", "add", "rule", "ip", defaultNFTTableName, chain, rule); err != nil {
		return fmt.Errorf("ensure nft rule %q: %w", comment, err)
	}
	return nil
}

// Remove deletes the tap device if it exists.
func (p *IPTapProvisioner) Remove(ctx context.Context, network NetworkAllocation) error {
	if p == nil || p.runCommand == nil {
		return fmt.Errorf("network provisioner is required")
	}
	if strings.TrimSpace(network.TapName) == "" {
		return nil
	}
	if err := p.runCommand(ctx, "ip", "link", "del", "dev", network.TapName); err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "cannot find device") || strings.Contains(lower, "does not exist") {
			return nil
		}
		return fmt.Errorf("remove tap device %q: %w", network.TapName, err)
	}
	return nil
}

func ipv4ToUint32(ip netip.Addr) uint32 {
	bytes := ip.As4()
	return binary.BigEndian.Uint32(bytes[:])
}

func uint32ToIPv4(value uint32) netip.Addr {
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes)
}

func macForIPv4(ip netip.Addr) string {
	bytes := ip.As4()
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", bytes[0], bytes[1], bytes[2], bytes[3])
}
