//go:build linux

package guestagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/vmexec"
	"github.com/farcloser/ossein/internal/rootfsblob"
)

// TestSniffRootfsBlob exercises the real dispatch materializeRootfs runs
// before it mounts anything: open the blob, read its head, classify. This is
// the only thing standing between a cache that served the wrong bytes and a
// bare EINVAL from the kernel naming nothing.
//
// A regular file stands in for the blob device — sniffRootfsBlob only opens
// and reads, and the classifier itself is covered on the host in
// internal/rootfsblob.
func TestSniffRootfsBlob(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, name string, data []byte) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		return path
	}

	erofs := make([]byte, rootfsblob.SniffLen+4096)
	copy(erofs[rootfsblob.EROFSSuperOffset:], []byte{0xE2, 0xE1, 0xF5, 0xE0})

	for _, testCase := range []struct {
		name string
		data []byte
		want rootfsblob.Format
	}{
		{"erofs image", erofs, rootfsblob.FormatEROFS},
		{"lz4 stream", append([]byte{0x04, 0x22, 0x4D, 0x18}, make([]byte, 512)...), rootfsblob.FormatLZ4},
		// Shorter than a superblock: must classify, not fail, and not panic
		// on the short read.
		{"truncated blob", make([]byte, 16), rootfsblob.FormatTar},
		{"empty blob", nil, rootfsblob.FormatTar},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := sniffRootfsBlob(write(t, "blob", testCase.data))
			if err != nil {
				t.Fatalf("sniffRootfsBlob: %v", err)
			}

			if got != testCase.want {
				t.Fatalf("format = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestSniffRootfsBlobMissingDevice pins the failure shape: a blob device that
// never appears must surface as an error naming the path, not as a
// misclassification that then fails at mount.
func TestSniffRootfsBlobMissingDevice(t *testing.T) {
	t.Parallel()

	if _, err := sniffRootfsBlob(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a device that does not exist")
	}
}

// TestOverlayDataLayout pins the option string both materialization paths
// build. overlayfs refuses a stack whose work dir is not an empty sibling of
// the upper on the same filesystem, so the shape is a contract, not a detail.
func TestOverlayDataLayout(t *testing.T) {
	t.Parallel()

	got := overlayData("/run/container/bc-1/rootfs", "/run/container/bc-1/rootfs/lower")
	want := "lowerdir=/run/container/bc-1/rootfs/lower," +
		"upperdir=/run/container/bc-1/rootfs/upper," +
		"workdir=/run/container/bc-1/rootfs/work"

	if got != want {
		t.Fatalf("overlayData =\n%s\nwant\n%s", got, want)
	}
}

// mountTmpfs mounts a tmpfs at a fresh directory with the given options,
// unmounting it on cleanup, and returns the mountpoint with the flags the
// option string parsed to — the same translation materializeRootfs applies to
// the boot plan.
func mountTmpfs(t *testing.T, opts string) (string, uintptr) {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(dest, stdDirMode); err != nil {
		t.Fatalf("mkdir %s: %v", dest, err)
	}

	flags, data := vmexec.ParseMountOptions(strings.Split(opts, ","))
	if err := unix.Mount("tmpfs", dest, "tmpfs", flags, data); err != nil {
		t.Fatalf("mount tmpfs on %s: %v", dest, err)
	}

	t.Cleanup(func() { _ = unix.Unmount(dest, unix.MNT_DETACH) })

	return dest, flags
}

// plantDeviceInLower creates the overlay dirs under dest and puts a character
// device in the lower — standing in for one an image shipped.
func plantDeviceInLower(t *testing.T, dest string) string {
	t.Helper()

	if err := makeOverlayDirs(dest); err != nil {
		t.Fatalf("overlay dirs: %v", err)
	}

	node := filepath.Join(dest+overlayLowerDir, "zero")
	if err := unix.Mknod(node, unix.S_IFCHR|0o666, int(unix.Mkdev(1, 5))); err != nil {
		t.Fatalf("mknod: %v", err)
	}

	return node
}

// TestStackOverlayHonoursMountFlags is the security regression test for the
// DEFAULT boot path. pkg/container puts nodev in the rootfs mount options
// because an image can ship a device node — under EROFS the host writer puts
// it straight into the filesystem image, so no mknod call is needed — and the
// container root it has to protect is the merged OVERLAY, not the tmpfs
// underneath. Mounting the overlay without the plan's flags would bring every
// such node back to life while the tmpfs below still looked hardened.
//
// A tmpfs lower stands in for the EROFS one: the invariant under test is the
// flag threading through stackOverlay, which is identical either way, and this
// keeps the test runnable without an image to mount.
//
//nolint:paralleltest // mounts into the process mount namespace; running these concurrently is not the point
func TestStackOverlayHonoursMountFlags(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount")
	}

	t.Run("nodev makes an image-supplied node inert", func(t *testing.T) {
		dest, flags := mountTmpfs(t, "mode=0755,nodev,size=64m")
		plantDeviceInLower(t, dest)

		if err := stackOverlay(dest, dest+overlayLowerDir, flags); err != nil {
			t.Fatalf("stackOverlay: %v", err)
		}

		t.Cleanup(func() { _ = unix.Unmount(dest, unix.MNT_DETACH) })

		// The node is visible through the merged overlay...
		merged := filepath.Join(dest, "zero")
		if _, err := os.Lstat(merged); err != nil {
			t.Fatalf("node not visible through the overlay: %v", err)
		}

		// ...but opening it must fail: nodev reached the overlay superblock.
		file, err := os.OpenFile(merged, os.O_RDONLY, 0)
		if err == nil {
			_ = file.Close()

			t.Fatal("ESCAPE: an image-supplied device node is live through the overlay; " +
				"stackOverlay dropped the rootfs mount flags")
		}
	})

	// The control. Without it the test above would still pass if opening a
	// character device failed for some unrelated reason, and would have
	// stopped testing anything without saying so.
	t.Run("control: without nodev the same node is live", func(t *testing.T) {
		dest, flags := mountTmpfs(t, "mode=0755,size=64m")
		plantDeviceInLower(t, dest)

		if err := stackOverlay(dest, dest+overlayLowerDir, flags); err != nil {
			t.Fatalf("stackOverlay: %v", err)
		}

		t.Cleanup(func() { _ = unix.Unmount(dest, unix.MNT_DETACH) })

		file, err := os.OpenFile(filepath.Join(dest, "zero"), os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("without nodev the node should open (the nodev case proves nothing otherwise): %v", err)
		}

		_ = file.Close()
	})
}

// TestStackOverlayIsWritableOverAReadOnlyLower pins the property the whole
// EROFS design rests on: the image layer is immutable, yet the container gets
// a writable root. A stack that came out read-only would break every workload
// while looking like a successful mount at boot.
//
//nolint:paralleltest // see TestStackOverlayHonoursMountFlags: this mounts.
func TestStackOverlayIsWritableOverAReadOnlyLower(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount")
	}

	dest, flags := mountTmpfs(t, "mode=0755,nodev,size=64m")

	if err := makeOverlayDirs(dest); err != nil {
		t.Fatalf("overlay dirs: %v", err)
	}

	lower := dest + overlayLowerDir
	if err := os.WriteFile(filepath.Join(lower, "from-image"), []byte("IMAGE"), 0o644); err != nil {
		t.Fatalf("seed lower: %v", err)
	}

	// Make the lower read-only, as the EROFS device mount is. A directory can
	// only be remounted read-only if it is a mount point, so bind it over
	// itself first.
	if err := unix.Mount(lower, lower, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind lower: %v", err)
	}

	t.Cleanup(func() { _ = unix.Unmount(lower, unix.MNT_DETACH) })

	if err := unix.Mount("", lower, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		t.Fatalf("remount lower read-only: %v", err)
	}

	// The overlay is about to be mounted over dest, shadowing the tmpfs
	// upper/lower dirs the assertions below inspect. Bind the tmpfs to a side
	// window first so they stay reachable.
	window := filepath.Join(t.TempDir(), "window")
	if err := os.MkdirAll(window, stdDirMode); err != nil {
		t.Fatalf("mkdir window: %v", err)
	}

	if err := unix.Mount(dest, window, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind window: %v", err)
	}

	t.Cleanup(func() { _ = unix.Unmount(window, unix.MNT_DETACH) })

	if err := stackOverlay(dest, lower, flags); err != nil {
		t.Fatalf("stackOverlay: %v", err)
	}

	t.Cleanup(func() { _ = unix.Unmount(dest, unix.MNT_DETACH) })

	// Image content reads through.
	got, err := os.ReadFile(filepath.Join(dest, "from-image"))
	if err != nil || string(got) != "IMAGE" {
		t.Fatalf("image file through the overlay = %q (%v), want IMAGE", got, err)
	}

	// A new file lands in the upper, not the read-only lower.
	if err := os.WriteFile(filepath.Join(dest, "written"), []byte("NEW"), 0o644); err != nil {
		t.Fatalf("write through overlay: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(window+overlayUpperDir, "written")); err != nil {
		t.Fatalf("write did not land in the upper: %v", err)
	}

	// And copy-up works: modifying an image file must not fail against the
	// read-only lower.
	if err := os.WriteFile(filepath.Join(dest, "from-image"), []byte("EDITED"), 0o644); err != nil {
		t.Fatalf("copy-up of an image file failed: %v", err)
	}

	got, err = os.ReadFile(filepath.Join(dest, "from-image"))
	if err != nil || string(got) != "EDITED" {
		t.Fatalf("after copy-up = %q (%v), want EDITED", got, err)
	}

	// The lower saw none of it: the new file is absent and the image file
	// still carries its original bytes — the immutability half of the pinned
	// property.
	if _, err := os.Lstat(filepath.Join(window+overlayLowerDir, "written")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lower has the new file (lstat err %v); writes are reaching the image layer", err)
	}

	got, err = os.ReadFile(filepath.Join(window+overlayLowerDir, "from-image"))
	if err != nil || string(got) != "IMAGE" {
		t.Fatalf("lower's from-image = %q (%v), want the original IMAGE", got, err)
	}
}
