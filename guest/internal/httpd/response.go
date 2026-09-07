package httpd

import (
	"bufio"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// errAborted marks a response abandoned mid-flight after a handler panic.
var errAborted = errors.New("httpd: response aborted")

// responseBufferLimit is how much body we hold before giving up on computing a
// Content-Length and switching to chunked. Connect's unary responses are far
// smaller than this, so the common case gets a length-delimited response and the
// streaming case gets chunked — which is exactly the split net/http makes.
const (
	responseBufferLimit = 8 << 10

	// crlf terminates every line of an HTTP/1.1 message head.
	crlf = "\r\n"
)

// responseWriter implements http.ResponseWriter and http.Flusher.
//
// Connect REQUIRES http.Flusher for server-streaming — protocol.go rejects a
// non-flushable writer with CodeInternal — so Flush is part of the contract,
// not a nicety.
type responseWriter struct {
	bw  *bufio.Writer
	req *http.Request
	hdr http.Header

	status      int
	buf         []byte
	wroteHeader bool
	headerSent  bool
	chunked     bool
	bodyAllowed bool
	head        bool
	closing     bool
	err         error
}

func newResponseWriter(bw *bufio.Writer, req *http.Request) *responseWriter {
	return &responseWriter{
		bw:  bw,
		req: req,
		hdr: make(http.Header),
		// ReadRequest computes Close from the Connection header and the protocol
		// version, including HTTP/1.0 keep-alive, so this covers both.
		closing: req.Close || !req.ProtoAtLeast(1, 1),
		head:    req.Method == http.MethodHead,
	}
}

func (w *responseWriter) Header() http.Header { return w.hdr }

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader || w.err != nil {
		return
	}

	w.wroteHeader = true
	w.status = status
	// 204 and 304 must not carry a body, and must not be framed as if they
	// could. Anything a handler writes afterwards is dropped.
	w.bodyAllowed = status != http.StatusNoContent && status != http.StatusNotModified
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if w.err != nil {
		return 0, w.err
	}

	// Report the full length for dropped bodies: the handler did nothing wrong,
	// and short-write errors would only confuse it.
	if !w.bodyAllowed || w.head || len(data) == 0 {
		return len(data), nil
	}

	if w.headerSent {
		w.writeChunk(data)

		return len(data), w.err
	}

	w.buf = append(w.buf, data...)
	if len(w.buf) > responseBufferLimit {
		// Too big to length-delimit: commit to chunked and release what we have.
		w.sendHeader(true)
		w.writeChunk(w.buf)
		w.buf = nil
	}

	return len(data), w.err
}

// Flush commits the response and pushes it to the peer.
//
// Flushing means the length is no longer knowable, so this is the point where a
// buffered response becomes a chunked one. Connect calls it after every message
// on a server-stream, which is what makes Copy's progress visible to the host
// while the stream is still open.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if w.err != nil {
		return
	}

	if !w.headerSent {
		w.sendHeader(true)

		if len(w.buf) > 0 {
			w.writeChunk(w.buf)
			w.buf = nil
		}
	}

	w.flushBuffered()
}

// finish completes the response, returning any write error.
func (w *responseWriter) finish() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if w.err != nil {
		return w.err
	}

	if !w.headerSent {
		// Never flushed and never overflowed: the whole body is in hand, so it
		// can be length-delimited.
		w.sendHeader(false)

		if len(w.buf) > 0 && !w.head {
			w.writeRaw(w.buf)
		}
	} else if w.chunked {
		w.writeRaw([]byte("0" + crlf + crlf))
	}

	w.flushBuffered()

	return w.err
}

// abort marks the response unusable after a panic.
//
// If the head is already on the wire the client is mid-response and the only
// honest signal left is a truncated connection. If it is not, a 500 tells the
// host what happened instead of leaving it to interpret an EOF.
func (w *responseWriter) abort() {
	w.closing = true

	if w.headerSent {
		w.err = errAborted

		return
	}

	w.buf = nil
	w.wroteHeader = false
	w.WriteHeader(http.StatusInternalServerError)
	w.hdr.Set("Content-Type", "text/plain; charset=utf-8")
	w.buf = []byte("internal error\n")
}

// keepAlive reports whether the connection may carry another request.
func (w *responseWriter) keepAlive() bool { return !w.closing && w.err == nil }

// sendHeader serialises the status line and headers.
func (w *responseWriter) sendHeader(chunked bool) {
	w.headerSent = true
	w.chunked = chunked && w.bodyAllowed && !w.head

	text := http.StatusText(w.status)
	if text == "" {
		text = "Status"
	}

	w.writeRaw([]byte("HTTP/1.1 " + strconv.Itoa(w.status) + " " + text + crlf))

	for key, values := range w.hdr {
		if managed(key) {
			continue
		}

		for _, value := range values {
			w.writeRaw([]byte(key + ": " + sanitizeHeaderValue(value) + crlf))
		}
	}

	switch {
	case !w.bodyAllowed:
		// No framing header at all: 204/304 define their own absence of body.
	case w.chunked:
		w.writeRaw([]byte("Transfer-Encoding: chunked" + crlf))
	default:
		w.writeRaw([]byte("Content-Length: " + strconv.Itoa(len(w.buf)) + crlf))
	}

	if w.closing {
		w.writeRaw([]byte("Connection: close" + crlf))
	}

	w.writeRaw([]byte(crlf))
}

func (w *responseWriter) writeChunk(data []byte) {
	if len(data) == 0 {
		return // a zero-length chunk would terminate the body
	}

	if !w.chunked {
		w.writeRaw(data)

		return
	}

	w.writeRaw([]byte(strconv.FormatInt(int64(len(data)), 16) + crlf))
	w.writeRaw(data)
	w.writeRaw([]byte(crlf))
}

func (w *responseWriter) writeRaw(data []byte) {
	if w.err != nil {
		return
	}

	if _, err := w.bw.Write(data); err != nil {
		w.err = err
		w.closing = true
	}
}

func (w *responseWriter) flushBuffered() {
	if w.err != nil {
		return
	}

	if err := w.bw.Flush(); err != nil {
		w.err = err
		w.closing = true
	}
}

// managed reports whether this writer decides a header itself, so that a
// handler cannot contradict the framing actually on the wire. Connect sets none
// of these.
func managed(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Content-Length", "Transfer-Encoding", "Connection":
		return true
	default:
		return false
	}
}

// sanitizeHeaderValue strips CR and LF so a header value can never inject a new
// header or a second response. Connect builds its own header values, but a
// handler may echo something from the request, and response splitting is not a
// bug worth leaving reachable.
func sanitizeHeaderValue(value string) string {
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}

	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}
