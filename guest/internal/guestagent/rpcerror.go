//go:build linux

package guestagent

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// rpcErrorf returns a coded RPC error, the Connect equivalent of grpc's
// status.Errorf (which is what this replaced). The code reaches the host as the response's status; ossein's
// own CLI only surfaces the message, but the code is what any other client —
// and any future retry logic — would branch on, so handlers keep stating it.
//
// The error is deliberately dynamic: these carry per-call context (the path
// that failed, the errno underneath), which a static sentinel cannot.
func rpcErrorf(code connect.Code, format string, args ...any) error {
	return connect.NewError(code, fmt.Errorf(format, args...)) //nolint:err113 // see above
}

// rpcContextError maps a cancelled or expired context onto the matching RPC
// code, so a client that gave up sees Canceled/DeadlineExceeded rather than a
// generic internal failure. Mirrors the grpc status.FromContextError this
// replaced.
func rpcContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	default:
		return connect.NewError(connect.CodeUnknown, err)
	}
}
