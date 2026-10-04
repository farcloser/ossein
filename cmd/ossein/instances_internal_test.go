//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindBuildkitInstance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// A record whose pid is THIS process: live.
	live := filepath.Join(root, "bc-live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := writeBuildkitRecord(live, buildkitRecord{Cache: "/caches/project", Sock: "/run/live.sock"}); err != nil {
		t.Fatal(err)
	}

	if err := writePid(live, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	// Same cache, but the pid file names a dead incarnation: skipped.
	dead := filepath.Join(root, "bc-dead")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := writeBuildkitRecord(dead, buildkitRecord{Cache: "/caches/project", Sock: "/run/dead.sock"}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dead, pidFileName), []byte("999999999:1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A live instance for ANOTHER cache, and a dir without a record (an
	// instance from a release that predates the record): both ignored.
	other := filepath.Join(root, "bc-other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := writeBuildkitRecord(other, buildkitRecord{Cache: "/caches/other", Sock: "/run/other.sock"}); err != nil {
		t.Fatal(err)
	}

	if err := writePid(other, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(root, "bc-norecord"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, found := findBuildkitInstance(root, "/caches/project")
	if !found || got.id != "bc-live" || got.rec.Sock != "/run/live.sock" || got.pid != os.Getpid() {
		t.Fatalf("findBuildkitInstance = (%+v, %v), want the live bc-live instance", got, found)
	}

	if _, found := findBuildkitInstance(root, "/caches/nobody"); found {
		t.Fatal("found an instance for a cache nobody serves")
	}

	if _, found := findBuildkitInstance(filepath.Join(root, "absent"), "/caches/project"); found {
		t.Fatal("found an instance under a missing state root")
	}
}
