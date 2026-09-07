//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// runForkexec measures fork+exec+wait round-trips — the operation ossein does on
// every container launch and buildkit hammers on every build step. It spawns a
// "noop" copy of the bench binary that exits immediately (see the noop fast-path in
// main.go), so the measurement is pure process spawn + reap, not dynamic-linker
// startup. Unlike getpid, this traverses the *complex* kernel paths (clone, mm
// setup, exec, exit, wait) with real stack frames.
//
// Two modes select where the exec target lives, because that dominates the cost:
//
//	rootfs (default) — copy self to the container-local tmp fs and exec that. The
//	                   representative container case: entrypoints exec off the rootfs
//	                   (tmpfs for ossein, overlayfs for docker).
//	virtio           — exec /proc/self/exe in place, i.e. from wherever the binary
//	                   was launched. When that is a virtio-fs share (-v), each spawn
//	                   re-opens/faults the binary across the share, isolating
//	                   virtio-fs execve latency (host metadata round-trips).
//
// Report the two numbers side by side: rootfs is the container hot path; virtio is
// the cost of execing a binary off a bind share. Do not compare one runtime's rootfs
// number against another's virtio number — that mismatch hides a real virtio-fs gap.
func runForkexec(args []string) error {
	mode := "rootfs"

	rest := args
	if len(rest) > 0 && (rest[0] == "rootfs" || rest[0] == "virtio") {
		mode = rest[0]
		rest = rest[1:]
	}

	spawns := int64(20000)

	if len(rest) > 0 {
		if v, err := strconv.ParseInt(rest[0], 10, 64); err == nil && v > 0 {
			spawns = v
		}
	}

	var target string

	switch mode {
	case "virtio":
		// exec the binary in place — across the share it was launched from.
		target = "/proc/self/exe"
	default: // rootfs
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("executable: %w", err)
		}

		target = filepath.Join(os.TempDir(), "forkexec-noop")
		if err := copyExe(self, target); err != nil {
			return fmt.Errorf("stage exec target: %w", err)
		}

		defer func() { _ = os.Remove(target) }()
	}

	start := time.Now()

	for range spawns {
		// exec.Command, NOT CommandContext: this loop IS the measurement, and
		// CommandContext starts an extra goroutine per command to watch the
		// context — perturbing the very fork/exec cost being timed.
		// G204: target is either /proc/self/exe or the copy this function just
		// staged in TempDir — never external input.
		// #nosec G204 -- see above
		cmd := exec.Command(target, "noop") //nolint:noctx // see above
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("spawn failed: %w", err)
		}
	}

	elapsed := time.Since(start)

	_, _ = fmt.Fprintf(os.Stdout, "forkexec/%s=%d total=%s perop=%.2fus (%.0f spawns/s)\n",
		mode, spawns, elapsed,
		float64(elapsed.Microseconds())/float64(spawns), float64(spawns)/elapsed.Seconds())

	return nil
}

// copyExe stages a copy of this binary to exec from the container rootfs. The
// copy must be EXECUTABLE, which no mode gosec accepts (G306 caps at 0600), so
// the suppression is unavoidable — 0700 is the least-privilege mode that still
// runs: owner-only, no group or other bits.
func copyExe(src, dst string) error {
	content, err := os.ReadFile(src) // #nosec G304 -- src is os.Executable(), this very binary
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	// #nosec G306 G703 -- see above
	if err := os.WriteFile(dst, content, 0o700); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	return nil
}
