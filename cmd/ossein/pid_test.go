//go:build darwin && arm64

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// deadChild spawns and fully reaps a child, returning its now-dead pid and the
// start time it had while running — the raw material for stale-pid-file cases.
func deadChild(t *testing.T) (int, int64) {
	t.Helper()

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	pid := cmd.Process.Pid

	start, ok := processStartTime(pid)
	if !ok {
		t.Fatal("no start time for a live child")
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	return pid, start
}

// liveChild spawns a long-lived child that is killed and reaped at test end.
func liveChild(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return cmd.Process.Pid
}

func TestWritePidReadPidRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writePid(dir, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, pidFileName)

	pid, start, ok := readPid(path)
	if !ok {
		t.Fatal("readPid: expected ok")
	}

	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", pid, os.Getpid())
	}

	wantStart, alive := processStartTime(os.Getpid())
	if !alive || start != wantStart {
		t.Fatalf("start = %d, want %d (alive=%v)", start, wantStart, alive)
	}

	if !pidAlive(path) {
		t.Fatal("pidAlive: expected alive for own pid")
	}
}

func TestWritePidRejectsDeadPid(t *testing.T) {
	t.Parallel()

	pid, _ := deadChild(t)

	if err := writePid(t.TempDir(), pid); err == nil {
		t.Fatal("writePid for a dead pid: expected error, got nil")
	}
}

func TestPidAliveStaleFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), pidFileName)

	// Dead (reaped) child: the file parses but the process is gone.
	pid, start := deadChild(t)
	if err := os.WriteFile(path, fmt.Appendf(nil, "%d:%d", pid, start), pidFileMode); err != nil {
		t.Fatal(err)
	}

	if pidAlive(path) {
		t.Fatal("dead child reads as alive")
	}

	// Recycled-pid simulation: a live pid recorded with a different start time
	// must read as dead — that "instance" no longer exists.
	if err := os.WriteFile(path, fmt.Appendf(nil, "%d:%d", os.Getpid(), start+12345), pidFileMode); err != nil {
		t.Fatal(err)
	}

	if pidAlive(path) {
		t.Fatal("mismatched start time reads as alive")
	}
}

func TestReadPidGarbageIsStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cases := map[string]string{
		"empty":         "",
		"old format":    "1234", // pre-starttime files are stale by design
		"garbage":       "not-a-pid",
		"negative pid":  "-5:100",
		"zero pid":      "0:100",
		"bad start":     "123:xyz",
		"empty start":   "123:",
		"extra field":   "123:456:789",
		"trailing junk": "123:456\n",
	}

	for name, content := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), pidFileMode); err != nil {
			t.Fatal(err)
		}

		if _, _, ok := readPid(path); ok {
			t.Errorf("%s (%q): expected stale, got ok", name, content)
		}
	}

	if _, _, ok := readPid(filepath.Join(dir, "does-not-exist")); ok {
		t.Error("missing file: expected stale, got ok")
	}
}

func TestGcRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Live: a dir whose pid file points at a running process must survive.
	liveDir := filepath.Join(root, "bc-live")
	if err := os.MkdirAll(liveDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := writePid(liveDir, liveChild(t)); err != nil {
		t.Fatal(err)
	}

	// Dead: a dir whose pid file points at an already-reaped child is reaped.
	deadDir := filepath.Join(root, "bc-dead")
	if err := os.MkdirAll(deadDir, 0o750); err != nil {
		t.Fatal(err)
	}

	pid, start := deadChild(t)

	deadRecord := fmt.Appendf(nil, "%d:%d", pid, start)
	if err := os.WriteFile(filepath.Join(deadDir, pidFileName), deadRecord, pidFileMode); err != nil {
		t.Fatal(err)
	}

	// Bare: a dir with no pid file at all is reaped.
	bareDir := filepath.Join(root, "bc-bare")
	if err := os.MkdirAll(bareDir, 0o750); err != nil {
		t.Fatal(err)
	}

	// A stray file at the root is not instance state and is left alone.
	strayFile := filepath.Join(root, "stray")
	if err := os.WriteFile(strayFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := gcRoot(root)
	if err != nil {
		t.Fatal(err)
	}

	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	for path, want := range map[string]bool{liveDir: true, deadDir: false, bareDir: false, strayFile: true} {
		_, statErr := os.Stat(path)
		if exists := statErr == nil; exists != want {
			t.Errorf("%s: exists=%v, want %v", path, exists, want)
		}
	}
}
