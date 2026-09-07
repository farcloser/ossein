//go:build linux

package guestagent

import (
	"context"
	"io"
	"net"
	"sync"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"

	pb "github.com/farcloser/ossein/internal/sandbox"
)

// proxyTable tracks live socket proxies by id so StopVsockProxy can tear them
// down. The stored net.Listener is the front (accepting) side; closing it stops
// the accept loop.
type proxyTable struct {
	mu sync.Mutex
	m  map[string]net.Listener
}

func newProxyTable() *proxyTable { return &proxyTable{m: map[string]net.Listener{}} }

// ProxyVsock bridges a guest unix socket and a host vsock port. OUT_OF exposes a
// guest socket to the host (host dials the vsock port, guest forwards to the
// unix socket) — this is how buildkitd's socket reaches host `buildctl`. INTO
// (the reverse direction) has no host-side caller and is not implemented.
func (a *Agent) ProxyVsock(_ context.Context, req *pb.ProxyVsockRequest) (*pb.ProxyVsockResponse, error) {
	var (
		listener net.Listener
		err      error
	)

	switch req.GetAction() {
	case pb.ProxyVsockRequest_OUT_OF:
		// The proxy deliberately outlives this RPC, so its backend dials use
		// context.Background() instead of the RPC context (see proxyOutOf).
		//nolint:contextcheck // see above
		listener, err = proxyOutOf(req)
	case pb.ProxyVsockRequest_INTO:
		return nil, rpcErrorf(connect.CodeUnimplemented, "INTO proxying is not supported")
	default:
		return nil, rpcErrorf(connect.CodeInvalidArgument, "unknown proxy action %v", req.GetAction())
	}

	if err != nil {
		return nil, err
	}

	a.proxies.mu.Lock()
	if old, ok := a.proxies.m[req.GetId()]; ok {
		_ = old.Close()
	}

	a.proxies.m[req.GetId()] = listener
	a.proxies.mu.Unlock()

	return &pb.ProxyVsockResponse{}, nil
}

// StopVsockProxy closes the proxy registered under id (idempotent).
func (a *Agent) StopVsockProxy(_ context.Context, req *pb.StopVsockProxyRequest) (*pb.StopVsockProxyResponse, error) {
	a.proxies.mu.Lock()
	listener, ok := a.proxies.m[req.GetId()]
	delete(a.proxies.m, req.GetId())
	a.proxies.mu.Unlock()

	if ok {
		_ = listener.Close()
	}

	return &pb.StopVsockProxyResponse{}, nil
}

// proxyOutOf listens on the vsock port and forwards each accepted connection to
// the guest unix socket. The unix socket is dialed lazily (per connection), so
// the backend need not exist yet when the proxy is created.
func proxyOutOf(req *pb.ProxyVsockRequest) (net.Listener, error) {
	listener, err := vsock.Listen(req.GetVsockPort(), nil)
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "vsock listen :%d: %v", req.GetVsockPort(), err)
	}

	guestPath := req.GetGuestPath()

	// The proxy outlives the RPC that created it, so the backend dial must not
	// inherit the RPC context; Background matches the listener's lifetime.
	dialer := &net.Dialer{}

	go acceptAndProxy(listener, func() (net.Conn, error) {
		return dialer.DialContext(context.Background(), "unix", guestPath)
	})

	return listener, nil
}

// acceptAndProxy accepts on listener until it is closed, piping each front-side
// connection to a freshly dialed backend.
func acceptAndProxy(listener net.Listener, dial func() (net.Conn, error)) {
	for {
		front, err := listener.Accept()
		if err != nil {
			return // listener closed
		}

		go func() {
			defer func() { _ = front.Close() }()

			back, err := dial()
			if err != nil {
				return
			}
			defer func() { _ = back.Close() }()

			bidiCopy(front, back)
		}()
	}
}

// closeWriter is the shutdown(SHUT_WR) half of a duplex conn. Both conns this
// proxy handles implement it: mdlayher's *vsock.Conn and the container's
// *net.UnixConn.
type closeWriter interface {
	CloseWrite() error
}

// bidiCopy relays both directions and returns once BOTH are done; the caller
// closes the pair.
//
// Ending one direction shuts down only that write side, so a container server
// that finishes replying does not cut off whatever the client is still sending,
// and the peer still sees a clean EOF. Tearing both conns down on the first EOF
// — the obvious shape — silently truncates the other direction.
//
// This is the half of the path that CAN do it: the host end cannot, because vz
// exposes no shutdown on its vsock connection (see bidiPipe in pkg/container),
// so a host→guest half-close still arrives as a full close. That is fine here —
// a full close ends both pumps — and it is why fixing this end alone does not
// make EOF-framed protocols work end to end.
func bidiCopy(front, back net.Conn) {
	var relay sync.WaitGroup

	pump := func(dst, src net.Conn) {
		defer relay.Done()

		_, _ = io.Copy(dst, src)

		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()

			return
		}

		// No shutdown available: closing is the only way to end the peer's
		// read, and leaving it open would hang the other pump forever.
		_ = dst.Close()
	}

	relay.Add(2)

	go pump(front, back)
	go pump(back, front)

	relay.Wait()
}
