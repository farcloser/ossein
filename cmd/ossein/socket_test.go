//go:build darwin && arm64

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// shortSockPath returns a unix socket path short enough for sockaddr_un
// (t.TempDir embeds the test name and can blow the 104-byte darwin limit).
func shortSockPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "oss") //nolint:usetesting // t.TempDir paths blow the 104-byte limit
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "s")
}

func TestProbeAndAwaitSocketReady(t *testing.T) {
	t.Parallel()

	sock := shortSockPath(t)

	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()

	// Connections sit unaccepted in the backlog — no bytes, no close — which
	// probeSocketChain must read as "held open = ready".
	if !probeSocketChain(t.Context(), sock) {
		t.Fatal("probeSocketChain: ready socket probed as not ready")
	}

	// Own pid as the "child": alive, and the ready path never reaches the
	// timeout kill.
	if err := awaitSocket(t.Context(), sock, os.Getpid(), "unused.log", 2*time.Second); err != nil {
		t.Fatalf("awaitSocket on a ready socket: %v", err)
	}
}

func TestProbeSocketChainAbsent(t *testing.T) {
	t.Parallel()

	if probeSocketChain(t.Context(), filepath.Join(t.TempDir(), "missing.sock")) {
		t.Fatal("probeSocketChain: absent socket probed as ready")
	}
}

func TestAwaitSocketTimesOutQuickly(t *testing.T) {
	t.Parallel()

	sock := shortSockPath(t) // never listened on

	// awaitSocket SIGKILLs the child on timeout, so give it a sacrificial one.
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	began := time.Now()

	err := awaitSocket(t.Context(), sock, cmd.Process.Pid, "unused.log", 300*time.Millisecond)
	if err == nil {
		t.Fatal("awaitSocket on an absent socket: expected timeout error")
	}

	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("awaitSocket took %s; want a quick timeout", elapsed)
	}
}
