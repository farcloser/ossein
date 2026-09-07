//go:build darwin && arm64

package vm

import "errors"

var (
	// ErrRosetta indicates Rosetta is unavailable on the host, or its
	// auto-install failed.
	ErrRosetta = errors.New("rosetta unavailable")

	// ErrLifecycle indicates the VM failed to reach or left an expected state
	// (e.g. did not start, entered the error state).
	ErrLifecycle = errors.New("vm lifecycle failure")

	// ErrVsock indicates a failure on the host<->guest vsock control channel.
	ErrVsock = errors.New("vsock failure")

	// ErrNetwork indicates the per-VM vmnet network could not be created or
	// interrogated (see network.go).
	ErrNetwork = errors.New("vm network failure")
)
