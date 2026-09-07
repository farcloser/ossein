//go:build linux

package vmexec

import "errors"

// Static sentinel errors for the stage2 runtime. Callers wrap them with the
// offending value (fmt.Errorf("%w: %q", ...)) so the agent's error pipe still
// carries full context.
var (
	errInvalidSpec       = errors.New("spec missing process or root")
	errInvalidSysctlKey  = errors.New("invalid sysctl key")
	errUnknownRlimit     = errors.New("unknown rlimit")
	errUnknownCapability = errors.New("unknown capability")
	errEmptyArgs         = errors.New("empty process args")
	errExecNotFound      = errors.New("executable not found")
)
