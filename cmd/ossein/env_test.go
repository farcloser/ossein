//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	// t.Setenv forbids t.Parallel; keep this test serial.
	t.Setenv("OSSEIN_TEST_PRESENT", "hostval")

	got, err := resolveEnv([]string{
		"FOO=bar",                // explicit key=value
		"EMPTY=",                 // explicit empty value is preserved
		"OSSEIN_TEST_PRESENT",    // bare key set on host -> expanded
		"OSSEIN_TEST_ABSENT_XYZ", // bare key unset on host -> dropped (docker)
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"FOO=bar", "EMPTY=", "OSSEIN_TEST_PRESENT=hostval"}
	if !slices.Equal(got, want) {
		t.Fatalf("resolveEnv = %v, want %v", got, want)
	}

	if nilOut, err := resolveEnv(nil); err != nil || nilOut != nil {
		t.Fatalf("resolveEnv(nil) = %v, %v; want nil, nil", nilOut, err)
	}
}

func TestResolveEnvRejectsEmptyName(t *testing.T) {
	t.Parallel()

	// An empty variable name must be rejected exactly like parseEnvFile
	// rejects "=value" lines.
	for _, entry := range []string{"=VALUE", "=", ""} {
		if _, err := resolveEnv([]string{entry}); err == nil {
			t.Errorf("resolveEnv(%q): expected error, got nil", entry)
		}
	}
}

func TestParseEnvFile(t *testing.T) {
	t.Setenv("OSSEIN_TEST_HOST", "hv")

	path := filepath.Join(t.TempDir(), "env.list")
	content := "# a comment\n" +
		"\n" + // blank line
		"FOO=bar\n" +
		"EMPTY=\n" + // empty value kept
		"WITH=has=equals\n" + // value may contain '='
		"OSSEIN_TEST_HOST\n" + // bare, set on host -> expanded
		"OSSEIN_TEST_MISSING_ZZZ\n" // bare, unset -> dropped

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"FOO=bar", "EMPTY=", "WITH=has=equals", "OSSEIN_TEST_HOST=hv"}
	if !slices.Equal(got, want) {
		t.Fatalf("parseEnvFile = %v, want %v", got, want)
	}
}

func TestParseEnvFileErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()

		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	cases := map[string]string{
		"whitespace in name": write("ws.list", "BAD KEY=value\n"),
		"empty name":         write("empty.list", "=value\n"),
		"missing file":       filepath.Join(dir, "does-not-exist"),
	}

	for name, path := range cases {
		if _, err := parseEnvFile(path); err == nil {
			t.Errorf("%s (%s): expected error, got nil", name, path)
		}
	}
}

func TestEnvVarsFileThenFlagOrder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "env.list")
	if err := os.WriteFile(path, []byte("FROM=file\nSHARED=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := &runCmd{
		EnvFile: []string{path},
		Env:     []string{"SHARED=flag", "ONLY=flag"},
	}

	got, err := cmd.envVars()
	if err != nil {
		t.Fatal(err)
	}

	// File entries come first, then -e; ocispec dedups last-wins so the -e
	// SHARED=flag ultimately overrides the file's SHARED=file.
	want := []string{"FROM=file", "SHARED=file", "SHARED=flag", "ONLY=flag"}
	if !slices.Equal(got, want) {
		t.Fatalf("envVars = %v, want %v", got, want)
	}
}
