//go:build linux

package vmexec

import (
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/rootpath"
)

// An image ships "data -> /sbin": a mount at /data must land on the rootfs's
// own sbin. Before, it landed on the VM's /sbin. A link naming a directory
// outside the rootfs by its real path must not be mounted through at all.
//
//nolint:paralleltest // mounts into the process mount namespace; running these concurrently is not the point
func TestMountIntoStaysInsideTheRootfs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount")
	}

	rootfs, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A file in each directory a mount could cover: a tmpfs mounted on top
	// hides it, so its visibility says where the mount went.
	marker := func(dir string) string {
		if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
			t.Fatal(mkErr)
		}

		path := filepath.Join(dir, "marker")
		if writeErr := os.WriteFile(path, nil, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}

		return path
	}

	insideMarker := marker(filepath.Join(rootfs, "sbin"))
	outside := filepath.Join(t.TempDir(), "outside")
	outsideMarker := marker(outside)

	if err = os.Symlink("/sbin", filepath.Join(rootfs, "data")); err != nil {
		t.Fatal(err)
	}

	if err = os.Symlink(outside, filepath.Join(rootfs, "escape")); err != nil {
		t.Fatal(err)
	}

	root, err := rootpath.Open(rootfs)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	tmpfs := func(dest string) specs.Mount { return specs.Mount{Type: "tmpfs", Source: "tmpfs", Destination: dest} }

	if err = mountInto(root, tmpfs("/data")); err != nil {
		t.Fatalf("mount at /data: %v", err)
	}

	t.Cleanup(func() { _ = unix.Unmount(filepath.Join(rootfs, "sbin"), unix.MNT_DETACH) })

	if _, err = os.Stat(insideMarker); !os.IsNotExist(err) {
		t.Fatalf("the rootfs's sbin is not covered by the /data mount: stat marker = %v", err)
	}

	if err = mountInto(root, tmpfs("/escape")); err == nil {
		_ = unix.Unmount(outside, unix.MNT_DETACH)

		t.Fatal("a mount through a link to a real directory outside the rootfs succeeded")
	}

	if _, err = os.Stat(outsideMarker); err != nil {
		t.Fatalf("the directory outside the rootfs was covered: stat marker = %v", err)
	}
}
