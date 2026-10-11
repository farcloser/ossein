//nolint:testpackage // exercises the unexported wrap and Dial internals directly
package guest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/farcloser/ossein/internal/protocol"
	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

// fakeGuest is a scriptable in-process guest agent.
//
// It implements only the two procedures these tests exercise and inherits the
// rest from the generated Unimplemented handler, so anything else the client
// calls fails loudly with CodeUnimplemented instead of silently returning a
// zero value.
type fakeGuest struct {
	sandboxconnect.UnimplementedSandboxContextHandler

	protoRev string
	copy     func(stream sandboxconnect.SandboxContextCopyServerStream) error
}

func (f *fakeGuest) Getenv(_ context.Context, req *pb.GetenvRequest) (*pb.GetenvResponse, error) {
	// The key is spelled out rather than taken from protocol.RevEnvVar on
	// purpose: this is the wire name a real guest must answer to, so an
	// accidental rename of the constant fails a test instead of silently
	// changing the protocol.
	if req.GetKey() == "OSSEIN_PROTO_REV" {
		return &pb.GetenvResponse{Value: &f.protoRev}, nil
	}

	return &pb.GetenvResponse{}, nil
}

func (f *fakeGuest) Copy(
	_ context.Context, _ *pb.CopyRequest, stream sandboxconnect.SandboxContextCopyServerStream,
) error {
	return f.copy(stream)
}

// dialFake serves fake over a loopback httptest server and returns a connected
// Agent. The transport is the real one — Dial's own http.Transport, Connect
// codec and HTTP/1.1 framing — with only the dialer swapped, so these tests
// cover the actual wire path rather than a mock of it. Dial ignores the address
// (production hands back a vsock conn), which is exactly why pointing the
// dialer at a TCP listener works unchanged.
func dialFake(t *testing.T, fake *fakeGuest) (*Agent, error) {
	t.Helper()

	rpc := connect.NewServer()
	sandboxconnect.RegisterSandboxContextHandler(rpc, fake)

	mux := http.NewServeMux()
	connecthttp.Mount(mux, rpc)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	addr := server.Listener.Addr().String()

	return Dial(t.Context(), func(ctx context.Context) (net.Conn, error) {
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
			copy: func(stream sandboxconnect.SandboxContextCopyServerStream) error {
				return stream.Send(&pb.CopyResponse{Status: pb.CopyResponse_COMPLETE})
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		if err := agent.CopyInArchive(t.Context(), "/rootfs", "/dev/vdb"); err != nil {
			t.Fatalf("CopyInArchive after COMPLETE: %v", err)
		}
	})

	t.Run("in-band guest error fails", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{
			protoRev: protocol.Revision,
			copy: func(stream sandboxconnect.SandboxContextCopyServerStream) error {
				return stream.Send(&pb.CopyResponse{Error: "short archive"})
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		err = agent.CopyInArchive(t.Context(), "/rootfs", "/dev/vdb")
		if err == nil || !errors.Is(err, ErrAgent) {
			t.Fatalf("in-band error must surface wrapped in ErrAgent, got: %v", err)
		}
	})

	t.Run("clean end before COMPLETE fails", func(t *testing.T) {
		t.Parallel()

		agent, err := dialFake(t, &fakeGuest{
			protoRev: protocol.Revision,
			copy: func(sandboxconnect.SandboxContextCopyServerStream) error {
				return nil // stream closes cleanly without ever sending COMPLETE
			},
		})
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		defer func() { _ = agent.Close() }()

		err = agent.CopyInArchive(t.Context(), "/rootfs", "/dev/vdb")
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

	underlying := connect.NewError(connect.CodeUnavailable, "gone")

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
