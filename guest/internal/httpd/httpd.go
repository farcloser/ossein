// Package httpd implements the minimal HTTP/1.1 server vminitd needs to serve
// Connect RPCs over vsock.
//
// It exists for one reason: naming http.Server links crypto/tls. net/http's
// (*conn).serve type-asserts its net.Conn to *tls.Conn, which keeps *tls.Conn's
// method set — and behind it the handshake, x509 chain verification, the NIST
// curves and math/big — reachable. Measured on this contract, avoiding
// http.Server removes 1.50 MiB (16.3%) from the guest binary and drops
// crypto/tls from 729 linked symbols to 19. vsock never negotiates TLS, so all
// of that is dead weight inside PID 1, multiplied by every initfs we ship.
//
// This package still imports net/http — for http.Handler, http.ReadRequest and
// http.Header, which cost nothing — it just never references http.Server.
//
// # Scope
//
// This is NOT a general-purpose HTTP server and must not be used as one. It
// serves a single trusted peer (the host, enforced by the caller's listener
// before Accept returns) speaking one protocol (Connect over HTTP/1.1, unary
// plus server-streaming). Everything below is written for that contract:
//
//   - No TLS. The transport is vsock.
//   - No HTTP/2. Bidi streaming is the only thing that would need it, and this
//     contract has none.
//   - No request-body size limit. WriteFile legitimately carries large payloads;
//     bounding it belongs in connect.WithReadMaxBytes, where the error can name
//     the RPC, not here where it can only close the connection.
//   - No Date header, no automatic HEAD/Range/conditional handling. Connect
//     never issues those.
//
// # Failure policy
//
// This runs as PID 1 in a VM with no supervisor: a process exit panics the
// kernel. So every request is served under a recover, and no parse error,
// handler panic or write failure may take the process down — the blast radius
// of anything going wrong is one connection.
package httpd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

const (
	// readBufSize also bounds the request head: http.ReadRequest reads lines
	// through this bufio.Reader, so a header line longer than this fails the
	// read rather than growing without limit.
	readBufSize  = 16 << 10
	writeBufSize = 16 << 10

	// maxDrainBytes caps how much unread request body we will consume to keep a
	// connection alive. Past this, closing costs less than reading.
	maxDrainBytes = 256 << 10
)

// ErrServerClosed is returned by Serve when the listener is closed.
var ErrServerClosed = errors.New("httpd: server closed")

// Server serves HTTP/1.1 on a listener. The zero value is not useful: Handler
// must be set.
type Server struct {
	// Handler receives every request. Required.
	Handler http.Handler

	// ReadHeaderTimeout bounds how long a peer may take to finish sending a
	// request head ONCE IT HAS STARTED. It deliberately does not cover idle time
	// between requests — see serveConn.
	ReadHeaderTimeout time.Duration

	// Logf receives connection-level anomalies: panics, malformed requests. Nil
	// discards them.
	Logf func(format string, args ...any)
}

// Serve accepts connections and serves each in its own goroutine. It returns
// only when the listener fails, which for vminitd means shutdown.
//
// Unlike net/http, there is no retry-on-temporary-error loop: the caller's
// listener already filters non-host peers and returns their rejection as a
// dropped connection rather than an error, so an error here is terminal.
func (s *Server) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("%w: accept: %w", ErrServerClosed, err)
		}

		go s.serveConn(conn)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// serveConn runs the request loop for one connection.
func (s *Server) serveConn(conn net.Conn) {
	defer func() {
		// The loop below is panic-safe per request, but a panic in the loop
		// itself (or in a deferred call) must still not reach the runtime and
		// kill PID 1.
		if rec := recover(); rec != nil {
			s.logf("httpd: panic serving connection: %v\n%s", rec, debug.Stack())
		}

		_ = conn.Close()
	}()

	reader := bufio.NewReaderSize(conn, readBufSize)
	writer := bufio.NewWriterSize(conn, writeBufSize)

	for {
		if !s.serveOne(conn, reader, writer) {
			return
		}
	}
}

// serveOne reads and answers a single request, reporting whether the connection
// may be reused.
func (s *Server) serveOne(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer) bool {
	// Block indefinitely for the FIRST byte, then bound the rest of the head.
	//
	// Splitting it this way is what makes an idle timeout unnecessary. A single
	// deadline covering idle time would race the client: connect issues POSTs,
	// and net/http's Transport will not retry a request it has already started
	// writing, so closing an idle-but-live connection surfaces as a spurious RPC
	// failure. Waiting for the first byte with no deadline removes that race,
	// while ReadHeaderTimeout still bounds a peer that starts a request and
	// stalls mid-head.
	if _, err := reader.Peek(1); err != nil {
		return false // clean EOF, or the peer went away
	}

	if s.ReadHeaderTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.ReadHeaderTimeout))
	}

	req, err := http.ReadRequest(reader)

	// Clear the deadline unconditionally: the body and the handler are not
	// bounded by it. WaitProcess blocks for the container's entire life and Copy
	// streams a rootfs, so any deadline covering handler execution would be a
	// bug, not a safeguard.
	_ = conn.SetReadDeadline(time.Time{})

	if err != nil {
		s.rejectMalformed(writer, err)

		return false
	}

	// Take over the body so drain decisions survive the handler closing it.
	body := &trackedBody{rc: req.Body}
	if req.Body != nil {
		req.Body = body
	}

	if expectsContinue(req) {
		_, _ = writer.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		if writer.Flush() != nil {
			return false
		}
	}

	res := newResponseWriter(writer, req)
	s.serveRequest(res, req)

	if res.finish() != nil {
		return false
	}

	if req.Body == nil {
		return res.keepAlive()
	}

	return body.drained() && res.keepAlive()
}

// serveRequest calls the handler with a recover in place.
//
// A panic leaves the response half-written and the protocol state unknowable,
// so the connection is always abandoned afterwards. If nothing has reached the
// wire yet we still send a 500, which is strictly more useful than net/http's
// behaviour of closing silently: the host gets an RPC error naming the failure
// instead of an opaque EOF it has to guess about.
func (s *Server) serveRequest(res *responseWriter, req *http.Request) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}

		res.abort()

		// ErrAbortHandler is the documented way for a handler to drop a
		// connection deliberately; it is not a bug and gets no stack.
		if errors.Is(asError(rec), http.ErrAbortHandler) {
			return
		}

		s.logf("httpd: panic serving %s: %v\n%s", req.URL.Path, rec, debug.Stack())
	}()

	s.Handler.ServeHTTP(res, req)
}

// rejectMalformed answers an unparseable request head, best-effort.
//
// A read timeout or a peer that vanished gets nothing: there is either no one
// listening or no point telling them.
func (s *Server) rejectMalformed(writer *bufio.Writer, err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isTimeout(err) {
		return
	}

	s.logf("httpd: malformed request: %v", err)

	_, _ = writer.WriteString("HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
	_ = writer.Flush()
}

func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// asError turns a recovered value into an error for errors.Is.
func asError(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}

	return nil
}

// expectsContinue reports whether the peer is waiting for a 100 Continue.
//
// Connect's client does not send Expect, but answering it costs two lines and
// the alternative is a peer that waits forever for a response we never send.
func expectsContinue(req *http.Request) bool {
	return req.ProtoAtLeast(1, 1) && strings.EqualFold(req.Header.Get("Expect"), "100-continue")
}
