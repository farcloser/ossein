//
//  virtualization_15.m
//
//  Created by codehex.
//
#import "virtualization_15.h"

/*!
 @abstract Check if nested virtualization is supported.
 @return true if supported.
 */
bool isNestedVirtualizationSupported()
{
#ifdef INCLUDE_TARGET_OSX_15
    if (@available(macOS 15, *)) {
        return (bool)VZGenericPlatformConfiguration.isNestedVirtualizationSupported;
    }
#endif
    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}

/*!
 @abstract Set nestedVirtualizationEnabled. The default is false.
 */
void setNestedVirtualizationEnabled(void *config, bool nestedVirtualizationEnabled)
{
#ifdef INCLUDE_TARGET_OSX_15
    if (@available(macOS 15, *)) {
        VZGenericPlatformConfiguration *platformConfig = (VZGenericPlatformConfiguration *)config;
        platformConfig.nestedVirtualizationEnabled = (BOOL)nestedVirtualizationEnabled;
        return;
    }
#endif
    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}

/*!
 @abstract Configuration for the USB XHCI controller.
 @discussion This configuration creates a USB XHCI controller device for the guest.
 */
void *newVZXHCIControllerConfiguration()
{
#ifdef INCLUDE_TARGET_OSX_15
    if (@available(macOS 15, *)) {
        return [[VZXHCIControllerConfiguration alloc] init];
    }
#endif
    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}




/*!
 @abstract Return the list of USB controllers configured on this virtual machine. Return an empty array if no USB controller is configured.
 @see VZUSBControllerConfiguration
 @see VZVirtualMachineConfiguration
 */
void *VZVirtualMachine_usbControllers(void *machine)
{
#ifdef INCLUDE_TARGET_OSX_15
    if (@available(macOS 15, *)) {
        return [(VZVirtualMachine *)machine usbControllers]; // NSArray<VZUSBController *>
    }
#endif
    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}



/*!
 @abstract Initialize the runtime USB Mass Storage device object.
 @param configuration The configuration of the USB Mass Storage device.
 @see VZUSBMassStorageDeviceConfiguration
 */
void *newVZUSBMassStorageDeviceWithConfiguration(void *config)
{
#ifdef INCLUDE_TARGET_OSX_15
    if (@available(macOS 15, *)) {
        return [[VZUSBMassStorageDevice alloc] initWithConfiguration:(VZUSBMassStorageDeviceConfiguration *)config];
    }
#endif
    RAISE_UNSUPPORTED_MACOS_EXCEPTION();
}