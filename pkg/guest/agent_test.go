//nolint:testpackage // exercises the unexported wrap and Dial internals directly
package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/farcloser/ossein/internal/protocol"
	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

// fakeGuest is a scriptable in-process guest agent.
//
// It mounts only the two procedures these tests exercise, as raw Connect
// handlers on their generated procedure paths, rather than implementing the
// whole 30-method SandboxContextHandler interface. That keeps the fake honest
// about what it stands in for: anything else the client calls 404s loudly
// instead of silently returning a zero value.
type fakeGuest struct {
	protoRev string
	copy     func(stream *connect.ServerStream[pb.CopyResponse]) error
}

// dialFake serves fake over a loopback httptest server and returns a connected
// Agent. The transport is the real one — Dial's own http.Transport, Connect
// codec and HTTP/1.1 framing — with only the dialer swapped, so these tests
// cover the actual wire path rather than a mock of it. Dial ignores the address
// (production hands back a vsock conn), which is exactly why pointing the
// dialer at a TCP listener works unchanged.
func dialFake(t *testing.T, fake *fakeGuest) (*Agent, error) {
	t.Helper()

	mux := http.NewServeMux()

	mux.Handle(sandboxconnect.SandboxContextGetenvProcedure, connect.NewUnaryHandler(
		sandboxconnect.SandboxContextGetenvProcedure,
		func(_ context.Context, req *connect.Request[pb.GetenvRequest],
		) (*connect.Response[pb.GetenvResponse], error) {
			// The key is spelled out rather than taken from protocol.RevEnvVar on
			// purpose: this is the wire name a real guest must answer to, so an
			// accidental rename of the constant fails a test instead of silently
			// changing the protocol.
			if req.Msg.GetKey() == "OSSEIN_PROTO_REV" {
				return connect.NewResponse(&pb.GetenvResponse{Value: &fake.protoRev}), nil
			}

			return connect.NewResponse(&pb.GetenvResponse{}), nil
		},
	))

	mux.Handle(sandboxconnect.SandboxContextCopyProcedure, connect.NewServerStreamHandler(
		sandboxconnect.SandboxContextCopyProcedure,
		func(_ context.Context, _ *connect.Request[pb.CopyRequest],
			stream *connect.ServerStream[pb.CopyResponse],
		) error {
			return fake.copy(stream)
		},
	))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	addr := server.Listener.Addr().String()

	return Dial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, "tcp", addr)
	})
}

func TestDialHandshake(t *testing.T) {
	t.Parallel()

	t.Run("matching rev succeeds", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{protoRev: protocol.Revision})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()
	})

	t.Run("missing rev is refused", func(t *testing.T) {
		t.Parallel()

		_, err := dialFake(t, &fakeGuest{protoRev: ""})
		if err == nil {
			t.Fatal("Dial accepted a guest with no OSSEIN_PROTO_REV")
		}

		if !errors.Is(err, ErrAgent) {
			t.Fatalf("mismatch error should wrap ErrAgent, got: %v", err)
		}
	})

	t.Run("wrong rev is refused", func(t *testing.T) {
		t.Parallel()

		_, err := dialFake(t, &fakeGuest{protoRev: "0"})
		if err == nil {
			t.Fatal("Dial accepted a guest speaking a different proto rev")
		}
	})
}

func TestCopyInArchive(t *testing.T) {
	t.Parallel()

	t.Run("complete means success", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{
			protoRev: protocol.Revision,
			copy: func(stream *connect.ServerStream[pb.CopyResponse]) error {
				return stream.Send(&pb.CopyResponse{Status: pb.CopyResponse_COMPLETE})
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		if err := agent.CopyInArchive(context.Background(), "/rootfs", "/dev/vdb"); err != nil {
			t.Fatalf("CopyInArchive after COMPLETE: %v", err)
		}
	})

	t.Run("in-band guest error fails", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{
			protoRev: protocol.Revision,
			copy: func(stream *connect.ServerStream[pb.CopyResponse]) error {
				return stream.Send(&pb.CopyResponse{Error: "short archive"})
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		err = agent.CopyInArchive(context.Background(), "/rootfs", "/dev/vdb")
		if err == nil || !errors.Is(err, ErrAgent) {
			t.Fatalf("in-band error must surface wrapped in ErrAgent, got: %v", err)
		}
	})

	t.Run("clean end before COMPLETE fails", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{
			protoRev: protocol.Revision,
			copy: func(*connect.ServerStream[pb.CopyResponse]) error {
				return nil // stream closes cleanly without ever sending COMPLETE
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		err = agent.CopyInArchive(context.Background(), "/rootfs", "/dev/vdb")
		if err == nil {
			t.Fatal("clean stream end without COMPLETE was treated as a successful rootfs copy")
		}
	})
}

func TestWrap(t *testing.T) {
	t.Parallel()

	if wrap(nil, "noop") != nil {
		t.Fatal("wrap(nil) must be nil")
	}

	underlying := connect.NewError(connect.CodeUnavailable, errors.New("gone"))

	err := wrap(underlying, "mount %s", "/x")
	if !errors.Is(err, ErrAgent) {
		t.Fatalf("wrapped error must match ErrAgent: %v", err)
	}

	// The transport code must survive our wrapping: a caller that wants to know
	// WHY a call failed has to be able to recover it through the %w chain.
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("wrapped error must preserve the Connect code, got %v", got)
	}

	if !errors.Is(err, underlying) {
		t.Fatal("wrapped error must still unwrap to the transport error")
	}
}
