//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVolumeDirectoryBind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	mount, err := parseVolume(dir + ":/work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mount.Host != dir || mount.Dest != "/work" || mount.ReadOnly {
		t.Fatalf("got %+v, want host=%s dest=/work rw", mount, dir)
	}
}

func TestParseVolumeReadOnlyAndNoOpOptions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, opts := range []string{"ro", "readonly", "rw,ro", "ro,z,cached,rshared"} {
		mount, err := parseVolume(dir + ":/work:" + opts)
		if err != nil {
			t.Fatalf("opts %q: unexpected error: %v", opts, err)
		}

		if !mount.ReadOnly {
			t.Fatalf("opts %q: expected read-only", opts)
		}
	}

	// rw stays writable; a bare no-op option keeps the default (rw).
	for _, opts := range []string{"rw", "z", "delegated"} {
		mount, err := parseVolume(dir + ":/work:" + opts)
		if err != nil {
			t.Fatalf("opts %q: unexpected error: %v", opts, err)
		}

		if mount.ReadOnly {
			t.Fatalf("opts %q: expected read-write", opts)
		}
	}
}

func TestParseVolumeAutoCreatesMissingSource(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not", "there")

	mount, err := parseVolume(missing + ":/work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, statErr := os.Stat(mount.Host)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("expected auto-created dir at %s: err=%v", mount.Host, statErr)
	}
}

func TestParseVolumeRejectsUnsupportedForms(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	file := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(file, []byte("x=1"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"anonymous volume": "/data",
		"named volume":     "cache:/root/.cache",
		"single-file bind": file + ":/etc/app.toml",
		"relative dest":    dir + ":work",
		"unknown option":   dir + ":/work:nocopy",
		"too many fields":  dir + ":/work:ro:extra",
	}

	for name, spec := range cases {
		if _, err := parseVolume(spec); err == nil {
			t.Errorf("%s (%q): expected error, got nil", name, spec)
		}
	}
}
