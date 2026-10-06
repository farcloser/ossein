//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/bytesize"
	"github.com/mycophonic/primordium/filesystem/pathcheck"

	"github.com/farcloser/ossein/internal/cli"
)

func TestValidateInstanceID(t *testing.T) {
	t.Parallel()

	// pathcheck's rules for a path component on this platform; a backslash is
	// an ordinary filename character on darwin and cannot leave the state root.
	bad := []string{"", ".", "..", "../../foo", "a/b", "/abs", " ", "a\x00b", strings.Repeat("a", 256)}
	for _, id := range bad {
		if err := validateInstanceID(id); err == nil {
			t.Errorf("validateInstanceID(%q) = nil, want error", id)
		}
	}

	good := []string{"abcdef123456", "doctor", "a-b_c.d", `a\b`}
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
	// BUILDKIT_HOST silently stops working after any `cd`. Climbing to the
	// root keeps the resolved path under the socket limit wherever the
	// checkout lives.
	cwd, _ := os.Getwd()
	rel := strings.Repeat("../", strings.Count(cwd, "/")) + "tmp/bk.sock"

	got, err = resolveSock(rel, dir)
	if err != nil {
		t.Fatalf("relative: %v", err)
	}

	if got != filepath.Join(cwd, rel) {
		t.Fatalf("relative --sock = %q, want %q, anchored at the caller's cwd", got, filepath.Join(cwd, rel))
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

func TestMemoryFlagTakesASizeWithAUnit(t *testing.T) {
	t.Parallel()

	parse := func(args ...string) (CLI, error) {
		var root CLI

		parser, err := kong.New(&root, kong.Vars{"buildkit_image": "img", "log_levels": cli.LogLevels})
		if err != nil {
			t.Fatal(err)
		}

		_, err = parser.Parse(args)

		return root, err
	}

	for _, check := range []struct {
		args []string
		want memorySize
	}{
		{[]string{"run", "img"}, 4 * bytesize.GiB},
		{[]string{"run", "--memory", "512MiB", "img"}, 512 * bytesize.MiB},
		{[]string{"run", "--memory", "1.5GB", "img"}, 1500 * bytesize.MB},
		{[]string{"run", "--memory", "0", "img"}, cli.WholeHost},
	} {
		root, err := parse(check.args...)
		if err != nil || root.Run.Memory != check.want {
			t.Errorf("%q: memory = (%d, %v), want %d", check.args, root.Run.Memory, err, check.want)
		}
	}

	if root, err := parse("buildkit"); err != nil || root.Buildkit.Memory != 8*bytesize.GiB {
		t.Errorf("buildkit: memory = (%d, %v), want 8 GiB", root.Buildkit.Memory, err)
	}

	// 512 meant MiB before --memory took units: never 512 bytes.
	if _, err := parse("run", "--memory", "512", "img"); !errors.Is(err, cli.ErrSizeUnit) {
		t.Errorf("--memory 512 = %v, want ErrSizeUnit", err)
	}
}

func TestResolveSockRefusesPastTheLimit(t *testing.T) {
	t.Parallel()

	// macOS bounds sun_path at 104 bytes, NUL included.
	limit := "/tmp/" + strings.Repeat("s", 98)

	got, err := resolveSock(limit, t.TempDir())
	if err != nil || got != limit {
		t.Fatalf("103-byte --sock = (%q, %v), want it accepted", got, err)
	}

	_, err = resolveSock(limit+"s", t.TempDir())
	if !errors.Is(err, errUsage) || !errors.Is(err, pathcheck.ErrInvalidPath) {
		t.Fatalf("104-byte --sock = %v, want errUsage wrapping ErrInvalidPath", err)
	}
}
