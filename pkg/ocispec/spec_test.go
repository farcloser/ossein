package ocispec_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/farcloser/ossein/pkg/ocispec"
)

func TestBuildUserParsing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		user     string
		uid, gid uint32
		wantErr  bool
	}{
		{"", 0, 0, false},
		{"0", 0, 0, false},
		{"1000", 1000, 1000, false},
		{"1000:2000", 1000, 2000, false},
		{"nobody", 0, 0, true},
		{"1000:staff", 0, 0, true},
	}
	for _, testCase := range cases {
		spec, err := ocispec.Build(ocispec.Params{User: testCase.user, Args: []string{"/bin/sh"}})
		if (err != nil) != testCase.wantErr {
			t.Fatalf("Build(user=%q) err=%v wantErr=%v", testCase.user, err, testCase.wantErr)
		}

		if err != nil {
			continue
		}

		if spec.Process.User.UID != testCase.uid || spec.Process.User.GID != testCase.gid {
			t.Fatalf("Build(user=%q) = %d:%d want %d:%d",
				testCase.user, spec.Process.User.UID, spec.Process.User.GID, testCase.uid, testCase.gid)
		}
	}
}

func TestBuildPrivilegedCaps(t *testing.T) {
	t.Parallel()

	priv, err := ocispec.Build(ocispec.Params{Privileged: true, Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(priv.Process.Capabilities.Effective, "CAP_SYS_ADMIN") {
		t.Fatal("privileged spec missing CAP_SYS_ADMIN")
	}

	unpriv, err := ocispec.Build(ocispec.Params{Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(unpriv.Process.Capabilities.Effective, "CAP_SYS_ADMIN") {
		t.Fatal("default spec must not grant CAP_SYS_ADMIN")
	}
}

func TestBuildPathAndCwdDefaults(t *testing.T) {
	t.Parallel()

	spec, err := ocispec.Build(ocispec.Params{Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	if spec.Process.Cwd != "/" {
		t.Fatalf("cwd default = %q, want /", spec.Process.Cwd)
	}

	if !slices.ContainsFunc(spec.Process.Env, func(e string) bool { return len(e) >= 5 && e[:5] == "PATH=" }) {
		t.Fatalf("PATH not defaulted: %v", spec.Process.Env)
	}

	// A caller-supplied PATH must not be duplicated.
	withPath, err := ocispec.Build(ocispec.Params{Args: []string{"/bin/sh"}, Env: []string{"PATH=/x"}})
	if err != nil {
		t.Fatal(err)
	}

	pathCount := 0

	for _, entry := range withPath.Process.Env {
		if len(entry) >= 5 && entry[:5] == "PATH=" {
			pathCount++
		}
	}

	if pathCount != 1 {
		t.Fatalf("PATH count = %d, want 1: %v", pathCount, withPath.Process.Env)
	}
}

// envValue returns key's value in env and whether it is present at all.
func envValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value, true
		}
	}

	return "", false
}

func TestBuildHostnameAndTermDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		params   ocispec.Params
		hostname string
		term     string
		wantTerm bool
	}{
		{
			name:     "no tty: HOSTNAME is the container id, no TERM",
			params:   ocispec.Params{ContainerID: "bc-1234", Args: []string{"/bin/sh"}},
			hostname: "bc-1234",
		},
		{
			name:     "tty: TERM defaults to xterm",
			params:   ocispec.Params{ContainerID: "bc-1234", Args: []string{"/bin/sh"}, TTY: true},
			hostname: "bc-1234",
			term:     "xterm",
			wantTerm: true,
		},
		{
			name: "image or user values win, empty included",
			params: ocispec.Params{
				ContainerID: "bc-1234", Args: []string{"/bin/sh"}, TTY: true,
				Env: []string{"HOSTNAME=custom", "TERM="},
			},
			hostname: "custom",
			term:     "",
			wantTerm: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			spec, err := ocispec.Build(testCase.params)
			if err != nil {
				t.Fatal(err)
			}

			if got, ok := envValue(spec.Process.Env, "HOSTNAME"); !ok || got != testCase.hostname {
				t.Fatalf("HOSTNAME = %q (present %v), want %q: %v", got, ok, testCase.hostname, spec.Process.Env)
			}

			got, ok := envValue(spec.Process.Env, "TERM")
			if ok != testCase.wantTerm || got != testCase.term {
				t.Fatalf("TERM = %q (present %v), want %q (present %v): %v",
					got, ok, testCase.term, testCase.wantTerm, spec.Process.Env)
			}

			// Defaults never duplicate a key.
			seen := map[string]int{}

			for _, entry := range spec.Process.Env {
				key, _, _ := strings.Cut(entry, "=")
				seen[key]++
			}

			for key, count := range seen {
				if count != 1 {
					t.Fatalf("%s appears %d times: %v", key, count, spec.Process.Env)
				}
			}
		})
	}
}

func TestBuildEnvUserOverridesImage(t *testing.T) {
	t.Parallel()

	// Params.Env arrives as image env followed by user env (see container.go),
	// so a later duplicate key must win and collapse to a single entry.
	spec, err := ocispec.Build(ocispec.Params{
		Args: []string{"/bin/sh"},
		Env:  []string{"FOO=image", "PATH=/img", "FOO=user"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := spec.Process.Env

	if !slices.Contains(env, "FOO=user") {
		t.Fatalf("user FOO not applied: %v", env)
	}

	if slices.Contains(env, "FOO=image") {
		t.Fatalf("image FOO not overridden: %v", env)
	}

	if !slices.Contains(env, "PATH=/img") {
		t.Fatalf("image PATH not preserved (should not be re-defaulted): %v", env)
	}
}

func TestBuildNamespacesHaveNoNetwork(t *testing.T) {
	t.Parallel()

	spec, err := ocispec.Build(ocispec.Params{Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	// The microVM is the network boundary; the container must share the guest's
	// network, so there must be NO network namespace.
	if len(spec.Linux.Namespaces) != 4 {
		t.Fatalf("namespaces = %d, want 4: %v", len(spec.Linux.Namespaces), spec.Linux.Namespaces)
	}

	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			t.Fatal("spec must not create a network namespace")
		}
	}
}

func TestCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		entrypoint, cmd, override, want []string
	}{
		{nil, []string{"sh"}, nil, []string{"sh"}},
		{[]string{"ep"}, []string{"cmd"}, nil, []string{"ep", "cmd"}},
		{[]string{"ep"}, []string{"cmd"}, []string{"other"}, []string{"ep", "other"}},
		{nil, nil, []string{"ls", "-l"}, []string{"ls", "-l"}},
	}
	for _, testCase := range cases {
		got := ocispec.Command(testCase.entrypoint, testCase.cmd, testCase.override)
		if !reflect.DeepEqual(got, testCase.want) {
			t.Fatalf("Command(%v,%v,%v) = %v want %v",
				testCase.entrypoint, testCase.cmd, testCase.override, got, testCase.want)
		}
	}
}

func TestMarshalNamespacePathKey(t *testing.T) {
	t.Parallel()

	spec, err := ocispec.Build(ocispec.Params{Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := ocispec.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	namespaces, ok := doc["linux"].(map[string]any)["namespaces"].([]any)
	if !ok {
		t.Fatal("namespaces missing from marshalled spec")
	}

	for _, entry := range namespaces {
		if _, has := entry.(map[string]any)["path"]; !has {
			// vminitd's Swift LinuxNamespace decoder requires this key.
			t.Fatalf("namespace missing path key: %v", entry)
		}
	}
}
