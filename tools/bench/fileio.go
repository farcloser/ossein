//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// runFileio measures create+write+close of many small files — the shape of ossein's
// dominant real cost, rootfs extraction (thousands of small files: open/write/close
// plus fs metadata work). It writes into the container's default tmp filesystem, so
// across runtimes it also reflects the storage backend (ossein tmpfs vs docker
// overlayfs) — a real, representative difference.
func runFileio(args []string) error {
	files := int64(50000)

	if len(args) > 0 {
		if v, err := strconv.ParseInt(args[0], 10, 64); err == nil && v > 0 {
			files = v
		}
	}

	dir, err := os.MkdirTemp("", "fileio")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	data := make([]byte, 256) // small file, like most rootfs/source entries

	start := time.Now()

	for i := range files {
		p := filepath.Join(dir, strconv.FormatInt(i, 10))

		// G304: p is our own MkdirTemp directory joined with a loop counter —
		// nothing here comes from outside the process.
		file, err := os.Create(p) // #nosec G304 -- see above
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}

		if _, err := file.Write(data); err != nil {
			_ = file.Close()

			return fmt.Errorf("write: %w", err)
		}

		_ = file.Close()
	}

	elapsed := time.Since(start)

	_, _ = fmt.Fprintf(os.Stdout, "fileio=%d total=%s perop=%.2fus (%.0f files/s)\n",
		files, elapsed, float64(elapsed.Microseconds())/float64(files), float64(files)/elapsed.Seconds())

	return nil
}
