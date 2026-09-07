//go:build darwin

package vz

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Virtualization -framework vmnet
# include "virtualization_11.h"
# include "virtualization_13.h"
# include "virtualization_26.h"
*/
import "C"
import (
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/farcloser/ossein/third_party/vz/internal/objc"
	"github.com/farcloser/ossein/third_party/vz/vmnet"
)

// Network device attachment using network address translation (NAT) with outside networks.
//

// NewNATNetworkDeviceAttachment creates a new NATNetworkDeviceAttachment.
//

// BridgedNetworkDeviceAttachment represents a physical interface on the host computer.
//
// Use this struct when configuring a network interface for your virtual machine.
// A bridged network device sends and receives packets on the same physical interface
// as the host computer, but does so using a different network layer.
//
// To use this attachment, your app must have the com.apple.vm.networking entitlement.
// If it doesn’t, the use of this attachment point results in an invalid VZVirtualMachineConfiguration object in objective-c.
//

// NewBridgedNetworkDeviceAttachment creates a new BridgedNetworkDeviceAttachment with networkInterface.
//

// FileHandleNetworkDeviceAttachment sending raw network packets over a file handle.
//

// NewFileHandleNetworkDeviceAttachment initialize the attachment with a file handle.
//
// file parameter is holding a connected datagram socket.
//

func validateDatagramSocket(fd int) error {
	sotype, err := syscall.GetsockoptInt(
		fd,
		syscall.SOL_SOCKET,
		syscall.SO_TYPE,
	)
	if err != nil {
		return os.NewSyscallError("getsockopt", err)
	}
	if sotype == syscall.SOCK_DGRAM && isAvailableDatagram(fd) {
		return nil
	}
	return fmt.Errorf("the fileHandle must be a datagram socket")
}

func isAvailableDatagram(fd int) bool {
	lsa, _ := syscall.Getsockname(fd)
	switch lsa.(type) {
	case *syscall.SockaddrInet4, *syscall.SockaddrInet6, *syscall.SockaddrUnix:
		return true
	}
	return false
}

// SetMaximumTransmissionUnit sets the maximum transmission unit (MTU) associated with this attachment.
//
// The maximum MTU allowed is 65535, and the minimum MTU allowed is 1500. An invalid MTU value will result in an invalid
// virtual machine configuration.
//
// The client side of the associated datagram socket must be properly configured with the appropriate values
// for SO_SNDBUF, and SO_RCVBUF. Set these using the setsockopt(_:_:_:_:_:) system call. The system expects
// the value of SO_RCVBUF to be at least double the value of SO_SNDBUF, and for optimal performance, the
// recommended value of SO_RCVBUF is four times the value of SO_SNDBUF.
//

// VmnetNetworkDeviceAttachment represents a vmnet network device attachment.
//
// This attachment is used to connect a virtual machine to a vmnet network.
// The attachment is created with a VmnetNetwork and can be used with a VirtioNetworkDeviceConfiguration.
// see: https://developer.apple.com/documentation/virtualization/vzvmnetnetworkdeviceattachment?language=objc
//
// This is only supported on macOS 26 and newer, error will
// be returned on older versions.
type VmnetNetworkDeviceAttachment struct {
	*pointer

	*baseNetworkDeviceAttachment
}

func (*VmnetNetworkDeviceAttachment) String() string {
	return "VmnetNetworkDeviceAttachment"
}

func (v *VmnetNetworkDeviceAttachment) Network() *vmnet.Network {
	ptr := C.VZVmnetNetworkDeviceAttachment_network(objc.Ptr(v))
	return vmnet.NewNetworkFromPointer(objc.NewPointer(ptr))
}

var _ NetworkDeviceAttachment = (*VmnetNetworkDeviceAttachment)(nil)

// NewVmnetNetworkDeviceAttachment creates a new VmnetNetworkDeviceAttachment with network.
//
// This is only supported on macOS 26 and newer, error will
// be returned on older versions.
func NewVmnetNetworkDeviceAttachment(network *vmnet.Network) (*VmnetNetworkDeviceAttachment, error) {
	if err := macOSAvailable(26); err != nil {
		return nil, err
	}

	attachment := &VmnetNetworkDeviceAttachment{
		pointer: objc.NewPointer(
			C.newVZVmnetNetworkDeviceAttachment(objc.Ptr(network)),
		),
	}
	objc.SetFinalizer(attachment, func(self *VmnetNetworkDeviceAttachment) {
		objc.Release(self)
	})
	return attachment, nil
}

// NetworkDeviceAttachment for a network device attachment.
// see: https://developer.apple.com/documentation/virtualization/vznetworkdeviceattachment?language=objc
type NetworkDeviceAttachment interface {
	objc.NSObject
	fmt.Stringer
	networkDeviceAttachment()
}

type baseNetworkDeviceAttachment struct{}

func (*baseNetworkDeviceAttachment) networkDeviceAttachment() {}

// VirtioNetworkDeviceConfiguration is configuration of a paravirtualized network device of type Virtio Network Device.
//
// The communication channel used on the host is defined through the attachment.
// It is set with the VZNetworkDeviceConfiguration.attachment property in objective-c.
//
// The configuration is only valid with valid MACAddress and attachment.
//
// see: https://developer.apple.com/documentation/virtualization/vzvirtionetworkdeviceconfiguration?language=objc
type VirtioNetworkDeviceConfiguration struct {
	*pointer

	attachment NetworkDeviceAttachment
}

// NewVirtioNetworkDeviceConfiguration creates a new VirtioNetworkDeviceConfiguration with NetworkDeviceAttachment.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewVirtioNetworkDeviceConfiguration(attachment NetworkDeviceAttachment) (*VirtioNetworkDeviceConfiguration, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	config := newVirtioNetworkDeviceConfiguration(attachment)
	objc.SetFinalizer(config, func(self *VirtioNetworkDeviceConfiguration) {
		objc.Release(self)
	})
	return config, nil
}

func newVirtioNetworkDeviceConfiguration(attachment NetworkDeviceAttachment) *VirtioNetworkDeviceConfiguration {
	ptr := C.newVZVirtioNetworkDeviceConfiguration(
		objc.Ptr(attachment),
	)
	return &VirtioNetworkDeviceConfiguration{
		pointer:    objc.NewPointer(ptr),
		attachment: attachment,
	}
}

func (v *VirtioNetworkDeviceConfiguration) SetMACAddress(macAddress *MACAddress) {
	C.setNetworkDevicesVZMACAddress(objc.Ptr(v), objc.Ptr(macAddress))
}

func (v *VirtioNetworkDeviceConfiguration) Attachment() NetworkDeviceAttachment {
	return v.attachment
}

// MACAddress represents a media access control address (MAC address), the 48-bit ethernet address.
// see: https://developer.apple.com/documentation/virtualization/vzmacaddress?language=objc
type MACAddress struct {
	*pointer
}

// NewMACAddress creates a new MACAddress with net.HardwareAddr (MAC address).
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewMACAddress(macAddr net.HardwareAddr) (*MACAddress, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	macAddrChar := charWithGoString(macAddr.String())
	defer macAddrChar.Free()
	ma := &MACAddress{
		pointer: objc.NewPointer(
			C.newVZMACAddress(macAddrChar.CString()),
		),
	}
	objc.SetFinalizer(ma, func(self *MACAddress) {
		objc.Release(self)
	})
	return ma, nil
}

// NewRandomLocallyAdministeredMACAddress creates a valid, random, unicast, locally administered address.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewRandomLocallyAdministeredMACAddress() (*MACAddress, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	ma := &MACAddress{
		pointer: objc.NewPointer(
			C.newRandomLocallyAdministeredVZMACAddress(),
		),
	}
	objc.SetFinalizer(ma, func(self *MACAddress) {
		objc.Release(self)
	})
	return ma, nil
}

func (m *MACAddress) String() string {
	cstring := (*char)(C.getVZMACAddressString(objc.Ptr(m)))
	return cstring.String()
}

func (m *MACAddress) HardwareAddr() net.HardwareAddr {
	hw, _ := net.ParseMAC(m.String())
	return hw
}
