//go:build darwin && arm64

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseVolumeSpec holds the -v grammar to its contract: any string is
// either a usage error or a source plus an absolute destination — never a
// panic, never a relative destination, never an empty source. The pure half
// only: parseVolume itself touches the filesystem (it creates a missing
// source directory), which a fuzz harness must never do with arbitrary paths.
func FuzzParseVolumeSpec(f *testing.F) {
	for _, seed := range []string{"/a:/b", "/a:/b:ro", "name:/x", "a", "::", "/a:/b:rw,ro", "/a:b", ":/b", "/a:/b:c:d", "/a:/b:"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		spec, err := parseVolumeSpec(value)
		if err != nil {
			if !errors.Is(err, errUsage) {
				t.Fatalf("parseVolumeSpec(%q): non-usage error %v", value, err)
			}

			return
		}

		if !strings.HasPrefix(spec.dest, "/") {
			t.Fatalf("parseVolumeSpec(%q) accepted a relative destination %q", value, spec.dest)
		}

		if spec.source == "" {
			t.Fatalf("parseVolumeSpec(%q) accepted an empty host source", value)
		}
	})
}

// FuzzParseEnvFile holds the --env-file parser to docker's contract: every
// entry it returns has a non-empty, whitespace-free name and a value; anything
// else is a usage error.
func FuzzParseEnvFile(f *testing.F) {
	for _, seed := range []string{"A=1\n", "# comment\n\nB\n", "A B=1\n", "=x\n", "\xEF\xBB\xBFA=1\n", "A==\n", "A=1"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, content string) {
		path := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		vars, err := parseEnvFile(path)
		if err != nil {
			if !errors.Is(err, errUsage) {
				t.Fatalf("parseEnvFile: non-usage error %v", err)
			}

			return
		}

		for _, entry := range vars {
			key, _, hasValue := strings.Cut(entry, "=")
			if key == "" || strings.ContainsAny(key, " \t") || !hasValue {
				t.Fatalf("parseEnvFile returned malformed entry %q for input %q", entry, content)
			}
		}
	})
}
