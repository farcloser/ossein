//go:build linux

package guestagent

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests cover the guest's one remaining consumer of image-controlled
// paths: writeRootfsFile, which the ConfigureDns/ConfigureHosts RPCs use to
// drop /etc/resolv.conf and /etc/hosts into a rootfs the IMAGE laid out.
//
// They were originally written against the tar extractor, which is gone along
// with the tar codec — the guest mounts an EROFS image now and unpacks
// nothing. The containment boundary they exercise (rootPath, openat2 with
// RESOLVE_IN_ROOT) is unchanged and still load-bearing, and this consumer is
// the one that was PROVEN reachable: an image shipping etc/resolv.conf as a
// symlink to an absolute guest path made the root-privileged agent write
// outside the rootfs on every plain `ossein run`.

// rootWithOutside returns a fresh rootfs directory plus a sibling directory
// that nothing under the root may ever reach.
func rootWithOutside(t *testing.T) (root, outside string) {
	t.Helper()

	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")

	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	return root, outside
}

// TestWriteRootfsFileFinalSymlinkIsReplacedNotFollowed is the regression test
// for the escape that was reproduced end to end: the image ships
// etc/resolv.conf as a symlink pointing outside the rootfs, and the agent —
// running as root — must replace it rather than write through it.
func TestWriteRootfsFileFinalSymlinkIsReplacedNotFollowed(t *testing.T) {
	t.Parallel()

	root, outside := rootWithOutside(t)

	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}

	victim := filepath.Join(outside, "resolv.conf")
	if err := os.Symlink(victim, filepath.Join(root, "etc/resolv.conf")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := writeRootfsFile(root, "etc/resolv.conf", "nameserver 1.1.1.1\n"); err != nil {
		t.Fatalf("writeRootfsFile: %v", err)
	}

	if _, err := os.Lstat(victim); err == nil {
		t.Fatal("ESCAPE: the write followed the symlink and landed outside the rootfs")
	}

	got, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if err != nil {
		t.Fatalf("resolv.conf should exist inside the rootfs: %v", err)
	}

	if string(got) != "nameserver 1.1.1.1\n" {
		t.Fatalf("content = %q", got)
	}

	// And it must be a real file now, not still a link.
	info, err := os.Lstat(filepath.Join(root, "etc/resolv.conf"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination is still a symlink: %v, mode %v", err, info.Mode())
	}
}

// TestWriteRootfsFileIntermediateSymlinkIsConfined covers the other half: an
// image-planted symlink at a PARENT component, pointing by absolute path at a
// real directory outside the rootfs. rootPath resolves parents through the
// kernel, so the write must not land there.
func TestWriteRootfsFileIntermediateSymlinkIsConfined(t *testing.T) {
	t.Parallel()

	root, outside := rootWithOutside(t)

	// "etc" itself is the attacker-controlled component.
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	// The write may succeed (redirected inside the root) or fail; what it may
	// never do is land outside.
	writeErr := writeRootfsFile(root, "etc/hosts", "127.0.0.1 localhost\n")

	if _, err := os.Lstat(filepath.Join(outside, "hosts")); err == nil {
		t.Fatal("ESCAPE: write landed outside the rootfs through an intermediate symlink")
	}

	if writeErr != nil {
		t.Logf("write was refused rather than redirected: %v (contained either way)", writeErr)
	}
}

// TestWriteRootfsFileAbsoluteSymlinkRedirectsIntoRoot is the sharp version:
// when the symlink's absolute target DOES exist inside the rootfs, the write
// is redirected there rather than to the host path of the same name. This is
// what "confined, not refused" buys, and it is why RESOLVE_IN_ROOT is used
// instead of RESOLVE_NO_SYMLINKS.
func TestWriteRootfsFileAbsoluteSymlinkRedirectsIntoRoot(t *testing.T) {
	t.Parallel()

	root, _ := rootWithOutside(t)

	for _, dir := range []string{"real", ""} {
		if dir == "" {
			continue
		}

		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// "etc -> /real", absolute, resolving to <root>/real under RESOLVE_IN_ROOT.
	if err := os.Symlink("/real", filepath.Join(root, "etc")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := writeRootfsFile(root, "etc/hosts", "127.0.0.1 localhost\n"); err != nil {
		t.Fatalf("writeRootfsFile: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(root, "real/hosts")); err != nil {
		t.Fatalf("write should have been redirected to <root>/real: %v", err)
	}
}

// TestWriteRootfsFileUsrMergeStillResolves guards the direction a
// stricter-looking fix would break: symlinked directories are FOLLOWED within
// the root, as usrmerge images require. Refusing symlink traversal outright
// would break every Debian-derived image.
func TestWriteRootfsFileUsrMergeStillResolves(t *testing.T) {
	t.Parallel()

	root, _ := rootWithOutside(t)

	if err := os.MkdirAll(filepath.Join(root, "usr/etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Symlink("usr/etc", filepath.Join(root, "etc")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := writeRootfsFile(root, "etc/hosts", "h\n"); err != nil {
		t.Fatalf("usrmerge-style write failed: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(root, "usr/etc/hosts")); err != nil {
		t.Fatalf("write did not resolve through etc -> usr/etc: %v", err)
	}

	// The symlink itself must survive as a symlink.
	info, err := os.Lstat(filepath.Join(root, "etc"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("etc should still be a symlink: %v, mode %v", err, info.Mode())
	}
}

// TestWriteRootfsFileRejectsDotDot pins the validation beside the containment
// boundary: ".." is malformed for this caller, so it is refused loudly rather
// than clamped silently. (Containment does not depend on this — the kernel
// keeps ".." at the root either way — which is exactly why it can be a plain
// input check.)
func TestWriteRootfsFileRejectsDotDot(t *testing.T) {
	t.Parallel()

	root, outside := rootWithOutside(t)

	if err := writeRootfsFile(root, "../outside/escaped", "x"); err == nil {
		t.Fatal("expected a .. path to be rejected")
	}

	if _, err := os.Lstat(filepath.Join(outside, "escaped")); err == nil {
		t.Fatal("ESCAPE: .. path was written outside the rootfs")
	}
}

// TestWriteRootfsFileCreatesMissingParents covers the ordinary case: a minimal
// image with no /etc at all still gets its resolv.conf.
func TestWriteRootfsFileCreatesMissingParents(t *testing.T) {
	t.Parallel()

	root, _ := rootWithOutside(t)

	if err := writeRootfsFile(root, "etc/resolv.conf", "nameserver 9.9.9.9\n"); err != nil {
		t.Fatalf("writeRootfsFile: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if err != nil || string(got) != "nameserver 9.9.9.9\n" {
		t.Fatalf("content = %q (%v)", got, err)
	}
}
