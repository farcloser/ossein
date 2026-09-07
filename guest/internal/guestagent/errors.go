//go:build linux

package guestagent

import "errors"

// Static sentinel errors for the agent. Callers wrap them with the offending
// value (fmt.Errorf("%w: %q", ...)) so RPC error messages keep full context.
var (
	errPathEscapes          = errors.New("path escapes rootfs")
	errUnknownBlobFormat    = errors.New("rootfs blob format has no materialization path")
	errUnsupportedNamespace = errors.New("namespace is not supported by this runtime")
)

// usable configuration requires.
