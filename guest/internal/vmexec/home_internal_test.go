package vmexec

import (
	"os"
	"path/filepath"
	"testing"
)

const testPasswd = `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
nohome:x:7:7::/nonexistent:/usr/sbin/nologin
emptyhome:x:8:8:::/bin/sh
short:x:9
builder:x:1000:1000:Builder,,,:/home/builder:/bin/bash
`

func writePasswd(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLookupHome(t *testing.T) {
	t.Parallel()

	path := writePasswd(t, testPasswd)

	tests := []struct {
		name string
		uid  uint32
		want string
	}{
		{name: "root", uid: 0, want: "/root"},
		{name: "system user", uid: 1, want: "/usr/sbin"},
		{name: "regular user with gecos commas", uid: 1000, want: "/home/builder"},
		{name: "nonexistent home is still the entry", uid: 7, want: "/nonexistent"},
		{name: "empty home field falls back to slash", uid: 8, want: "/"},
		{name: "truncated line is skipped", uid: 9, want: "/"},
		{name: "unknown uid", uid: 4242, want: "/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := lookupHome(path, tc.uid); got != tc.want {
				t.Fatalf("lookupHome(%d) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}

func TestLookupHomeMissingFile(t *testing.T) {
	t.Parallel()

	if got := lookupHome(filepath.Join(t.TempDir(), "absent"), 0); got != "/" {
		t.Fatalf("missing passwd: got %q, want /", got)
	}
}

func TestEnsureHome(t *testing.T) {
	t.Parallel()

	// The real lookup reads /etc/passwd of the machine running the test; only
	// the presence/precedence contract is asserted here, not the value.
	tests := []struct {
		name string
		env  []string
		keep bool
	}{
		{name: "image or user HOME wins", env: []string{"PATH=/bin", "HOME=/srv/app"}, keep: true},
		{name: "absent HOME is added", env: []string{"PATH=/bin"}},
		{name: "empty HOME= counts as unset", env: []string{"HOME=", "PATH=/bin"}},
		{name: "prefix lookalike is not HOME", env: []string{"HOMEBREW=1"}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := ensureHome(append([]string{}, testCase.env...), 0)

			if testCase.keep {
				if len(got) != len(testCase.env) {
					t.Fatalf("env changed: %v -> %v", testCase.env, got)
				}

				return
			}

			last := got[len(got)-1]
			if len(got) != len(testCase.env)+1 || len(last) <= len("HOME=") || last[:len("HOME=")] != "HOME=" {
				t.Fatalf("no non-empty HOME appended: %v -> %v", testCase.env, got)
			}
		})
	}
}
