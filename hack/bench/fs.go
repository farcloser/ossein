//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mycophonic/primordium/bytesize"
	"github.com/mycophonic/primordium/filesystem"
)

// fs's two measurements (see runFS).
const (
	opStat = "stat"
	opRead = "read"
)

// runFS isolates virtio-fs performance from the fork/exec path: it times pure file
// operations on a target that lives either on the virtio-fs share (virtio mode:
// operate on the binary in place, where it was launched) or on the container rootfs
// (rootfs mode: copy self to tmp, operate there). The virtio − rootfs delta is the
// isolated virtio-fs overhead, with no exec/fault noise — the clean metric for how
// the runtime's file share performs.
//
//	fs <virtio|rootfs> stat [N]  open+fstat+close latency — the per-open metadata
//	                             round-trip that makes execve slow.
//	fs <virtio|rootfs> read [N]  open+read-whole-file+close — data path. Also reveals
//	                             caching: if virtio >> rootfs, the guest is not
//	                             caching file data across opens.
func runFS(args []string) error {
	mode, operation, iterations := fsArgs(args)

	target, cleanup, err := fsTarget(mode)
	if err != nil {
		return err
	}
	defer cleanup()

	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat target: %w", err)
	}

	size := fi.Size()

	step := statOnce

	if operation == opRead {
		buf := make([]byte, bytesize.MiB)
		step = func(path string) error { return readAll(path, buf) }
	}

	start := time.Now()

	for range iterations {
		if err = step(target); err != nil {
			return err
		}
	}

	elapsed := time.Since(start)

	perop := float64(elapsed.Microseconds()) / float64(iterations)
	if operation == opRead {
		mibps := float64(size) * float64(iterations) / elapsed.Seconds() / bytesize.MiB
		_, _ = fmt.Fprintf(os.Stdout, "fs/%s/read=%d size=%d total=%s perop=%.2fus (%.0f MiB/s)\n",
			mode, iterations, size, elapsed, perop, mibps)

		return nil
	}

	_, _ = fmt.Fprintf(os.Stdout, "fs/%s/stat=%d total=%s perop=%.2fus (%.0f ops/s)\n",
		mode, iterations, elapsed, perop, float64(iterations)/elapsed.Seconds())

	return nil
}

// fsArgs reads fs's arguments: [virtio|rootfs] [stat|read] [N].
func fsArgs(args []string) (mode, operation string, iterations int64) {
	mode = modeVirtio
	if len(args) > 0 && (args[0] == modeVirtio || args[0] == modeRootfs) {
		mode = args[0]
		args = args[1:]
	}

	operation = opStat
	if len(args) > 0 && (args[0] == opStat || args[0] == opRead) {
		operation = args[0]
		args = args[1:]
	}

	const defaultIterations = 20000

	iterations = defaultIterations

	if operation == opRead {
		iterations = 2000 // reads move real bytes; fewer iterations
	}

	if len(args) > 0 {
		if v, err := strconv.ParseInt(args[0], 10, 64); err == nil && v > 0 {
			iterations = v
		}
	}

	return mode, operation, iterations
}

// fsTarget is the file fs measures: this binary in place on the virtio-fs
// share it was launched from, or, in rootfs mode, a copy staged in tmp.
// cleanup removes the copy.
func fsTarget(mode string) (target string, cleanup func(), err error) {
	self, err := os.Executable() // resolves /proc/self/exe -> the real path
	if err != nil {
		return "", nil, fmt.Errorf("executable: %w", err)
	}

	if mode != modeRootfs {
		return self, func() {}, nil
	}

	target = filepath.Join(os.TempDir(), "fs-target")
	if err = copyFile(self, target); err != nil {
		return "", nil, fmt.Errorf("stage rootfs target: %w", err)
	}

	return target, func() { _ = os.Remove(target) }, nil
}

// statOnce is the per-open metadata round-trip: open, fstat, close.
func statOnce(path string) error {
	// G304: path is runFS's target — os.Executable() or our staged copy.
	file, err := os.Open(path) // #nosec G304 -- see above
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	_, err = file.Stat()
	_ = file.Close()

	if err != nil {
		return fmt.Errorf("fstat: %w", err)
	}

	return nil
}

func readAll(path string, buf []byte) error {
	// G304: path is runFS's target — os.Executable() or our staged copy.
	file, err := os.Open(path) // #nosec G304 -- see above
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = file.Close() }()

	for {
		if _, err := file.Read(buf); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("read: %w", err)
		}
	}
}

// copyFile stages a copy of this binary for the rootfs-mode measurements. Both
// paths are the benchmark's own — src is os.Executable(), dst is a fixed name
// under os.TempDir() — so gosec's file-inclusion (G304) and taint-based path
// traversal (G703) warnings have no external input to act on.
func copyFile(src, dst string) error {
	content, err := os.ReadFile(src) // #nosec G304 -- see above
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	// 0600, not 0644: unlike copyExe's target this one is only stat'd and read
	// back by this same process, so it never needs to be executable OR readable
	// by anyone else. Least privilege, and it satisfies G306 outright.
	// #nosec G703 -- see above
	if err := os.WriteFile(dst, content, filesystem.FilePermissionsPrivate); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	return nil
}
