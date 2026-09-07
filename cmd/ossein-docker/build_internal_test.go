//go:build darwin && arm64

package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestFrontendAttrs(t *testing.T) {
	t.Parallel()

	env := map[string]string{"FROM_HOST": "yes"}
	lookup := func(key string) (string, bool) {
		value, ok := env[key]

		return value, ok
	}

	got, err := frontendAttrs("my.Dockerfile", "linux/amd64", "final", true, true,
		[]string{"A=1", "LIST=a,b=c", "FROM_HOST", "UNSET_ON_HOST"}, lookup)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"filename": "my.Dockerfile", "platform": "linux/amd64", "target": "final", "no-cache": "",
		"image-resolve-mode":  "pull",
		"build-arg:A":         "1",
		"build-arg:LIST":      "a,b=c", // split on the FIRST '=' only
		"build-arg:FROM_HOST": "yes",   // bare KEY → host value
		// UNSET_ON_HOST: bare and unset → not passed, as docker does
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attrs:\n got %v\nwant %v", got, want)
	}

	// Defaults: only the filename.
	got, err = frontendAttrs("Dockerfile", "", "", false, false, nil, lookup)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"filename": "Dockerfile"}) {
		t.Fatalf("default attrs = (%v, %v)", got, err)
	}

	if _, err := frontendAttrs(
		"Dockerfile",
		"",
		"",
		false,
		false,
		[]string{"=v"},
		lookup,
	); !errors.Is(
		err,
		errUnsupported,
	) {
		t.Fatalf("nameless build-arg = %v, want errUnsupported", err)
	}
}

func TestNormalizeTags(t *testing.T) {
	t.Parallel()

	got, err := normalizeTags([]string{"foo", "ghcr.io/org/app:v1", "reg.local:5000/org/x:dev"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"index.docker.io/library/foo:latest", "ghcr.io/org/app:v1", "reg.local:5000/org/x:dev"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeTags = %q, want %q", got, want)
	}

	if _, err := normalizeTags([]string{"foo@sha256:" + strings.Repeat("a", 64)}); !errors.Is(err, errUnsupported) {
		t.Fatalf("digest tag = %v, want errUnsupported", err)
	}

	if _, err := normalizeTags([]string{"UPPER:Case"}); err == nil {
		t.Fatal("invalid ref accepted")
	}
}

func TestParseBuildkitHost(t *testing.T) {
	t.Parallel()

	host, err := parseBuildkitHost("some noise\nexport BUILDKIT_HOST=unix:///tmp/x/buildkitd.sock\n")
	if err != nil || host != "unix:///tmp/x/buildkitd.sock" {
		t.Fatalf("parseBuildkitHost = (%q, %v)", host, err)
	}

	if _, err := parseBuildkitHost("nothing here"); !errors.Is(err, errBuildkitHost) {
		t.Fatalf("missing export line = %v, want errBuildkitHost", err)
	}
}

func TestBuildGrammarRefusesUnimplementedFlags(t *testing.T) {
	t.Parallel()

	var cli CLI

	parser, err := kong.New(&cli, kong.Vars{"log_levels": "debug,info,warn,error"})
	if err != nil {
		t.Fatal(err)
	}

	for _, argv := range [][]string{
		{"build", "--push", "-t", "x", "."},
		{"build", "--output", "type=local,dest=out", "."},
		{"build", "--secret", "id=x", "."},
		{"buildx", "bake"},
		{"compose", "up"},
	} {
		if _, err := parser.Parse(argv); err == nil {
			t.Errorf("%q was accepted; the shim must refuse what it does not implement", argv)
		}
	}

	// And accepts the shapes projects use — `buildx build` included.
	for _, argv := range [][]string{
		{"build", "-t", "app:dev", "-f", "ci/Dockerfile", "--build-arg", "V=1,2", "--platform", "linux/arm64", "."},
		{"buildx", "build", "--no-cache", "--target", "final", "--progress", "plain", "-q", "."},
	} {
		if _, err := parser.Parse(argv); err != nil {
			t.Errorf("%q refused: %v", argv, err)
		}
	}

	if cli.Buildx.Build.Target != "final" || !cli.Buildx.Build.NoCache || !cli.Buildx.Build.Quiet {
		t.Fatalf("buildx build flags not bound: %+v", cli.Buildx.Build)
	}
}
