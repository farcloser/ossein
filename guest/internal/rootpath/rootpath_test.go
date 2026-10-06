//go:build linux

package rootpath_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/rootpath"
)

// resolvedFD is where fd points: what a mount or a connect through
// ProcFDPath(fd) would reach.
func resolvedFD(t *testing.T, fd int) string {
	t.Helper()

	target, err := os.Readlink(rootpath.ProcFDPath(fd))
	if err != nil {
		t.Fatal(err)
	}

	return target
}

func openRoot(t *testing.T) (string, *rootpath.Root) {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	root, err := rootpath.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	return dir, root
}

// An image ships "data -> /sbin": the directory must be created, and handed
// back, under the rootfs's own sbin, never the VM's.
func TestDirFollowsAnAbsoluteSymlinkInsideTheRoot(t *testing.T) {
	t.Parallel()

	dir, root := openRoot(t)

	if err := os.Mkdir(filepath.Join(dir, "sbin"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("/sbin", filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}

	fd, err := root.Dir("/data/sub")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = unix.Close(fd) }()

	if got, want := resolvedFD(t, fd), filepath.Join(dir, "sbin", "sub"); got != want {
		t.Fatalf("Dir(/data/sub) resolved to %q, want %q", got, want)
	}
}

func TestDirKeepsDotDotInsideTheRoot(t *testing.T) {
	t.Parallel()

	dir, root := openRoot(t)

	if err := os.Mkdir(filepath.Join(dir, "escape"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("../../../../escape", filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"/../../x": filepath.Join(dir, "x"),
		"/up":      filepath.Join(dir, "escape"),
	} {
		fd, err := root.Dir(name)
		if err != nil {
			t.Fatalf("Dir(%q): %v", name, err)
		}

		if got := resolvedFD(t, fd); got != want {
			t.Errorf("Dir(%q) resolved to %q, want %q", name, got, want)
		}

		_ = unix.Close(fd)
	}
}

func TestOpenPathFollowsTheLastComponentInsideTheRoot(t *testing.T) {
	t.Parallel()

	dir, root := openRoot(t)

	if err := os.WriteFile(filepath.Join(dir, "target"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("/target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	fd, err := root.OpenPath("link")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = unix.Close(fd) }()

	if got, want := resolvedFD(t, fd), filepath.Join(dir, "target"); got != want {
		t.Fatalf("OpenPath(link) resolved to %q, want %q", got, want)
	}
}

// A dangling absolute symlink is not created through: Dir refuses it, and
// nothing appears at the path it names outside the root.
func TestDirRefusesADanglingSymlinkAndCreatesNothingOutside(t *testing.T) {
	t.Parallel()

	dir, root := openRoot(t)
	outside := filepath.Join(t.TempDir(), "outside")

	if err := os.Symlink(outside, filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}

	if fd, err := root.Dir("/data/sub"); err == nil {
		got := resolvedFD(t, fd)
		_ = unix.Close(fd)

		t.Fatalf("Dir through a dangling symlink resolved to %q, want an error", got)
	}

	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("Dir created %s outside the root: %v", outside, err)
	}
}
