// Package guestartifacts serves the guest boot artifacts (the Linux kernel and
// the vminitd initfs) that ossein hands to Virtualization.framework.
//
// Both are EMBEDDED into the ossein binary at build time and EXTRACTED to a
// content-addressed cache on first use (VZ's bootloader/disk APIs take file paths,
// not bytes). `just build` fetches + cosign/sha verifies the pinned ossein-kernel
// release and builds the initfs, then `go build` bundles them via //go:embed — so
// trust is settled at build time and the runtime carries no network, cosign, or
// GitHub dependency. A fresh binary's artifacts hash differently and land in a new
// cache dir, so they never collide with a previous build's.
package guestartifacts

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mycophonic/primordium/digest"
	"github.com/mycophonic/primordium/filesystem"
)

//go:embed kernel-arm64
var kernelBytes []byte

//go:embed initfs.cpio
var initfsBytes []byte

// kernelDigest / initfsDigest may be stamped at build time via -ldflags -X.
// When empty (the normal case — limen's shared build recipe owns ldflags), the
// digest is computed once per BINARY, not per run: a memo in the cache root,
// keyed by the executable's stat identity, makes every run after the first a
// stat instead of a ~24 MB BLAKE2b pass on the boot-latency-critical path.
//
//nolint:gochecknoglobals // -ldflags -X targets require package-level vars
var (
	kernelDigest string
	initfsDigest string
)

const (
	kernelName = "kernel-arm64"
	initfsName = "initfs.cpio"

	// cacheKeyLen is how many hex chars of the content BLAKE2b-256 digest name the
	// cache dir — 64 bits, collision-free across builds without an unwieldy path.
	cacheKeyLen = 16
)

// Kernel extracts the embedded guest kernel to a content-addressed cache path under
// cacheRoot and returns it. Idempotent: a matching cached file is reused.
func Kernel(cacheRoot string) (string, error) {
	return materialize(cacheRoot, kernelName, kernelBytes, kernelDigest)
}

// Initfs extracts the embedded vminitd initfs and returns its cache path.
func Initfs(cacheRoot string) (string, error) {
	return materialize(cacheRoot, initfsName, initfsBytes, initfsDigest)
}

// materialize writes data to cacheRoot/<key>/<name> if not already present, and
// returns the path. filesystem.WriteFile is atomic (temp + rename), so concurrent
// ossein invocations racing to extract the same artifact are safe. Stale sibling
// dirs left by previous builds of this artifact are pruned best-effort.
func materialize(cacheRoot, name string, data []byte, stampedDigest string) (string, error) {
	key := stampedDigest
	if len(key) < cacheKeyLen {
		key = memoizedDigest(cacheRoot, name, data)
	}

	key = key[:cacheKeyLen]
	dir := filepath.Join(cacheRoot, key)
	path := filepath.Join(dir, name)

	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(data)) {
		return path, nil // already extracted
	}

	if err := os.MkdirAll(dir, filesystem.DirPermissionsPrivate); err != nil {
		return "", fmt.Errorf("cache dir: %w", err)
	}

	if err := filesystem.WriteFile(path, data, filesystem.FilePermissionsPrivate); err != nil {
		return "", fmt.Errorf("extract %s: %w", name, err)
	}

	pruneStale(cacheRoot, name, key)

	return path, nil
}

// memoizedDigest returns the BLAKE2b-256 hex digest of data, memoized per
// binary in cacheRoot/.digest-<name>. The memo key is the running executable's
// path+size+mtime, so a rebuilt binary re-hashes exactly once. The memo is
// purely advisory: a wrong digest merely names a different cache dir, into
// which the correct embedded bytes are then extracted — content is never
// trusted from the memo.
func memoizedDigest(cacheRoot, name string, data []byte) string {
	memoPath := filepath.Join(cacheRoot, ".digest-"+name)
	binKey := executableKey()

	if binKey != "" {
		// #nosec G304 -- advisory memo under the ossein-owned cache root
		if memo, err := os.ReadFile(memoPath); err == nil {
			// The digest field must be long enough for materialize's
			// key[:cacheKeyLen] slice: a truncated or corrupt memo must fall
			// through to re-hashing, never panic every VM-booting command.
			if fields := strings.Fields(string(memo)); len(fields) == 2 && fields[0] == binKey &&
				len(fields[1]) >= cacheKeyLen {
				return fields[1]
			}
		}
	}

	hasher := digest.BLAKE2b256.Hash()
	_, _ = hasher.Write(data) // hash.Hash.Write never errors
	sum := hex.EncodeToString(hasher.Sum(nil))

	if binKey != "" {
		if err := os.MkdirAll(cacheRoot, filesystem.DirPermissionsPrivate); err == nil {
			_ = filesystem.WriteFile(memoPath, []byte(binKey+" "+sum), filesystem.FilePermissionsPrivate)
		}
	}

	return sum
}

// executableKey identifies this binary build: path plus size plus mtime.
// Empty when the executable cannot be stat'ed — memoization is then skipped.
func executableKey() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}

	info, err := os.Stat(exe)
	if err != nil {
		return ""
	}

	return fmt.Sprintf("%s|%d|%d", exe, info.Size(), info.ModTime().UnixNano())
}

// pruneStale removes cache dirs holding an OLD build's copy of this artifact
// (identified by containing a file of the same name under a different key), so
// upgrades don't accumulate ~24 MB per build forever. Best-effort by design:
// a concurrently running older binary re-extracts on its next boot in the
// narrow window between its stat and VZ opening the path.
func pruneStale(cacheRoot, name, currentKey string) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == currentKey {
			continue
		}

		stale := filepath.Join(cacheRoot, entry.Name())
		if _, err := os.Stat(filepath.Join(stale, name)); err != nil {
			continue // not this artifact's dir (or already gone)
		}

		_ = os.RemoveAll(stale)
	}
}
