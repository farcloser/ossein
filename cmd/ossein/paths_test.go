//go:build darwin && arm64

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/filesystem/pathcheck"

	"github.com/farcloser/ossein/internal/cli"
)

func parseArgs(t *testing.T, args ...string) (CLI, error) {
	t.Helper()

	var root CLI

	parser, err := kong.New(&root, kong.Vars{"buildkit_image": "img", "log_levels": cli.LogLevels})
	if err != nil {
		t.Fatal(err)
	}

	_, err = parser.Parse(args)

	return root, err
}

func TestConsoleLogResolvedAtParse(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// ".." is legitimate in a relative path: resolved, not refused.
	want := filepath.Join(filepath.Dir(cwd), "c.log")

	root, err := parseArgs(t, "run", "--console-log", "../c.log", "img")
	if err != nil || root.Run.ConsoleLog.Path != want {
		t.Errorf("run: console log = (%q, %v), want %q", root.Run.ConsoleLog.Path, err, want)
	}

	root, err = parseArgs(t, "buildkit", "--console-log", "../c.log")
	if err != nil || root.Buildkit.ConsoleLog.Path != want {
		t.Errorf("buildkit: console log = (%q, %v), want %q", root.Buildkit.ConsoleLog.Path, err, want)
	}

	root, err = parseArgs(t, "doctor", "--console-log", "../c.log")
	if err != nil || root.Doctor.ConsoleLog.Path != want {
		t.Errorf("doctor: console log = (%q, %v), want %q", root.Doctor.ConsoleLog.Path, err, want)
	}
}

func TestConsoleLogRefusedAtParse(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"run", "--console-log", "/tmp/a\x00b/c.log", "img"},
		{"buildkit", "--console-log", "/tmp/a\x00b/c.log"},
		{"doctor", "--console-log", "/tmp/a\x00b/c.log"},
	} {
		if _, err := parseArgs(t, args...); !errors.Is(err, errUsage) || !errors.Is(err, pathcheck.ErrInvalidPath) {
			t.Errorf("%q = %v, want errUsage wrapping ErrInvalidPath", args, err)
		}
	}
}
