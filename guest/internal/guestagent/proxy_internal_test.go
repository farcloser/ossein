//go:build linux

package guestagent

import (
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// socketPair returns a connected *net.UnixConn pair via socketpair(2), which
// needs no filesystem path and so no sun_path budget.
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
		_ = file.Close()

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

// TestBidiCopyHalfCloseDoesNotCutTheOtherDirection is the regression test for
// the proxy tearing both conns down on the first EOF: a container server that
// finishes replying must not cut off what the client is still sending.
func TestBidiCopyHalfCloseDoesNotCutTheOtherDirection(t *testing.T) {
	t.Parallel()

	client, front := socketPair(t)
	back, server := socketPair(t)

	relayDone := make(chan struct{})

	go func() {
		defer close(relayDone)

		bidiCopy(front, back)
	}()

	// The server replies and shuts down only its write side.
	if _, err := server.Write([]byte("resp")); err != nil {
		t.Fatalf("server write: %v", err)
	}

	if err := server.CloseWrite(); err != nil {
		t.Fatalf("server half-close: %v", err)
	}

	// The client must see the reply, then a clean EOF on that direction.
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("client read: %v", err)
	}

	if string(got) != "resp" {
		t.Fatalf("reply = %q, want %q", got, "resp")
	}

	// The request direction must still be alive: this is what closing both
	// conns on the first EOF destroyed.
	if _, err := client.Write([]byte("more")); err != nil {
		t.Fatalf("client write after server half-close: %v", err)
	}

	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))

	tail := make([]byte, 4)
	if _, err := io.ReadFull(server, tail); err != nil {
		t.Fatalf("server did not receive data sent after its half-close: %v", err)
	}

	if string(tail) != "more" {
		t.Fatalf("tail = %q, want %q", tail, "more")
	}

	// Closing the client's write side ends the last direction, so the relay
	// returns and its caller can close the pair.
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client half-close: %v", err)
	}

	select {
	case <-relayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("bidiCopy did not return after both directions ended")
	}
}
