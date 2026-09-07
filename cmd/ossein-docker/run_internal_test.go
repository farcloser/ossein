//go:build darwin && arm64

package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/alecthomas/kong"
)

// parseRun runs the real kong grammar over a docker-shaped argv and returns
// the ossein argv it translates to.
func parseRun(t *testing.T, argv ...string) ([]string, error) {
	t.Helper()

	var cli CLI

	parser, err := kong.New(&cli, kong.Vars{"log_levels": "debug,info,warn,error"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := parser.Parse(append([]string{"run"}, argv...)); err != nil {
		return nil, err
	}

	return cli.Run.osseinArgs()
}

func TestRunTranslatesBuildCurlInvocation(t *testing.T) {
	t.Parallel()

	// build-posix.sh's exact shape (podman/docker head), plus the resource
	// flags in docker syntax.
	got, err := parseRun(t,
		"--rm", "--platform", "linux/amd64", "--cpus", "4", "--memory", "8g",
		"--volume", "/w:/w", "--workdir", "/w", "--env-file", "/dev/fd/63",
		"debian@sha256:abc", "sh", "-c", "./_ci-linux-debian.sh")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"run", "--cpus", "4", "--memory", "8192", "--rm", "--workdir", "/w", "--platform", "linux/amd64",
		"--pull", "missing", "--env-file", "/dev/fd/63", "--volume", "/w:/w",
		"--", "debian@sha256:abc", "sh", "-c", "./_ci-linux-debian.sh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ossein argv:\n got %q\nwant %q", got, want)
	}
}

func TestRunKeepsCommasAndCombinedShortFlags(t *testing.T) {
	t.Parallel()

	got, err := parseRun(t, "-it", "-e", "LIST=a,b", "-e", "HOME", "--network", "none", "--user", "1000:1000",
		"img", "--not-a-flag")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"run", "--no-network", "--interactive", "--tty", "--user", "1000:1000", "--pull", "missing",
		"--env", "LIST=a,b", "--env", "HOME", "--", "img", "--not-a-flag",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ossein argv:\n got %q\nwant %q", got, want)
	}
}

func TestRunRefusesWhatOsseinCannotDo(t *testing.T) {
	t.Parallel()

	for _, argv := range [][]string{
		{"--network", "mynet", "img"},
		{"--cpus", "zero", "img"},
		{"--memory=-1", "img"},
		{"--memory", "0", "img"},
	} {
		if _, err := parseRun(t, argv...); !errors.Is(err, errUnsupported) {
			t.Errorf("run %q = %v, want errUnsupported", argv, err)
		}
	}

	// Unknown docker flags are refused by the grammar itself (never dropped).
	if _, err := parseRun(t, "--name", "x", "img"); err == nil {
		t.Error("run --name was accepted; the shim must refuse flags it does not implement")
	}
}

func TestParseMemoryMiB(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]uint64{
		"8g": 8192, "8G": 8192, "512m": 512, "1024k": 1, "1": 1, "1048577": 2, "1048576b": 1, "1500k": 2,
	} {
		got, err := parseMemoryMiB(spec)
		if err != nil || got != want {
			t.Errorf("parseMemoryMiB(%q) = (%d, %v), want %d", spec, got, err, want)
		}
	}

	for _, bad := range []string{"", "0", "g", "1t", "abc"} {
		if _, err := parseMemoryMiB(bad); !errors.Is(err, errUnsupported) {
			t.Errorf("parseMemoryMiB(%q) = %v, want errUnsupported", bad, err)
		}
	}
}

func TestParseCPUs(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]uint64{"4": 4, "1.5": 2, "0.1": 1} {
		got, err := parseCPUs(spec)
		if err != nil || got != want {
			t.Errorf("parseCPUs(%q) = (%d, %v), want %d", spec, got, err, want)
		}
	}

	for _, bad := range []string{"0", "-1", "x", "Inf"} {
		if _, err := parseCPUs(bad); !errors.Is(err, errUnsupported) {
			t.Errorf("parseCPUs(%q) = %v, want errUnsupported", bad, err)
		}
	}
}
