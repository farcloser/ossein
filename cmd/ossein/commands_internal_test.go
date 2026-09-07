//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateInstanceID(t *testing.T) {
	t.Parallel()

	bad := []string{"", ".", "..", "../../foo", `..\foo`, "a/b", `a\b`, "/abs"}
	for _, id := range bad {
		if err := validateInstanceID(id); err == nil {
			t.Errorf("validateInstanceID(%q) = nil, want error", id)
		}
	}

	good := []string{"abcdef123456", "doctor", "a-b_c.d"}
	for _, id := range good {
		if err := validateInstanceID(id); err != nil {
			t.Errorf("validateInstanceID(%q) = %v, want nil", id, err)
		}
	}
}

func TestRemoveInstanceDirOnReturn(t *testing.T) {
	t.Parallel()

	if !removeInstanceDirOnReturn(nil) {
		t.Error("clean return must remove the dir")
	}

	if !removeInstanceDirOnReturn(exitError{code: 3}) {
		t.Error("workload's own nonzero exit is a completed run — must remove the dir")
	}

	if !removeInstanceDirOnReturn(fmt.Errorf("wrapped: %w", exitError{code: 1})) {
		t.Error("wrapped exitError must still remove the dir")
	}

	// The whole point of the fix: a boot/expose failure whose message points
	// at console.log inside the dir must NOT delete it.
	if removeInstanceDirOnReturn(errors.New("boot: ... (console log: /path/console.log)")) {
		t.Error("failure return must keep the dir the error message points into")
	}
}

func TestResolveSock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	got, err := resolveSock("", dir)
	if err != nil {
		t.Fatalf("default: %v", err)
	}

	if got != filepath.Join(dir, hostBkSock) {
		t.Fatalf("default = %q, want it inside the instance dir", got)
	}

	// The fix: a relative flag must come back absolute, or the printed
	// BUILDKIT_HOST silently stops working after any `cd`.
	got, err = resolveSock("bk.sock", dir)
	if err != nil {
		t.Fatalf("relative: %v", err)
	}

	if !filepath.IsAbs(got) {
		t.Fatalf("relative --sock resolved to %q, want absolute", got)
	}

	cwd, _ := os.Getwd()
	if got != filepath.Join(cwd, "bk.sock") {
		t.Fatalf("relative --sock = %q, want anchored at the caller's cwd", got)
	}

	abs := filepath.Join(dir, "x.sock")
	if got, _ := resolveSock(abs, dir); got != abs {
		t.Fatalf("absolute --sock must pass through, got %q", got)
	}
}

// resetConn's Read fails with a non-timeout net.Error — the shape a broken
// proxy chain would produce on platforms where peer teardown surfaces as
// ECONNRESET. No real darwin AF_UNIX socket can produce it (reads yield clean
// EOF on every peer-close shape), hence the netDial seam.
type resetConn struct{ net.Conn }

func (resetConn) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "unix", Err: errors.New("connection reset by peer")}
}

func (resetConn) SetReadDeadline(time.Time) error { return nil }

func (resetConn) Close() error { return nil }

func TestProbeSocketChainReset(t *testing.T) { //nolint:paralleltest // swaps the package-level netDial seam
	orig := netDial

	t.Cleanup(func() { netDial = orig })

	netDial = func(_ context.Context, _ string) (net.Conn, error) {
		return resetConn{}, nil
	}

	// A reset is a broken chain, exactly like EOF: probeSocketChain must not
	// report ready just because the error satisfies net.Error.
	if probeSocketChain(t.Context(), "ignored") {
		t.Fatal("probeSocketChain: non-timeout net.Error (reset) probed as ready")
	}
}
