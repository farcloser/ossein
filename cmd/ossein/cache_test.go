//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLooksLikePath(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"name":      false,
		"my-cache":  false,
		"./x":       true,
		".hidden":   true,
		"..":        true,
		"/abs/path": true,
		"a/b":       true,
	}

	for value, want := range cases {
		if got := looksLikePath(value); got != want {
			t.Errorf("looksLikePath(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestResolveCacheDir(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// Internal --cache-dir (detach child): used verbatim, never re-resolved.
	cmd := &buildkitCmd{CacheDir: "/pre/resolved", CacheLocal: true}

	dir, local, err := cmd.resolveCacheDir()
	if err != nil || dir != "/pre/resolved" || !local {
		t.Fatalf("CacheDir passthrough: got (%q, %v, %v)", dir, local, err)
	}

	// Default (empty --cache): central cache keyed by the current dir.
	cmd = &buildkitCmd{}

	dir, local, err = cmd.resolveCacheDir()
	if err != nil || !filepath.IsAbs(dir) || local {
		t.Fatalf("default: got (%q, %v, %v), want central abs dir", dir, local, err)
	}

	// Bare name: central cache — NOT a path under the current dir.
	cmd = &buildkitCmd{Cache: "teamcache"}

	dir, local, err = cmd.resolveCacheDir()
	if err != nil || !filepath.IsAbs(dir) || local {
		t.Fatalf("bare name: got (%q, %v, %v), want central abs dir", dir, local, err)
	}

	if dir == filepath.Join(cwd, "teamcache") {
		t.Fatalf("bare name resolved as a relative path: %q", dir)
	}

	// Path-like values: project-local, resolved against the current dir.
	for _, cache := range []string{"./x", "a/b"} {
		cmd = &buildkitCmd{Cache: cache}

		dir, local, err = cmd.resolveCacheDir()
		if err != nil || !local {
			t.Fatalf("%q: got (%q, %v, %v), want project-local", cache, dir, local, err)
		}

		want := filepath.Join(cwd, filepath.FromSlash(cache))
		if dir != want {
			t.Errorf("%q: dir = %q, want %q", cache, dir, want)
		}
	}

	// Absolute path: kept as-is, project-local.
	tmp := t.TempDir()
	cmd = &buildkitCmd{Cache: tmp}

	dir, local, err = cmd.resolveCacheDir()
	if err != nil || dir != tmp || !local {
		t.Fatalf("abs path: got (%q, %v, %v), want (%q, true, nil)", dir, local, err, tmp)
	}
}
