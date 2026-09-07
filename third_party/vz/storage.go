//go:build darwin

package vz

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Virtualization
# include "virtualization_11.h"
# include "virtualization_12.h"
# include "virtualization_12_3.h"
# include "virtualization_13.h"
# include "virtualization_14.h"
*/
import "C"
import (
	"os"
	"unsafe"

	"github.com/farcloser/ossein/third_party/vz/internal/objc"
)

type baseStorageDeviceAttachment struct{}

func (*baseStorageDeviceAttachment) storageDeviceAttachment() {}

// StorageDeviceAttachment for a storage device attachment.
//
// A storage device attachment defines how a virtual machine storage device interfaces with the host system.
// see: https://developer.apple.com/documentation/virtualization/vzstoragedeviceattachment?language=objc
type StorageDeviceAttachment interface {
	objc.NSObject

	storageDeviceAttachment()
}

var _ StorageDeviceAttachment = (*DiskImageStorageDeviceAttachment)(nil)

// DiskImageStorageDeviceAttachment is a storage device attachment using a disk image to implement the storage.
//
// This storage device attachment uses a disk image on the host file system as the drive of the storage device.
// Only raw data disk images are supported.
// see: https://developer.apple.com/documentation/virtualization/vzdiskimagestoragedeviceattachment?language=objc
type DiskImageStorageDeviceAttachment struct {
	*pointer

	*baseStorageDeviceAttachment
}

// DiskImageCachingMode describes the disk image caching mode.
//
// see: https://developer.apple.com/documentation/virtualization/vzdiskimagecachingmode?language=objc
type DiskImageCachingMode int

const (
	DiskImageCachingModeAutomatic DiskImageCachingMode = iota
	DiskImageCachingModeUncached
	DiskImageCachingModeCached
)

// DiskImageSynchronizationMode describes the disk image synchronization mode.
//
// see: https://developer.apple.com/documentation/virtualization/vzdiskimagesynchronizationmode?language=objc
type DiskImageSynchronizationMode int

const (
	DiskImageSynchronizationModeFull DiskImageSynchronizationMode = 1 + iota
	DiskImageSynchronizationModeFsync
	DiskImageSynchronizationModeNone
)

// NewDiskImageStorageDeviceAttachment initialize the attachment from a local file path.
// Returns error is not nil, assigned with the error if the initialization failed.
//
// - diskPath is local file URL to the disk image in RAW format.
// - readOnly if YES, the device attachment is read-only, otherwise the device can write data to the disk image.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewDiskImageStorageDeviceAttachment(diskPath string, readOnly bool) (*DiskImageStorageDeviceAttachment, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}
	if _, err := os.Stat(diskPath); err != nil {
		return nil, err
	}

	nserrPtr := newNSErrorAsNil()

	diskPathChar := charWithGoString(diskPath)
	defer diskPathChar.Free()
	attachment := &DiskImageStorageDeviceAttachment{
		pointer: objc.NewPointer(
			C.newVZDiskImageStorageDeviceAttachment(
				diskPathChar.CString(),
				C.bool(readOnly),
				&nserrPtr,
			),
		),
	}
	if err := newNSError(nserrPtr); err != nil {
		return nil, err
	}
	objc.SetFinalizer(attachment, func(self *DiskImageStorageDeviceAttachment) {
		objc.Release(self)
	})
	return attachment, nil
}

// NewDiskImageStorageDeviceAttachmentWithCacheAndSync initialize the attachment from a local file path.
// Returns error is not nil, assigned with the error if the initialization failed.
//
// - diskPath is local file URL to the disk image in RAW format.
// - readOnly if YES, the device attachment is read-only, otherwise the device can write data to the disk image.
// - cachingMode is one of the available DiskImageCachingMode options.
// - syncMode is to define how the disk image synchronizes with the underlying storage when the guest operating system flushes data, described by one of the available DiskImageSynchronizationMode modes.
//
// This is only supported on macOS 12 and newer, error will
// be returned on older versions.
func NewDiskImageStorageDeviceAttachmentWithCacheAndSync(diskPath string, readOnly bool, cachingMode DiskImageCachingMode, syncMode DiskImageSynchronizationMode) (*DiskImageStorageDeviceAttachment, error) {
	if err := macOSAvailable(12); err != nil {
		return nil, err
	}
	if _, err := os.Stat(diskPath); err != nil {
		return nil, err
	}

	nserrPtr := newNSErrorAsNil()

	diskPathChar := charWithGoString(diskPath)
	defer diskPathChar.Free()
	attachment := &DiskImageStorageDeviceAttachment{
		pointer: objc.NewPointer(
			C.newVZDiskImageStorageDeviceAttachmentWithCacheAndSyncMode(
				diskPathChar.CString(),
				C.bool(readOnly),
				C.int(cachingMode),
				C.int(syncMode),
				&nserrPtr,
			),
		),
	}
	if err := newNSError(nserrPtr); err != nil {
		return nil, err
	}
	objc.SetFinalizer(attachment, func(self *DiskImageStorageDeviceAttachment) {
		objc.Release(self)
	})
	return attachment, nil
}

// StorageDeviceConfiguration for a storage device configuration.
type StorageDeviceConfiguration interface {
	objc.NSObject

	storageDeviceConfiguration()
	Attachment() StorageDeviceAttachment
}

type baseStorageDeviceConfiguration struct {
	attachment StorageDeviceAttachment
}

func (*baseStorageDeviceConfiguration) storageDeviceConfiguration() {}
func (b *baseStorageDeviceConfiguration) Attachment() StorageDeviceAttachment {
	return b.attachment
}

var _ StorageDeviceConfiguration = (*VirtioBlockDeviceConfiguration)(nil)

// VirtioBlockDeviceConfiguration is a configuration of a paravirtualized storage device of type Virtio Block Device.
//
// This device configuration creates a storage device using paravirtualization.
// The emulated device follows the Virtio Block Device specification.
//
// The host implementation of the device is done through an attachment subclassing VZStorageDeviceAttachment
// like VZDiskImageStorageDeviceAttachment.
// see: https://developer.apple.com/documentation/virtualization/vzvirtioblockdeviceconfiguration?language=objc
type VirtioBlockDeviceConfiguration struct {
	*pointer

	*baseStorageDeviceConfiguration

	blockDeviceIdentifier string
}

// NewVirtioBlockDeviceConfiguration initialize a VZVirtioBlockDeviceConfiguration with a device attachment.
//
// - attachment The storage device attachment. This defines how the virtualized device operates on the host side.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewVirtioBlockDeviceConfiguration(attachment StorageDeviceAttachment) (*VirtioBlockDeviceConfiguration, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	config := &VirtioBlockDeviceConfiguration{
		pointer: objc.NewPointer(
			C.newVZVirtioBlockDeviceConfiguration(
				objc.Ptr(attachment),
			),
		),
		baseStorageDeviceConfiguration: &baseStorageDeviceConfiguration{
			attachment: attachment,
		},
	}
	objc.SetFinalizer(config, func(self *VirtioBlockDeviceConfiguration) {
		objc.Release(self)
	})
	return config, nil
}

// BlockDeviceIdentifier returns the device identifier is a string identifying the Virtio block device.
// Empty string by default.
//
// The identifier can be retrieved in the guest via a VIRTIO_BLK_T_GET_ID request.
//
// This is only supported on macOS 12.3 and newer, error will be returned on older versions.
//
// see: https://developer.apple.com/documentation/virtualization/vzvirtioblockdeviceconfiguration/3917717-blockdeviceidentifier
func (v *VirtioBlockDeviceConfiguration) BlockDeviceIdentifier() (string, error) {
	if err := macOSAvailable(12.3); err != nil {
		return "", err
	}
	return v.blockDeviceIdentifier, nil
}

// SetBlockDeviceIdentifier sets the device identifier is a string identifying the Virtio block device.
//
// The device identifier must be at most 20 bytes in length and ASCII-encodable.
//
// This is only supported on macOS 12.3 and newer, error will be returned on older versions.
//
// see: https://developer.apple.com/documentation/virtualization/vzvirtioblockdeviceconfiguration/3917717-blockdeviceidentifier
func (v *VirtioBlockDeviceConfiguration) SetBlockDeviceIdentifier(identifier string) error {
	if err := macOSAvailable(12.3); err != nil {
		return err
	}
	idChar := charWithGoString(identifier)
	defer idChar.Free()

	nserrPtr := newNSErrorAsNil()
	C.setBlockDeviceIdentifierVZVirtioBlockDeviceConfiguration(
		objc.Ptr(v),
		idChar.CString(),
		&nserrPtr,
	)
	if err := newNSError(nserrPtr); err != nil {
		return err
	}
	v.blockDeviceIdentifier = identifier
	return nil
}

// USBMassStorageDeviceConfiguration is a configuration of a USB Mass Storage storage device.
//
// This device configuration creates a storage device that conforms to the USB Mass Storage specification.
//

// NewUSBMassStorageDeviceConfiguration initialize a USBMassStorageDeviceConfiguration
// with a device attachment.
//

// NVMExpressControllerDeviceConfiguration is a configuration of an NVM Express Controller storage device.
//

// NewNVMExpressControllerDeviceConfiguration creates a new NVMExpressControllerDeviceConfiguration with
// a device attachment.
//
// attachment is the storage device attachment. This defines how the virtualized device operates on the
// host side.
//

// DiskSynchronizationMode describes the synchronization modes available to the guest OS.
//
// see: https://developer.apple.com/documentation/virtualization/vzdisksynchronizationmode?language=objc
type DiskSynchronizationMode int

const (
	// Perform all synchronization operations as requested by the guest OS.
	//
	// Using this mode, flush and barrier commands from the guest result in
	// the system sending their counterpart synchronization commands to the
	// underlying disk implementation.
	DiskSynchronizationModeFull DiskSynchronizationMode = iota

	// DiskSynchronizationModeNone don’t synchronize the data with the permanent storage.
	//
	// This option doesn’t guarantee data integrity if any error condition occurs such as
	// disk full on the host, panic, power loss, and so on.
	//
	// This mode is useful when a VM is only run once to perform a task to completion or
	// failure. In case of failure, the state of blocks on disk and their order isn’t defined.
	//
	// Using this mode may result in improved performance since no synchronization with the underlying
	// storage is necessary.
	DiskSynchronizationModeNone
)

// DiskBlockDeviceStorageDeviceAttachment is a storage device attachment that uses a disk to store data.
//
// The disk block device implements a storage attachment by using an actual disk rather than a disk image
// on a file system.
//
// Warning: Handle the disk passed to this attachment with caution. If the disk has a file system formatted
// on it, the guest can destroy data in a way that isn’t recoverable.
//
// By default, only the root user can access the disk file handle. Running virtual machines as root isn’t
// recommended. The best practice is to open the file in a separate process that has root privileges, then
// pass the open file descriptor using XPC or a Unix socket to a non-root process running Virtualization.
type DiskBlockDeviceStorageDeviceAttachment struct {
	*pointer

	*baseStorageDeviceAttachment
}

var _ StorageDeviceAttachment = (*DiskBlockDeviceStorageDeviceAttachment)(nil)

// NewDiskBlockDeviceStorageDeviceAttachment creates a new block storage device attachment from a file handle and with the
// specified access mode, synchronization mode, and error object that you provide.
//
// - file is the *os.File to a block device to attach to this VM.
// - readOnly is a boolean value that indicates whether this disk attachment is read-only; otherwise, if the file handle
// allows writes, the device can write data into it. this parameter affects how the Virtualization framework exposes the
// disk to the guest operating system by the storage controller. If you intend to use the disk in read-only mode, it’s
// also a best practice to open the file handle as read-only.
// - syncMode is one of the available DiskSynchronizationMode options.
//
// Note that the disk attachment retains the file handle, and the handle must be open when the virtual machine starts.
//
// This is only supported on macOS 14 and newer, error will
// be returned on older versions.
func NewDiskBlockDeviceStorageDeviceAttachment(file *os.File, readOnly bool, syncMode DiskSynchronizationMode) (*DiskBlockDeviceStorageDeviceAttachment, error) {
	if err := macOSAvailable(14); err != nil {
		return nil, err
	}

	nserrPtr := newNSErrorAsNil()

	attachment := &DiskBlockDeviceStorageDeviceAttachment{
		pointer: objc.NewPointer(
			C.newVZDiskBlockDeviceStorageDeviceAttachment(
				C.int(file.Fd()),
				C.bool(readOnly),
				C.int(syncMode),
				&nserrPtr,
			),
		),
	}
	if err := newNSError(nserrPtr); err != nil {
		return nil, err
	}
	objc.SetFinalizer(attachment, func(self *DiskBlockDeviceStorageDeviceAttachment) {
		objc.Release(self)
	})
	return attachment, nil
}

// OSSEIN FORK: the NBD (Network Block Device) storage attachment is removed.
//
// It was reachable from nothing — not from ossein, which attaches only local
// virtio-blk disks, and not from anywhere inside this package. Its two delegate
// callbacks survive below as discards because the Objective-C side still
// declares and calls them (virtualization_14.m:111,116); deleting those is the
// kind of mechanical ObjC prune FORK.md records going wrong. With no Go caller
// creating the attachment, the delegate is never instantiated, so in practice
// they never fire.
//
// What went with it: NetworkBlockDeviceStorageDeviceAttachment,
// NewNetworkBlockDeviceStorageDeviceAttachment, Connected, DidEncounterError,
// and the unbounded channel pair behind the last two — the reason this fork
// depended on github.com/Code-Hex/go-infinity-channel at all. Take the whole
// block back from upstream if ossein ever grows a use for NBD.

// attachmentDidEncounterErrorHandler function is called when the NBD client encounters a nonrecoverable error.
// After the attachment object calls this method, the NBD client is in a nonfunctional state.
//
//export attachmentDidEncounterErrorHandler
func attachmentDidEncounterErrorHandler(cgoHandleUintptr C.uintptr_t, errorPtr unsafe.Pointer) {
	// Consume the NSError bridging as before, then drop it: with the Go-side
	// attachment gone there is no delegate to receive this.
	_ = newNSError(errorPtr)
}

// attachmentWasConnectedHandler function is called when a connection to the server is first established as the VM starts,
// and during any reconnection attempts triggered by connection timeouts or recoverable errors encountered by the NBD client,
// such as server-side I/O errors.
//
// Note that the Virtualization framework may invoke this method multiple times throughout the VM’s lifecycle,
// ensuring reconnection processes remain seamless and transparent to the guest.
// For more details, see: https://developer.apple.com/documentation/virtualization/vznetworkblockdevicestoragedeviceattachmentdelegate/4168511-attachmentwasconnected?language=objc
//
//export attachmentWasConnectedHandler
func attachmentWasConnectedHandler(cgoHandleUintptr C.uintptr_t) {}
