//go:build linux

package guestagent

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
)

// WireErrorText is the server interceptor that lets a handler's error text
// reach the host. connect serializes only the message of a *connect.Error it
// was handed; any other error crosses as a bare code. The host is the one
// caller and ossein's CLI shows the message, so a plain error (an errno, a
// wrapped cause) is given its text here, and a context error its code, which
// is what v1 did on its own.
func WireErrorText(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err == nil {
			return nil
		}

		if _, coded := errors.AsType[*connect.Error](err); coded {
			return err
		}

		code := connect.CodeUnknown

		switch {
		case errors.Is(err, context.Canceled):
			code = connect.CodeCanceled
		case errors.Is(err, context.DeadlineExceeded):
			code = connect.CodeDeadlineExceeded
		}

		return connect.NewError(code, err.Error()).WithCause(err)
	}
}
