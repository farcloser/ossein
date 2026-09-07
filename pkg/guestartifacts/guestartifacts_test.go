//nolint:testpackage // exercises unexported materialize/memoizedDigest directly
package guestartifacts

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMaterialize(t *testing.T) {
	t.Parallel()

	data := []byte("pretend kernel")

	t.Run("idempotent", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		first, err := materialize(root, "kernel-test", data, "")
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}

		second, err := materialize(root, "kernel-test", data, "")
		if err != nil {
			t.Fatalf("re-materialize: %v", err)
		}

		if first != second {
			t.Fatalf("paths differ across calls: %q vs %q", first, second)
		}

		got, err := os.ReadFile(first)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}

		if !bytes.Equal(got, data) {
			t.Fatal("extracted content differs from embedded data")
		}
	})

	t.Run("size mismatch rewrites", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		path, err := materialize(root, "kernel-test", data, "")
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}

		if err := os.Truncate(path, 3); err != nil {
			t.Fatalf("truncate: %v", err)
		}

		if _, err := materialize(root, "kernel-test", data, ""); err != nil {
			t.Fatalf("re-materialize over truncated file: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}

		if !bytes.Equal(got, data) {
			t.Fatal("truncated cache entry was not rewritten")
		}
	})

	t.Run("stamped digest picks the dir, malformed stamp falls back", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		stamp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

		path, err := materialize(root, "kernel-test", data, stamp)
		if err != nil {
			t.Fatalf("materialize with stamp: %v", err)
		}

		if filepath.Base(filepath.Dir(path)) != stamp[:cacheKeyLen] {
			t.Fatalf("dir %q does not use the stamped key", filepath.Dir(path))
		}

		if _, err := materialize(root, "kernel-test", data, "short"); err != nil {
			t.Fatalf("materialize with malformed stamp must fall back, got: %v", err)
		}
	})

	t.Run("concurrent extraction is safe", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		var group sync.WaitGroup

		for range 8 {
			group.Add(1)

			go func() {
				defer group.Done()

				if _, err := materialize(root, "kernel-test", data, ""); err != nil {
					t.Errorf("concurrent materialize: %v", err)
				}
			}()
		}

		group.Wait()
	})

	t.Run("digest memo short-circuits re-hashing and is advisory", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		first := memoizedDigest(root, "kernel-test", data)
		second := memoizedDigest(root, "kernel-test", data)

		if first != second {
			t.Fatalf("memoized digest differs: %q vs %q", first, second)
		}

		// A corrupted memo must be ignored (wrong key), not trusted.
		memoPath := filepath.Join(root, ".digest-kernel-test")
		if err := os.WriteFile(memoPath, []byte("bogus-key deadbeef"), 0o600); err != nil {
			t.Fatalf("corrupt memo: %v", err)
		}

		if got := memoizedDigest(root, "kernel-test", data); got != first {
			t.Fatalf("corrupt memo changed the digest: %q vs %q", got, first)
		}
	})

	t.Run("matching-key memo with a short digest re-hashes instead of panicking", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		// The dangerous shape: the binKey MATCHES this binary, so the memo is
		// trusted, and the digest field is shorter than cacheKeyLen — the exact
		// input that made materialize's key[:cacheKeyLen] slice panic.
		memoPath := filepath.Join(root, ".digest-kernel-test")
		if err := os.WriteFile(memoPath, []byte(executableKey()+" abc"), 0o600); err != nil {
			t.Fatalf("write short memo: %v", err)
		}

		path, err := materialize(root, "kernel-test", data, "")
		if err != nil {
			t.Fatalf("materialize with short memo: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}

		if !bytes.Equal(got, data) {
			t.Fatal("extracted content differs from embedded data")
		}
	})

	t.Run("prunes stale sibling dirs of the same artifact", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		staleDir := filepath.Join(root, "deadbeefdeadbeef")
		if err := os.MkdirAll(staleDir, 0o700); err != nil {
			t.Fatalf("mkdir stale: %v", err)
		}

		if err := os.WriteFile(filepath.Join(staleDir, "kernel-test"), []byte("old"), 0o600); err != nil {
			t.Fatalf("write stale: %v", err)
		}

		otherDir := filepath.Join(root, "cafecafecafecafe")
		if err := os.MkdirAll(otherDir, 0o700); err != nil {
			t.Fatalf("mkdir other: %v", err)
		}

		if err := os.WriteFile(filepath.Join(otherDir, "other-artifact"), []byte("keep"), 0o600); err != nil {
			t.Fatalf("write other: %v", err)
		}

		if _, err := materialize(root, "kernel-test", data, ""); err != nil {
			t.Fatalf("materialize: %v", err)
		}

		if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
			t.Fatal("stale dir for the same artifact was not pruned")
		}

		if _, err := os.Stat(filepath.Join(otherDir, "other-artifact")); err != nil {
			t.Fatal("dir belonging to a different artifact was wrongly pruned")
		}
	})
}
