//go:build darwin && arm64

package vm

import (
	"fmt"
	"net/netip"

	"github.com/farcloser/ossein/third_party/vz/vmnet"
)

// Network is a vmnet network ossein created for ONE microVM, plus the
// addressing the guest needs to use it.
//
// This replaces Virtualization.framework's NAT attachment
// (VZNATNetworkDeviceAttachment), and the reason is not performance, it is
// ownership. That attachment silently joins the host-wide vmnet SHARED
// network: one bridge, one subnet, one DHCP server, shared by every VM
// product on the machine. Its configuration is system state
// (/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist,
// /etc/bootpd.plist) that any root-privileged peer rewrites for everyone —
// observed 2026-08-01, a peer client renumbered the network from
// 192.168.64.0/24 to 192.168.138.0/23 and left bootpd with dhcp_enabled
// false, after which every ossein guest failed to boot while the bridge and
// NAT engine kept working for its owner. There is no defensive fix for that
// from inside a tenant.
//
// A per-VM network removes the shared state entirely: the framework
// allocates a distinct subnet per network (measured: two concurrent VMs got
// 192.168.64.0/24 and 192.168.65.0/24), arbitrates collisions itself
// (creating a network on a subnet already in use fails at create time
// rather than corrupting routing), and reaps the bridge with the process.
//
// It also removes DHCP from the boot path. Because WE chose the network, the
// address is known host-side before the VM starts, so the guest is
// configured statically over the netlink RPCs — about a millisecond, against
// the 85-300ms a lease acquisition cost, which was the floor small-image
// boots hit.
//
// Requires macOS 26 (vmnet_network_configuration). It needs neither the
// com.apple.vm.networking entitlement nor root: ossein's existing
// entitlements are enough (verified). The binding is carried in-tree —
// see third_party/vz/FORK.md.
type Network struct {
	// Gateway is the network's host-side address: the guest's default route,
	// its NAT inside address, and where the framework answers DNS.
	Gateway netip.Addr
	// Guest is the address assigned to this VM.
	Guest netip.Addr
	// Prefix is the subnet the framework allocated.
	Prefix netip.Prefix

	network *vmnet.Network
}

// GuestCIDR is the guest address in "addr/bits" form, for AddrAdd.
func (n *Network) GuestCIDR() string {
	return fmt.Sprintf("%s/%d", n.Guest, n.Prefix.Bits())
}

// newNetwork creates a per-VM vmnet network and derives its addressing.
//
// The subnet is deliberately NOT requested: letting the framework allocate
// makes it the single arbiter across every VM on the host (ours and other
// products'), which is exactly the conflict-avoidance a hand-rolled
// allocator would have to reimplement badly.
func newNetwork() (*Network, error) {
	config, err := vmnet.NewNetworkConfiguration(vmnet.SharedMode)
	if err != nil {
		return nil, fmt.Errorf("%w: vmnet configuration (macOS 26+ required): %w", ErrNetwork, err)
	}

	network, err := vmnet.NewNetwork(config)
	if err != nil {
		return nil, fmt.Errorf("%w: creating vmnet network: %w", ErrNetwork, err)
	}

	prefix, err := network.IPv4Subnet()
	if err != nil {
		return nil, fmt.Errorf("%w: reading allocated subnet: %w", ErrNetwork, err)
	}

	gateway, guest, err := addressing(prefix)
	if err != nil {
		return nil, err
	}

	return &Network{
		Gateway: gateway,
		Guest:   guest,
		Prefix:  prefix,
		network: network,
	}, nil
}

// addressing derives the gateway and guest addresses from the prefix the
// framework reported.
//
// vmnet_network_get_ipv4_subnet reports the address the NETWORK ITSELF holds
// — the bridge's, i.e. the gateway (observed: "192.168.64.1/24", not
// .0/24). So the gateway is taken from that value directly rather than
// assumed to be the first host address; only when the framework reports the
// network base (a form this API has not been seen to use, but which the type
// permits) is the first host address derived instead. The guest then takes
// the next address after the gateway.
func addressing(prefix netip.Prefix) (gateway, guest netip.Addr, err error) {
	if !prefix.Addr().Is4() {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("%w: subnet %s is not IPv4", ErrNetwork, prefix)
	}

	base := prefix.Masked().Addr()

	gateway = prefix.Addr()
	if gateway == base {
		gateway = base.Next()
	}

	guest = gateway.Next()
	if !prefix.Contains(guest) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf(
			"%w: subnet %s has no room for a guest address beside gateway %s", ErrNetwork, prefix, gateway,
		)
	}

	return gateway, guest, nil
}
