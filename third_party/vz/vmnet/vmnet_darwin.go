package vmnet

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework vmnet
# include "vmnet_darwin.h"
*/
import "C"
import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"unsafe"

	"github.com/farcloser/ossein/third_party/vz/internal/objc"
	"github.com/farcloser/ossein/third_party/vz/internal/osversion"
	"golang.org/x/sys/unix"
)

var macOSAvailable = osversion.MacOSAvailable

// MARK: - Return

// The status code returning the result of vmnet operations.
//   - https://developer.apple.com/documentation/vmnet/vmnet_return_t?language=objc
type Return C.uint32_t

const (
	ErrSuccess            Return = C.VMNET_SUCCESS              // VMNET_SUCCESS Successfully completed.
	ErrFailure            Return = C.VMNET_FAILURE              // VMNET_FAILURE General failure.
	ErrMemFailure         Return = C.VMNET_MEM_FAILURE          // VMNET_MEM_FAILURE Memory allocation failure.
	ErrInvalidArgument    Return = C.VMNET_INVALID_ARGUMENT     // VMNET_INVALID_ARGUMENT Invalid argument specified.
	ErrSetupIncomplete    Return = C.VMNET_SETUP_INCOMPLETE     // VMNET_SETUP_INCOMPLETE Interface setup is not complete.
	ErrInvalidAccess      Return = C.VMNET_INVALID_ACCESS       // VMNET_INVALID_ACCESS Permission denied.
	ErrPacketTooBig       Return = C.VMNET_PACKET_TOO_BIG       // VMNET_PACKET_TOO_BIG Packet size larger than MTU.
	ErrBufferExhausted    Return = C.VMNET_BUFFER_EXHAUSTED     // VMNET_BUFFER_EXHAUSTED Buffers exhausted in kernel.
	ErrTooManyPackets     Return = C.VMNET_TOO_MANY_PACKETS     // VMNET_TOO_MANY_PACKETS Packet count exceeds limit.
	ErrSharingServiceBusy Return = C.VMNET_SHARING_SERVICE_BUSY // VMNET_SHARING_SERVICE_BUSY Vmnet Interface cannot be started as conflicting sharing service is in use.
	ErrNotAuthorized      Return = C.VMNET_NOT_AUTHORIZED       // VMNET_NOT_AUTHORIZED The operation could not be completed due to missing authorization.
)

var _ error = Return(0)

func (e Return) Error() string {
	switch e {
	case ErrSuccess:
		return "Vmnet: Successfully completed"
	case ErrFailure:
		return "Vmnet: Failure"
	case ErrMemFailure:
		return "Vmnet: Memory allocation failure"
	case ErrInvalidArgument:
		return "Vmnet: Invalid argument specified"
	case ErrSetupIncomplete:
		return "Vmnet: Interface setup is not complete"
	case ErrInvalidAccess:
		return "Vmnet: Permission denied"
	case ErrPacketTooBig:
		return "Vmnet: Packet size larger than MTU"
	case ErrBufferExhausted:
		return "Vmnet: Buffers exhausted in kernel"
	case ErrTooManyPackets:
		return "Vmnet: Packet count exceeds limit"
	case ErrSharingServiceBusy:
		return "Vmnet: Vmnet Interface cannot be started as conflicting sharing service is in use"
	case ErrNotAuthorized:
		return "Vmnet: The operation could not be completed due to missing authorization"
	default:
		return fmt.Sprintf("Vmnet: Unknown error %d", uint32(e))
	}
}

// MARK: - Mode

// Mode defines the mode of a [Network]. (See [operating_modes_t])
//   - [HostMode] and [SharedMode] are supported by [NewNetworkConfiguration].
//   - VMNET_BRIDGED_MODE is not supported by underlying API [vmnet_network_configuration_create].
//
// [operating_modes_t]: https://developer.apple.com/documentation/vmnet/operating_modes_t?language=objc
// [vmnet_network_configuration_create]: https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_create(_:_:)?language=objc
type Mode uint32

const (
	// https://developer.apple.com/documentation/vmnet/operating_modes_t/vmnet_host_mode?language=objc
	HostMode Mode = C.VMNET_HOST_MODE
	// https://developer.apple.com/documentation/vmnet/operating_modes_t/vmnet_shared_mode?language=objc
	SharedMode Mode = C.VMNET_SHARED_MODE
)

// MARK: - pointer

type pointer = objc.Pointer

// Retain calls retain method on the given object and returns it.
func Retain[O interface{ retain() }](o O) O {
	o.retain()
	return o
}

// ReleaseOnCleanup calls releaseOnCleanup method on the given object and returns it.
func ReleaseOnCleanup[O interface{ releaseOnCleanup() }](o O) O {
	o.releaseOnCleanup()
	return o
}

// MARK: - NetworkConfiguration

// NetworkConfiguration represents a [vmnet_network_configuration_ref].
//
// [vmnet_network_configuration_ref]: https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_ref?language=objc
type NetworkConfiguration struct {
	*pointer
}

// NewNetworkConfiguration creates a new [NetworkConfiguration] with [Mode].
// This is only supported on macOS 26 and newer, error will be returned on older versions.
// [BridgedMode] is not supported by this function.
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_create(_:_:)?language=objc
func NewNetworkConfiguration(mode Mode) (*NetworkConfiguration, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}
	var status Return
	ptr := C.VmnetNetworkConfigurationCreate(
		C.uint32_t(mode),
		(*C.uint32_t)(unsafe.Pointer(&status)),
	)
	if !errors.Is(status, ErrSuccess) {
		return nil, fmt.Errorf("failed to create VmnetNetworkConfiguration: %w", status)
	}
	config := &NetworkConfiguration{objc.NewPointer(ptr)}
	ReleaseOnCleanup(config)
	return config, nil
}

// releaseOnCleanup registers a cleanup function to release the object when cleaned up.
func (c *NetworkConfiguration) releaseOnCleanup() {
	runtime.AddCleanup(c, func(p unsafe.Pointer) {
		C.vmnetRelease(p)
	}, objc.Ptr(c))
}

// AddDhcpReservation configures a new DHCP reservation for the [Network].
// client is the MAC address for which the DHCP address is reserved.
// reservation is the DHCP IPv4 address to be reserved.
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_add_dhcp_reservation(_:_:_:)?language=objc
func (c *NetworkConfiguration) AddDhcpReservation(client net.HardwareAddr, reservation netip.Addr) error {
	if !reservation.Is4() {
		return fmt.Errorf("reservation is not ipv4")
	}
	ip := reservation.As4()
	var cReservation C.struct_in_addr

	cClient, err := netHardwareAddrToEtherAddr(client)
	if err != nil {
		return err
	}
	copy((*[4]byte)(unsafe.Pointer(&cReservation))[:], ip[:])

	status := C.VmnetNetworkConfiguration_addDhcpReservation(
		objc.Ptr(c),
		&cClient,
		&cReservation,
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to add dhcp reservation: %w", Return(status))
	}
	return nil
}

// AddPortForwardingRule configures a port forwarding rule for the [Network].
// These rules will not be able to be removed or queried until network has been started.
// To do that, use `vmnet_interface_remove_ip_forwarding_rule` or
// `vmnet_interface_get_ip_port_forwarding_rules` C API directly.
// (`vmnet_interface` related functionality not implemented in this package yet)
//
// protocol must be either IPPROTO_TCP or IPPROTO_UDP
// addressFamily must be either AF_INET or AF_INET6
// internalPort is the TCP or UDP port that forwarded traffic should be redirected to.
// externalPort is the TCP or UDP port on the outside network that should be redirected from.
// internalAddress is the IPv4 or IPv6 address of the machine on the internal network that should receive the forwarded traffic.
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_add_port_forwarding_rule(_:_:_:_:_:_:)?language=objc
func (c *NetworkConfiguration) AddPortForwardingRule(protocol uint8, addressFamily uint8, internalPort uint16, externalPort uint16, internalAddress netip.Addr) error {
	var address unsafe.Pointer
	switch addressFamily {
	case unix.AF_INET:
		if !internalAddress.Is4() {
			return fmt.Errorf("internal address is not ipv4")
		}
		var inAddr C.struct_in_addr
		ip := internalAddress.As4()
		copy((*[4]byte)(unsafe.Pointer(&inAddr))[:], ip[:])
		address = unsafe.Pointer(&inAddr)
	case unix.AF_INET6:
		if !internalAddress.Is6() {
			return fmt.Errorf("internal address is not ipv6")
		}
		var in6Addr C.struct_in6_addr
		ip := internalAddress.As16()
		copy((*[16]byte)(unsafe.Pointer(&in6Addr))[:], ip[:])
		address = unsafe.Pointer(&in6Addr)
	default:
		return fmt.Errorf("unsupported address family: %d", addressFamily)
	}
	status := C.VmnetNetworkConfiguration_addPortForwardingRule(
		objc.Ptr(c),
		C.uint8_t(protocol),
		C.sa_family_t(addressFamily),
		C.uint16_t(internalPort),
		C.uint16_t(externalPort),
		address,
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to add port forwarding rule: %w", Return(status))
	}
	return nil
}

// DisableDhcp disables DHCP server on the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_disable_dhcp(_:)?language=objc
func (c *NetworkConfiguration) DisableDhcp() {
	C.VmnetNetworkConfiguration_disableDhcp(objc.Ptr(c))
}

// DisableDnsProxy disables DNS proxy on the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_disable_dns_proxy(_:)?language=objc
func (c *NetworkConfiguration) DisableDnsProxy() {
	C.VmnetNetworkConfiguration_disableDnsProxy(objc.Ptr(c))
}

// DisableNat44 disables NAT44 on the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_disable_nat44(_:)?language=objc
func (c *NetworkConfiguration) DisableNat44() {
	C.VmnetNetworkConfiguration_disableNat44(objc.Ptr(c))
}

// DisableNat66 disables NAT66 on the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_disable_nat66(_:)?language=objc
func (c *NetworkConfiguration) DisableNat66() {
	C.VmnetNetworkConfiguration_disableNat66(objc.Ptr(c))
}

// DisableRouterAdvertisement disables router advertisement on the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_disable_router_advertisement(_:)?language=objc
func (c *NetworkConfiguration) DisableRouterAdvertisement() {
	C.VmnetNetworkConfiguration_disableRouterAdvertisement(objc.Ptr(c))
}

// SetExternalInterface sets the external interface of the [Network].
// This is only available to networks of [SharedMode].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_set_external_interface(_:_:)?language=objc
func (c *NetworkConfiguration) SetExternalInterface(ifname string) error {
	cIfname := C.CString(ifname)
	defer C.free(unsafe.Pointer(cIfname))

	status := C.VmnetNetworkConfiguration_setExternalInterface(
		objc.Ptr(c),
		cIfname,
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to set external interface: %w", Return(status))
	}
	return nil
}

// SetIPv4Subnet configures the IPv4 address for the [Network].
// Note that the first, second, and last addresses of the range are reserved.
// The second address is reserved for the host, the first and last are not assignable to any node.
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_set_ipv4_subnet(_:_:_:)?language=objc
func (c *NetworkConfiguration) SetIPv4Subnet(subnet netip.Prefix) error {
	if !subnet.Addr().Is4() {
		return fmt.Errorf("subnet is not ipv4")
	}
	if !netip.MustParsePrefix("192.168.0.0/16").Overlaps(subnet) {
		return fmt.Errorf("subnet %s is out of range", subnet.String())
	}
	// Use the first assignable address as the subnet address to avoid
	// Virtualization fails with error "Internal Virtualization error. Internal Network Error.".
	ip := subnet.Masked().Addr().Next().As4()
	mask := net.CIDRMask(subnet.Bits(), 32)
	var cSubnet C.struct_in_addr
	var cMask C.struct_in_addr

	copy((*[4]byte)(unsafe.Pointer(&cSubnet))[:], ip[:])
	copy((*[4]byte)(unsafe.Pointer(&cMask))[:], mask[:])

	status := C.VmnetNetworkConfiguration_setIPv4Subnet(
		objc.Ptr(c),
		&cSubnet,
		&cMask,
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to set ipv4 subnet: %d", Return(status))
	}
	return nil
}

// SetIPv6Prefix configures the IPv6 prefix for the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_set_ipv6_prefix(_:_:_:)?language=objc
func (c *NetworkConfiguration) SetIPv6Prefix(prefix netip.Prefix) error {
	if !prefix.Addr().Is6() {
		return fmt.Errorf("prefix is not ipv6")
	}
	ip := prefix.Addr().As16()
	var cPrefix C.struct_in6_addr

	copy((*[16]byte)(unsafe.Pointer(&cPrefix))[:], ip[:])

	status := C.VmnetNetworkConfiguration_setIPv6Prefix(
		objc.Ptr(c),
		&cPrefix,
		C.uint8_t(prefix.Bits()),
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to set ipv6 prefix: %w", Return(status))
	}
	return nil
}

func netHardwareAddrToEtherAddr(hw net.HardwareAddr) (C.ether_addr_t, error) {
	if len(hw) != 6 {
		return C.ether_addr_t{}, fmt.Errorf("invalid MAC address length: %d", len(hw))
	}
	var addr C.ether_addr_t
	copy((*[6]byte)(unsafe.Pointer(&addr))[:], hw[:6])
	return addr, nil
}

// SetMtu configures the maximum transmission unit (MTU) for the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_set_mtu(_:_:)?language=objc
func (c *NetworkConfiguration) SetMtu(mtu uint32) error {
	status := C.VmnetNetworkConfiguration_setMtu(
		objc.Ptr(c),
		C.uint32_t(mtu),
	)
	if !errors.Is(Return(status), ErrSuccess) {
		return fmt.Errorf("failed to set mtu: %w", Return(status))
	}
	return nil
}

// MARK: - Network

// Network represents a [vmnet_network_ref].
//
// [vmnet_network_ref]: https://developer.apple.com/documentation/vmnet/vmnet_network_ref?language=objc
type Network struct {
	*pointer
}

// NewNetwork creates a new [Network] with [NetworkConfiguration].
// This is only supported on macOS 26 and newer, error will be returned on older versions.
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_create(_:_:)?language=objc
func NewNetwork(config *NetworkConfiguration) (*Network, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}

	var status Return
	ptr := C.VmnetNetworkCreate(
		objc.Ptr(config),
		(*C.uint32_t)(unsafe.Pointer(&status)),
	)
	if !errors.Is(status, ErrSuccess) {
		return nil, fmt.Errorf("failed to create VmnetNetwork: %w", status)
	}
	network := &Network{objc.NewPointer(ptr)}
	ReleaseOnCleanup(network)
	return network, nil
}

// releaseOnCleanup registers a cleanup function to release the object when cleaned up.
func (n *Network) releaseOnCleanup() {
	runtime.AddCleanup(n, func(p unsafe.Pointer) {
		C.vmnetRelease(p)
	}, objc.Ptr(n))
}


// NewNetworkFromPointer creates a new [Network] from an existing [objc.Pointer].
func NewNetworkFromPointer(p *objc.Pointer) *Network {
	return &Network{p}
}


// IPv4Subnet returns the IPv4 subnet of the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_get_ipv4_subnet(_:_:_:)?language=objc
func (n *Network) IPv4Subnet() (subnet netip.Prefix, err error) {
	// OSSEIN FORK: guard like every other entry point in this file. Reaching
	// the C call on an older OS raises an Objective-C exception instead of
	// returning an error, and a Go-side check is what keeps that impossible.
	if err := macOSAvailable(26); err != nil {
		return netip.Prefix{}, err
	}

	var cSubnet C.struct_in_addr
	var cMask C.struct_in_addr

	C.VmnetNetwork_getIPv4Subnet(objc.Ptr(n), &cSubnet, &cMask)

	sIP := inAddrToNetipAddr(cSubnet)
	mIP := inAddrToIP(cMask)

	// netmask → prefix length
	ones, bits := net.IPMask(mIP.To4()).Size()
	if bits != 32 {
		return netip.Prefix{}, fmt.Errorf("unexpected mask size")
	}

	return netip.PrefixFrom(sIP, ones), nil
}

func inAddrToNetipAddr(a C.struct_in_addr) netip.Addr {
	p := (*[4]byte)(unsafe.Pointer(&a))
	return netip.AddrFrom4(*p)
}

func inAddrToIP(a C.struct_in_addr) net.IP {
	p := (*[4]byte)(unsafe.Pointer(&a))
	return net.IPv4(p[0], p[1], p[2], p[3])
}

// IPv6Prefix returns the IPv6 prefix of the [Network].
//   - https://developer.apple.com/documentation/vmnet/vmnet_network_get_ipv6_prefix(_:_:_:)?language=objc
func (n *Network) IPv6Prefix() (netip.Prefix, error) {
	// OSSEIN FORK: same guard as IPv4Subnet.
	if err := macOSAvailable(26); err != nil {
		return netip.Prefix{}, err
	}

	var prefix C.struct_in6_addr
	var prefixLen C.uint8_t

	C.VmnetNetwork_getIPv6Prefix(objc.Ptr(n), &prefix, &prefixLen)

	addr := in6AddrToNetipAddr(prefix)
	pfx := netip.PrefixFrom(addr, int(prefixLen))

	if !pfx.IsValid() {
		return netip.Prefix{}, fmt.Errorf("invalid ipv6 prefix")
	}
	return pfx, nil
}

func in6AddrToNetipAddr(a C.struct_in6_addr) netip.Addr {
	p := (*[16]byte)(unsafe.Pointer(&a))
	return netip.AddrFrom16(*p)
}
