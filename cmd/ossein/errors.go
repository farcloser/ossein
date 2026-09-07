//go:build darwin && arm64

package main

import (
	"errors"
	"fmt"
)

var (
	// errUsage indicates the command line was malformed (bad flags, arguments,
	// or a missing precondition like the kernel/initfs artifacts).
	errUsage = errors.New("invalid usage")

	// errBuildkit indicates the backgrounded buildkitd instance failed to
	// start or exited.
	errBuildkit = errors.New("buildkit failure")

	// errCacheBusy indicates a buildkit instance is already running for this
	// project's cache (its volume lock is held).
	errCacheBusy = errors.New("buildkit already running for this cache")

	// errSockBusy indicates a user-supplied --sock path is still served by a
	// live listener (a previous instance or an unrelated daemon).
	errSockBusy = errors.New("socket already in use")

	// errPidGone indicates the process being recorded in a pid file exited
	// before its identity (start time) could be captured.
	errPidGone = errors.New("process exited before pid file write")
)

// exitError carries a container's nonzero exit status through the normal error
// return path — deferred cleanup unwinds first, then main exits the process
// with the workload's own code (not an ossein failure, so nothing is logged).
type exitError struct{ code int }

func (err exitError) Error() string {
	return fmt.Sprintf("container exited with status %d", err.code)
}
