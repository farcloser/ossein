//go:build darwin

package container

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// noHalfClose hides CloseWrite from a conn that has one: a duplex conn whose
// write side cannot be shut down independently. This modelled vz's
// VirtioSocketConnection until the fork gave it CloseWrite; it now models only
// the fallback branch, which no production conn reaches. The relay's behaviour
// is chosen by this capability alone, so hiding the method is the whole
// fidelity requirement either way.
type noHalfClose struct {
	net.Conn
}

// socketPair returns a connected *net.UnixConn pair. socketpair(2) rather than
// a listener: macOS caps sun_path at 104 bytes and t.TempDir() paths overrun it.
func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}

	conns := make([]*net.UnixConn, 0, len(fds))

	for _, fd := range fds {
		file := os.NewFile(uintptr(fd), "socketpair")

		conn, err := net.FileConn(file)
		_ = file.Close() // FileConn dups; the original is ours to close

		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}

		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			t.Fatalf("FileConn returned %T, want *net.UnixConn", conn)
		}

		t.Cleanup(func() { _ = unixConn.Close() })

		conns = append(conns, unixConn)
	}

	return conns[0], conns[1]
}

// readWithin reads until EOF or deadline, returning what arrived.
func readWithin(t *testing.T, conn net.Conn, d time.Duration) string {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(d))

	buf, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Logf("read: %v", err)
		}
	}

	return string(buf)
}

// TestBidiPipeHalfClosePropagatesWhenSupported pins the production path: when
// the destination implements CloseWrite — which both ends of a real relay now
// do — a client that sends, half-closes, and then reads still gets the reply.
func TestBidiPipeHalfClosePropagatesWhenSupported(t *testing.T) {
	t.Parallel()

	client, hostSide := socketPair(t)
	relaySide, server := socketPair(t)

	go bidiPipe(hostSide, relaySide)

	// Server: read the request to EOF, then reply and close.
	served := make(chan struct{})

	go func() {
		defer close(served)

		buf := make([]byte, 3)
		if _, err := io.ReadFull(server, buf); err != nil {
			return
		}

		// Wait for the half-close to arrive as EOF before replying.
		_, _ = io.Copy(io.Discard, server)
		_, _ = server.Write([]byte("resp"))
		_ = server.Close()
	}()

	if _, err := client.Write([]byte("req")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	if got := readWithin(t, client, 2*time.Second); got != "resp" {
		t.Fatalf("reply = %q, want %q — half-close was not propagated", got, "resp")
	}

	<-served
}

// TestBidiPipeFullCloseWhenHalfCloseUnavailable pins the fallback and the
// reason it is written the way it is: given a conn with no CloseWrite the
// relay must close the whole connection, which guarantees teardown at the cost
// of truncating the reply. No production conn takes this branch since vz
// gained CloseWrite; it stays because the alternative — doing nothing when the
// destination cannot half-close — leaks a conn pair per client.
func TestBidiPipeFullCloseWhenHalfCloseUnavailable(t *testing.T) {
	t.Parallel()

	client, hostSide := socketPair(t)
	relaySide, server := socketPair(t)

	// The relay sees a conn with no CloseWrite, exactly like vz's.
	go bidiPipe(hostSide, noHalfClose{relaySide})

	serverSawEOF := make(chan struct{})

	go func() {
		defer close(serverSawEOF)

		_, _ = io.Copy(io.Discard, server) // returns when the relay closes it
	}()

	if _, err := client.Write([]byte("req")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	// Teardown is the property being bought: the guest end must observe the
	// connection ending rather than waiting forever for a client that is done.
	select {
	case <-serverSawEOF:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not tear the connection down; the pump would leak")
	}

	// And the documented cost: a reply written after that point cannot arrive.
	if _, err := server.Write([]byte("resp")); err == nil {
		if got := readWithin(t, client, 500*time.Millisecond); got == "resp" {
			t.Fatal("reply survived: bidiPipe now propagates half-close, update the docs")
		}
	}
}
