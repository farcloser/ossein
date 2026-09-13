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
	mode := "virtio"
	if len(args) > 0 && (args[0] == "virtio" || args[0] == "rootfs") {
		mode = args[0]
		args = args[1:]
	}

	operation := "stat"
	if len(args) > 0 && (args[0] == "stat" || args[0] == "read") {
		operation = args[0]
		args = args[1:]
	}

	iterations := int64(20000)
	if operation == "read" {
		iterations = 2000 // reads move real bytes; fewer iterations
	}

	if len(args) > 0 {
		if v, err := strconv.ParseInt(args[0], 10, 64); err == nil && v > 0 {
			iterations = v
		}
	}

	self, err := os.Executable() // resolves /proc/self/exe -> the real path
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}

	target := self // virtio: the binary in place, on the share it was launched from
	if mode == "rootfs" {
		target = filepath.Join(os.TempDir(), "fs-target")
		if err := copyFile(self, target); err != nil {
			return fmt.Errorf("stage rootfs target: %w", err)
		}
		defer func() { _ = os.Remove(target) }()
	}

	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat target: %w", err)
	}

	size := fi.Size()
	buf := make([]byte, 1<<20)

	start := time.Now()

	switch operation {
	case "read":
		for range iterations {
			if err := readAll(target, buf); err != nil {
				return err
			}
		}
	default: // stat: open + fstat + close, the per-open metadata round-trip
		for range iterations {
			// G304: target is os.Executable() or our own staged copy of it.
			file, err := os.Open(target) // #nosec G304 -- see above
			if err != nil {
				return fmt.Errorf("open: %w", err)
			}

			if _, err := file.Stat(); err != nil {
				_ = file.Close()

				return fmt.Errorf("fstat: %w", err)
			}

			_ = file.Close()
		}
	}

	elapsed := time.Since(start)

	perop := float64(elapsed.Microseconds()) / float64(iterations)
	if operation == "read" {
		mibps := float64(size) * float64(iterations) / elapsed.Seconds() / (1 << 20)
		_, _ = fmt.Fprintf(os.Stdout, "fs/%s/read=%d size=%d total=%s perop=%.2fus (%.0f MiB/s)\n",
			mode, iterations, size, elapsed, perop, mibps)

		return nil
	}

	_, _ = fmt.Fprintf(os.Stdout, "fs/%s/stat=%d total=%s perop=%.2fus (%.0f ops/s)\n",
		mode, iterations, elapsed, perop, float64(iterations)/elapsed.Seconds())

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
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	return nil
}
