//go:build darwin && arm64

// Command vsockbench measures bulk throughput through ossein's PRODUCTION vsock
// proxy path — the one `ossein buildkit` puts every build byte through:
//
//	host unix socket → bidiPipe (pkg/container) → vz vsock →
//	guest agent bidiCopy (vsock ⇄ unix) → the container's unix socket
//
// It boots a container running tools/vsockpeer (bind-mounted in from build/),
// exposes the peer's socket with Instance.ExposeUnix exactly as the buildkit
// command does, then pushes (host→guest) and pulls (guest→host) N bytes over
// one or more concurrent connections and reports MiB/s.
//
// The client's own copy loop uses a 1 MiB buffer with the ReaderFrom/WriterTo
// fast paths hidden, so what is measured is the relay, not this program.
//
//	build/vsockbench -image debian -bytes 2147483648 -streams 1 -runs 3
//
// Requires the VZ entitlement (codesign like build/ossein) and the guest
// artifacts: -kernel (default pkg/guestartifacts/kernel-arm64) and -initfs
// (default build/initfs.cpio, so a rebuilt guest agent is what gets measured).
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mycophonic/primordium/filesystem/dirs"

	"github.com/farcloser/ossein/pkg/container"
	"github.com/farcloser/ossein/pkg/image"
)

const (
	modeRecv     = 'R'
	modeSend     = 'S'
	bufSize      = 1 << 20
	headerSize   = 9
	guestBench   = "/bench"
	guestPeer    = "/bench/vsockpeer"
	guestSock    = "/tmp/vsockpeer.sock"
	mib          = 1 << 20
	peerSettle   = 500 * time.Millisecond
	dialAttempts = 40
	dialWait     = 250 * time.Millisecond
	defaultBytes = 2 << 30
	defaultCPUs  = 2
	defaultMem   = 2048
	defaultRuns  = 3
)

var (
	errPeerSilent = errors.New("peer never answered through the proxy")
	errShort      = errors.New("short transfer")
)

// options is everything the flags decide.
type options struct {
	image, kernel, initfs, benchDir, label string
	nbytes                                 int64
	streams, runs                          int
	cpus                                   uint
	memMiB                                 uint64
}

func main() {
	var opts options

	flag.StringVar(&opts.image, "image", "debian", "container image (needs nothing but a shell)")
	flag.StringVar(&opts.kernel, "kernel", "pkg/guestartifacts/kernel-arm64", "guest kernel")
	flag.StringVar(&opts.initfs, "initfs", "build/initfs.cpio", "vminitd initfs (the guest agent under test)")
	flag.StringVar(&opts.benchDir, "bench-dir", "build", "host dir holding vsockpeer; bind-mounted at "+guestBench)
	flag.Int64Var(&opts.nbytes, "bytes", defaultBytes, "bytes per stream per run")
	flag.IntVar(&opts.streams, "streams", 1, "concurrent connections")
	flag.IntVar(&opts.runs, "runs", defaultRuns, "runs per direction")
	flag.UintVar(&opts.cpus, "cpus", defaultCPUs, "guest vCPUs")
	flag.Uint64Var(&opts.memMiB, "memory", defaultMem, "guest memory MiB")
	flag.StringVar(&opts.label, "label", "", "free-text label echoed in the result lines")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "vsockbench:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	absBench, err := filepath.Abs(opts.benchDir)
	if err != nil {
		return fmt.Errorf("bench dir: %w", err)
	}

	if _, err := os.Stat(filepath.Join(absBench, "vsockpeer")); err != nil {
		return fmt.Errorf("vsockpeer not built: %w (just build-vsockbench)", err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// Same app name as cmd/ossein, so the bench shares its image cache and
	// runtime dirs instead of minting a parallel tree under a bare "images".
	dirs.SetAppName("ossein")

	cache, err := image.NewCache()
	if err != nil {
		return fmt.Errorf("image cache: %w", err)
	}

	defer func() { _ = cache.Close() }()

	spec := container.RunSpec{
		Image:     opts.image,
		Command:   []string{guestPeer, guestSock},
		CPUs:      opts.cpus,
		MemoryMiB: opts.memMiB,
		Mounts:    []container.Mount{{Host: absBench, Dest: guestBench, ReadOnly: true}},
		Stdout:    os.Stderr,
		Stderr:    os.Stderr,
	}

	inst, _, err := container.Boot(ctx, container.Artifacts{Kernel: opts.kernel, Initfs: opts.initfs}, cache, spec)
	if err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	defer inst.Close(context.Background())

	if err := inst.StartProcess(ctx); err != nil {
		return fmt.Errorf("start peer: %w", err)
	}

	hostSock := filepath.Join(inst.Dir, "vsockbench.sock")

	cleanup, err := inst.ExposeUnix(ctx, guestSock, hostSock)
	if err != nil {
		return fmt.Errorf("expose: %w", err)
	}
	defer cleanup()

	// The peer's listen races our first dial: retry until it answers.
	if err := waitPeer(ctx, hostSock); err != nil {
		return err
	}

	time.Sleep(peerSettle)

	return measure(ctx, hostSock, opts)
}

func measure(ctx context.Context, hostSock string, opts options) error {
	_, _ = fmt.Fprintf(os.Stdout, "# vsockbench label=%q image=%s bytes=%d streams=%d runs=%d\n",
		opts.label, opts.image, opts.nbytes, opts.streams, opts.runs)

	for _, direction := range []struct {
		name string
		mode byte
	}{{"guest->host", modeRecv}, {"host->guest", modeSend}} {
		for runIdx := range opts.runs {
			elapsed, err := oneRun(ctx, hostSock, direction.mode, opts.nbytes, opts.streams)
			if err != nil {
				return fmt.Errorf("%s run %d: %w", direction.name, runIdx, err)
			}

			total := float64(opts.nbytes) * float64(opts.streams)
			_, _ = fmt.Fprintf(os.Stdout, "%-12s streams=%d run=%d  %8.1f MiB/s  (%.2fs)\n",
				direction.name, opts.streams, runIdx, total/mib/elapsed.Seconds(), elapsed.Seconds())
		}
	}

	return nil
}

func waitPeer(ctx context.Context, hostSock string) error {
	dialer := net.Dialer{Timeout: time.Second}

	for range dialAttempts {
		conn, err := dialer.DialContext(ctx, "unix", hostSock)
		if err == nil {
			// A connection accepted by the host relay is not proof the guest
			// side answered; a zero-length probe is: RECV of 0 bytes must EOF.
			_, _ = conn.Write(make([]byte, headerSize))

			var one [1]byte

			_, rerr := conn.Read(one[:])
			_ = conn.Close()

			if errors.Is(rerr, io.EOF) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for peer: %w", ctx.Err())
		case <-time.After(dialWait):
		}
	}

	return errPeerSilent
}

// oneRun opens `streams` connections and moves nbytes on each, concurrently;
// the clock covers the slowest stream.
func oneRun(ctx context.Context, hostSock string, mode byte, nbytes int64, streams int) (time.Duration, error) {
	conns := make([]net.Conn, 0, streams)

	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()

	dialer := net.Dialer{}

	for range streams {
		conn, err := dialer.DialContext(ctx, "unix", hostSock)
		if err != nil {
			return 0, fmt.Errorf("dial %s: %w", hostSock, err)
		}

		conns = append(conns, conn)
	}

	var (
		group   sync.WaitGroup
		errLock sync.Mutex
		errs    []error
	)

	start := time.Now()

	for _, conn := range conns {
		group.Go(func() {
			if err := oneStream(conn, mode, nbytes); err != nil {
				errLock.Lock()

				errs = append(errs, err)

				errLock.Unlock()
			}
		})
	}

	group.Wait()

	return time.Since(start), errors.Join(errs...)
}

func oneStream(conn net.Conn, mode byte, nbytes int64) error {
	var header [headerSize]byte

	header[0] = mode
	binary.BigEndian.PutUint64(header[1:], uint64(nbytes)) // #nosec G115 -- bench sizes are positive

	if _, err := conn.Write(header[:]); err != nil {
		return fmt.Errorf("header: %w", err)
	}

	buf := make([]byte, bufSize)

	switch mode {
	case modeRecv:
		received, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{conn}, buf)
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}

		if received != nbytes {
			return fmt.Errorf("%w: received %d of %d bytes", errShort, received, nbytes)
		}
	case modeSend:
		src := struct{ io.Reader }{io.LimitReader(zeroReader{}, nbytes)}
		if _, err := io.CopyBuffer(struct{ io.Writer }{conn}, src, buf); err != nil {
			return fmt.Errorf("send: %w", err)
		}

		var ack [1]byte
		if _, err := io.ReadFull(conn, ack[:]); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
	default:
		return fmt.Errorf("%w: mode %q", errShort, mode)
	}

	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}
