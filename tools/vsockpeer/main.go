// Command vsockpeer is the GUEST half of the vsock proxy throughput bench
// (tools/vsockbench). It runs inside the container and serves a unix socket —
// the same shape as buildkitd behind ExposeUnix — so the host can push and
// pull bulk bytes through the production proxy path: host unix socket →
// bidiPipe → vz vsock → guest agent bidiCopy → this socket.
//
// Protocol per connection (client speaks first): one mode byte, then a
// big-endian uint64 byte count.
//
//	'R' — RECV: the peer SOURCES count bytes to the client, then half-closes.
//	'S' — SEND: the peer SINKS count bytes from the client, then writes one
//	      ack byte so the client's clock stops only once every byte arrived.
//
// Both loops use a 1 MiB buffer with the ReaderFrom/WriterTo fast paths hidden,
// so this end is never the bottleneck being measured.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
)

const (
	modeRecv = 'R'
	modeSend = 'S'
	bufSize  = 1 << 20
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vsockpeer <unix-socket-path>")
		os.Exit(2)
	}

	path := os.Args[1]
	_ = os.Remove(path) // #nosec G703 -- the operator names the socket path; there is nothing to traverse

	var listenCfg net.ListenConfig

	listener, err := listenCfg.Listen(context.Background(), "unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "vsockpeer: serving", path)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			os.Exit(1)
		}

		go serve(conn)
	}
}

func serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var header [9]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		fmt.Fprintln(os.Stderr, "header:", err)

		return
	}

	count := binary.BigEndian.Uint64(header[1:])
	buf := make([]byte, bufSize)

	switch header[0] {
	case modeRecv:
		src := struct{ io.Reader }{io.LimitReader(zeroReader{}, int64(count))} // #nosec G115 -- bench sizes
		if _, err := io.CopyBuffer(struct{ io.Writer }{conn}, src, buf); err != nil {
			fmt.Fprintln(os.Stderr, "source:", err)
		}

		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	case modeSend:
		src := struct{ io.Reader }{io.LimitReader(conn, int64(count))} // #nosec G115 -- bench sizes

		n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, src, buf)
		if err != nil || uint64(n) != count { // #nosec G115 -- n >= 0
			fmt.Fprintln(os.Stderr, "sink:", n, err)

			return
		}

		_, _ = conn.Write([]byte{1})
	default:
		fmt.Fprintf(os.Stderr, "bad mode: %q\n", header[0])
	}
}

// zeroReader is an endless source of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}
