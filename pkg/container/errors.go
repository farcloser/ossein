//go:build darwin && arm64

package container

import "errors"

// ErrNoCommand indicates the image carries no entrypoint/cmd and the caller
// supplied no command to run.
var ErrNoCommand = errors.New("no command to run")

// ErrNoNetwork indicates network configuration was requested for a VM that
// was built without a network device.
var ErrNoNetwork = errors.New("vm has no network device")

// ErrSocketInUse indicates the host socket path for ExposeUnix is still served
// by a live listener — exposing over it would hijack another instance.
var ErrSocketInUse = errors.New("host socket in use by another process")
