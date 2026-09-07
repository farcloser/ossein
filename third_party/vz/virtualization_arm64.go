//go:build darwin && arm64

package vz

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Virtualization
# include "virtualization_11.h"
# include "virtualization_13_arm64.h"
# include "virtualization_14_arm64.h"
*/
import "C"
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/farcloser/ossein/third_party/vz/internal/progress"
)

// WithStartUpFromMacOSRecovery is an option to specifiy whether to start up
// from macOS Recovery for macOS VM.
//
// This is only supported on macOS 13 and newer, error will
// be returned on older versions.
func WithStartUpFromMacOSRecovery(startInRecovery bool) VirtualMachineStartOption {
	return func(vmso *virtualMachineStartOptions) error {
		if err := macOSAvailable(13); err != nil {
			return err
		}
		vmso.macOSVirtualMachineStartOptionsPtr = C.newVZMacOSVirtualMachineStartOptions(
			C.bool(startInRecovery),
		)
		return nil
	}
}

// NewMacHardwareModelWithDataPath initialize a new hardware model described by the specified pathname.
//

// NewMacHardwareModelWithData initialize a new hardware model described by the specified data representation.
//

// NewMacMachineIdentifierWithDataPath initialize a new machine identifier described by the specified pathname.
//

// NewMacMachineIdentifierWithData initialize a new machine identifier described by the specified data representation.
//

// NewMacMachineIdentifier initialize a new Mac machine identifier is used by macOS guests to uniquely
// identify the virtual hardware.
//
// Two virtual machines running concurrently should not use the same identifier.
//
// If the virtual machine is serialized to disk, the identifier can be preserved in a binary representation through
// DataRepresentation method.
// The identifier can then be recreated with NewMacMachineIdentifierWithData function from the binary representation.
//

// NewMacAuxiliaryStorage creates a new MacAuxiliaryStorage is based Mac auxiliary storage data from the storagePath
// of an existing file by default.
//

// downloadRestoreImage resumable downloads macOS restore image (ipsw) file.
func downloadRestoreImage(ctx context.Context, url string, destPath string) (*progress.Reader, error) {
	// open or create
	f, err := os.OpenFile(destPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		f.Close()
		return nil, err
	}

	req.Header.Add("User-Agent", "github.com/Code-Hex/vz")
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-", fileInfo.Size()))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.Close()
		return nil, err
	}

	if 200 > resp.StatusCode || resp.StatusCode >= 300 {
		f.Close()
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected http status code: %d", resp.StatusCode)
	}

	reader := progress.NewReader(resp.Body, resp.ContentLength, fileInfo.Size())

	go func() {
		defer f.Close()
		defer resp.Body.Close()
		_, err := io.Copy(f, reader)
		reader.Finish(err)
	}()

	return reader, nil
}
