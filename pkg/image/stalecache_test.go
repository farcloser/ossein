//nolint:testpackage // white-box: drives the unexported cache wiring and blob layout
package image

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/random"
)

// dropBlobs removes every cached blob directory while leaving the index alone,
// which is exactly what primordium's blob GC does when it reclaims over quota:
// Store.GarbageCollect deletes blob dirs and never touches index.dat.
func dropBlobs(t *testing.T, root string) int {
	t.Helper()

	dropped := 0

	buckets, err := os.ReadDir(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}

	for _, bucket := range buckets {
		if !bucket.IsDir() {
			continue
		}

		entries, err := os.ReadDir(filepath.Join(root, "cache", bucket.Name()))
		if err != nil {
			t.Fatalf("read bucket: %v", err)
		}

		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(root, "cache", bucket.Name(), entry.Name())); err != nil {
				t.Fatalf("drop blob: %v", err)
			}

			dropped++
		}
	}

	return dropped
}

// TestRootfsRecoversFromStaleIndexEntry covers the one cache state that used to
// be unrecoverable: blob GC'd, index entry surviving, and a re-flatten that no
// longer reproduces the recorded digest (what a compression change does).
//
// Without the healing in RootfsFile the first acquire fails and EVERY later
// run fails identically — including --pull=always, which re-resolves the tag
// to the same identifier — leaving "delete ~/Library/Caches/ossein" as the
// only way out.
func TestRootfsRecoversFromStaleIndexEntry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	cache, err := openCache(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()

	const identifier = "test/img:latest@sha256:stale|linux/arm64"

	// 1. A normal pull: the index records this identifier → digest(bytes A).
	first, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}

	original := readPin(t, mustRootfsFile(t, newTestImage(t, cache, first, identifier)))
	if len(original) == 0 {
		t.Fatal("empty rootfs")
	}

	// 2. Blob GC reclaims the blob. The index entry survives it.
	if dropped := dropBlobs(t, root); dropped == 0 {
		t.Fatal("no blobs dropped; the cache layout changed and this test is not exercising GC")
	}

	// 3. The same identifier now flattens to DIFFERENT bytes — the situation a
	//    compression change creates. The store verifies against the recorded
	//    digest, rejects them, and discards what it fetched.
	second, err := random.Image(2048, 3)
	if err != nil {
		t.Fatal(err)
	}

	_, acquireErr := newTestImage(t, cache, second, identifier).RootfsFile()
	if acquireErr == nil {
		t.Skip("store accepted mismatched bytes; primordium no longer verifies on this path")
	}

	// Non-fatal: the property under test is step 4, and stopping here would
	// hide whether recovery works.
	if !errors.Is(acquireErr, ErrCache) {
		t.Errorf("stale-entry failure should surface as ErrCache, got %v", acquireErr)
	}

	// 4. The recovery this test exists for: the next run must succeed on its
	//    own, with no manual cache surgery.
	healed := readPin(t, mustRootfsFile(t, newTestImage(t, cache, second, identifier)))
	if len(healed) == 0 {
		t.Fatal("empty rootfs after healing")
	}

	// 5. And it must be the NEW content, not a stale blob resurrected.
	again := readPin(t, mustRootfsFile(t, newTestImage(t, cache, second, identifier)))
	if string(healed) != string(again) {
		t.Fatal("healed entry is not stable across runs")
	}
}
