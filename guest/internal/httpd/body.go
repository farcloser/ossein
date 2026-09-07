package httpd

import (
	"errors"
	"io"
	"net/http"
)

// trackedBody wraps a request body so the server can tell, after the handler
// returns, whether the stream was actually consumed.
//
// Asking the body directly does not work: a handler (Connect does this) closes
// the body when it is done, and reading a closed http body returns an error.
// Treating that error as "unread bytes remain" makes the server drop a
// perfectly good keep-alive connection after EVERY request — which shows up not
// as a failure but as connect's pooled POSTs racing a close they are not
// allowed to retry.
//
// So Close here is bookkeeping only. The real body is closed by the server once
// it has decided what to do with the connection.
type trackedBody struct {
	rc     io.ReadCloser
	eof    bool
	closed bool
}

func (b *trackedBody) Read(data []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}

	n, err := b.rc.Read(data)
	if errors.Is(err, io.EOF) {
		b.eof = true
	}

	return n, err //nolint:wrapcheck // this is a transparent io.Reader wrapper
}

func (b *trackedBody) Close() error {
	b.closed = true

	return nil
}

// drained reports whether the connection can be reused, consuming any leftover
// body up to a bound.
func (b *trackedBody) drained() bool {
	defer func() { _ = b.rc.Close() }()

	if b.eof {
		return true
	}

	// Read past the cap deliberately: hitting it means more is coming, and
	// draining an unbounded body costs more than a new connection.
	n, err := io.Copy(io.Discard, io.LimitReader(b.rc, maxDrainBytes+1))
	if err != nil {
		return false
	}

	return n <= maxDrainBytes
}
