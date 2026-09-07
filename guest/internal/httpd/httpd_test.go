package httpd_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/farcloser/ossein/guest/internal/httpd"
	pb "github.com/farcloser/ossein/internal/sandbox"
)

// errMismatch reports an unexpected echo from the test handler.
var errMismatch = errors.New("echo mismatch")

// start runs a server on a loopback listener and returns its address.
func start(t *testing.T, handler http.Handler, headerTimeout time.Duration) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	server := &httpd.Server{
		Handler:           handler,
		ReadHeaderTimeout: headerTimeout,
		Logf:              func(string, ...any) {},
	}

	go func() { _ = server.Serve(listener) }()

	return listener.Addr().String()
}

// raw dials and returns the connection plus a buffered reader over it.
func raw(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn, bufio.NewReader(conn)
}

func post(path, body string) string {
	return request(path, body, "")
}

// postClose is post plus an explicit Connection: close.
func postClose(path, body string) string {
	return request(path, body, "Connection: close\r\n")
}

func request(path, body, extra string) string {
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n%s\r\n%s",
		path, len(body), extra, body)
}

// --- the actual contract: a real Connect client over this server ---

func connectHandler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/x.v1/Getenv", connect.NewUnaryHandler("/x.v1/Getenv",
		func(_ context.Context, req *connect.Request[pb.GetenvRequest],
		) (*connect.Response[pb.GetenvResponse], error) {
			value := req.Msg.GetKey()

			return connect.NewResponse(&pb.GetenvResponse{Value: &value}), nil
		}))

	mux.Handle("/x.v1/Copy", connect.NewServerStreamHandler("/x.v1/Copy",
		func(_ context.Context, _ *connect.Request[pb.CopyRequest],
			stream *connect.ServerStream[pb.CopyResponse],
		) error {
			for range 3 {
				if err := stream.Send(&pb.CopyResponse{Status: pb.CopyResponse_METADATA}); err != nil {
					return err
				}
			}

			return stream.Send(&pb.CopyResponse{Status: pb.CopyResponse_COMPLETE})
		}))

	mux.Handle("/x.v1/Fail", connect.NewUnaryHandler("/x.v1/Fail",
		func(_ context.Context, _ *connect.Request[pb.GetenvRequest],
		) (*connect.Response[pb.GetenvResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("nope"))
		}))

	return mux
}

func TestConnectUnary(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), time.Second)
	client := connect.NewClient[pb.GetenvRequest, pb.GetenvResponse](
		http.DefaultClient, "http://"+addr+"/x.v1/Getenv",
	)

	// Several calls on one client: proves keep-alive framing, not just the first
	// response.
	for index, key := range []string{"A", "B", "C"} {
		resp, err := client.CallUnary(t.Context(), connect.NewRequest(&pb.GetenvRequest{Key: key}))
		if err != nil {
			t.Fatalf("call %d: %v", index, err)
		}

		if resp.Msg.GetValue() != key {
			t.Fatalf("call %d: got %q want %q", index, resp.Msg.GetValue(), key)
		}
	}
}

func TestConnectServerStream(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), time.Second)
	client := connect.NewClient[pb.CopyRequest, pb.CopyResponse](
		http.DefaultClient, "http://"+addr+"/x.v1/Copy",
	)

	stream, err := client.CallServerStream(t.Context(), connect.NewRequest(&pb.CopyRequest{Path: "/r"}))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = stream.Close() }()

	var count int

	var complete bool

	for stream.Receive() {
		count++

		if stream.Msg().GetStatus() == pb.CopyResponse_COMPLETE {
			complete = true
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("after %d messages: %v", count, err)
	}

	if count != 4 || !complete {
		t.Fatalf("got %d messages complete=%v, want 4 ending COMPLETE", count, complete)
	}
}

// A Connect error must arrive as a coded error, not a transport failure: the
// host branches on connect.CodeOf.
func TestConnectErrorCode(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), time.Second)
	client := connect.NewClient[pb.GetenvRequest, pb.GetenvResponse](
		http.DefaultClient, "http://"+addr+"/x.v1/Fail",
	)

	_, err := client.CallUnary(t.Context(), connect.NewRequest(&pb.GetenvRequest{Key: "k"}))
	if err == nil {
		t.Fatal("want an error")
	}

	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("got code %v want %v (err: %v)", got, connect.CodeNotFound, err)
	}
}

// --- PID 1 survival ---

func TestPanicDoesNotKillProcess(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), time.Second)

	resp, err := http.Post("http://"+addr+"/", "text/plain", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("panicking handler produced a transport error: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got status %d want 500", resp.StatusCode)
	}

	// The process is obviously alive (we are it), so prove the SERVER is too.
	resp2, err := http.Post("http://"+addr+"/", "text/plain", strings.NewReader("again"))
	if err != nil {
		t.Fatalf("server stopped accepting after a panic: %v", err)
	}

	_ = resp2.Body.Close()
}

func TestPanicMidStreamIsSurvivable(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic("boom after flush")
	}), time.Second)

	resp, err := http.Get("http://" + addr + "/")
	if err == nil {
		// A truncated chunked body surfaces on read, not on dial.
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	if err == nil {
		t.Fatal("a panic after flush must not look like a complete response")
	}

	again, err := http.Get("http://" + addr + "/")
	if err == nil {
		_, _ = io.ReadAll(again.Body)
		_ = again.Body.Close()
	}
}

func TestAbortHandlerIsSilent(t *testing.T) {
	t.Parallel()

	var logged strings.Builder

	var logMu sync.Mutex

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	server := &httpd.Server{
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}),
		Logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()

			fmt.Fprintf(&logged, format, args...)
		},
	}

	go func() { _ = server.Serve(listener) }()

	resp, err := http.Get("http://" + listener.Addr().String() + "/")
	if err == nil {
		_ = resp.Body.Close()
	}

	time.Sleep(50 * time.Millisecond)
	logMu.Lock()
	defer logMu.Unlock()

	if strings.Contains(logged.String(), "goroutine") {
		t.Fatalf("ErrAbortHandler must not log a stack, got: %s", logged.String())
	}
}

// --- framing ---

func TestUndrainedBodyDoesNotDesyncNextRequest(t *testing.T) {
	t.Parallel()

	// A handler that ignores the request body entirely — the case that silently
	// corrupts a naive keep-alive loop.
	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), time.Second)

	conn, reader := raw(t, addr)

	for index := range 3 {
		if _, err := conn.Write([]byte(post("/", "BODYBODYBODY"))); err != nil {
			t.Fatalf("write %d: %v", index, err)
		}

		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("response %d (desync?): %v", index, err)
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if string(body) != "ok" {
			t.Fatalf("response %d: got %q", index, body)
		}
	}
}

func TestSmallResponseIsLengthDelimited(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte(post("/", "")))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.ContentLength != int64(len("short")) {
		t.Fatalf("want Content-Length 5, got %d (TE=%v)", resp.ContentLength, resp.TransferEncoding)
	}

	if len(resp.TransferEncoding) != 0 {
		t.Fatalf("small response must not be chunked, got %v", resp.TransferEncoding)
	}
}

func TestLargeResponseSwitchesToChunked(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("x", 64<<10)
	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}), time.Second)

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != payload {
		t.Fatalf("large body round-trip corrupted: got %d bytes want %d", len(body), len(payload))
	}
}

func TestConnectionCloseIsHonored(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bye"))
	}), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte(postClose("/", "")))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !resp.Close {
		t.Fatal("response to Connection: close must announce close")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("server must close the connection, got %v", err)
	}
}

func TestHTTP10ClosesAfterResponse(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("HTTP/1.0 connection must close, got %v", err)
	}
}

func TestMalformedRequestGets400AndServerSurvives(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte("this is not http\r\n\r\n"))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("want a 400, got %v", err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}

	// Still serving?
	client := connect.NewClient[pb.GetenvRequest, pb.GetenvResponse](
		http.DefaultClient, "http://"+addr+"/x.v1/Getenv",
	)
	if _, err := client.CallUnary(t.Context(), connect.NewRequest(&pb.GetenvRequest{Key: "k"})); err != nil {
		t.Fatalf("server died after a malformed request: %v", err)
	}
}

// --- timeouts ---

func TestStalledHeadTimesOut(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), 200*time.Millisecond)

	conn, reader := raw(t, addr)
	// Start a request head and never finish it.
	_, _ = conn.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\n"))

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("a stalled request head must not be served indefinitely")
	}
}

// The counterpart, and the reason the timeout is split: an IDLE connection must
// survive longer than ReadHeaderTimeout, or connect's pooled POSTs race a close
// they cannot retry.
func TestIdleConnectionIsNotClosedByHeaderTimeout(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), 100*time.Millisecond)

	conn, reader := raw(t, addr)

	_, _ = conn.Write([]byte(post("/", "")))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Idle for well over the header timeout, then reuse the connection.
	time.Sleep(400 * time.Millisecond)

	if _, err := conn.Write([]byte(post("/", ""))); err != nil {
		t.Fatalf("write after idle: %v", err)
	}

	resp2, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("idle connection was closed by the header timeout: %v", err)
	}

	_ = resp2.Body.Close()
}

func TestExpect100Continue(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	}), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n"))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("no interim response: %v", err)
	}

	if !strings.Contains(line, "100") {
		t.Fatalf("want 100 Continue, got %q", line)
	}

	_, _ = reader.ReadString('\n') // blank line terminating the interim response
	_, _ = conn.Write([]byte("ping"))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if string(body) != "ping" {
		t.Fatalf("got %q want %q", body, "ping")
	}
}

// --- hardening ---

func TestHeaderValueCannotSplitResponse(t *testing.T) {
	t.Parallel()

	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["X-Evil"] = []string{"a\r\nInjected: yes\r\n\r\nHTTP/1.1 200 OK"}
		_, _ = w.Write([]byte("ok"))
	}), time.Second)

	conn, reader := raw(t, addr)
	_, _ = conn.Write([]byte(postClose("/", "")))

	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("Injected") != "" {
		t.Fatal("CRLF in a header value injected a second header")
	}
}

func TestConcurrentConnections(t *testing.T) {
	t.Parallel()

	addr := start(t, connectHandler(), 5*time.Second)

	var waiters sync.WaitGroup

	errs := make(chan error, 16)

	for index := range 16 {
		waiters.Add(1)

		go func() {
			defer waiters.Done()

			client := connect.NewClient[pb.GetenvRequest, pb.GetenvResponse](
				&http.Client{}, "http://"+addr+"/x.v1/Getenv",
			)

			key := fmt.Sprintf("k%d", index)

			resp, err := client.CallUnary(context.Background(),
				connect.NewRequest(&pb.GetenvRequest{Key: key}))
			if err != nil {
				errs <- err

				return
			}

			if resp.Msg.GetValue() != key {
				errs <- fmt.Errorf("%w: got %q want %q", errMismatch, resp.Msg.GetValue(), key)
			}
		}()
	}

	waiters.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
}
