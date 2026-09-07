//go:build linux

package guestagent

import "testing"

func TestExitCodeNilState(t *testing.T) {
	t.Parallel()

	// A failed cmd.Wait (ECHILD and kin) leaves ProcessState nil. exitCode
	// must return its documented placeholder, not dereference nil — the
	// pre-fix code panicked here, killing the agent connection, and
	// WaitProcess then fabricated exit code 0. (WaitProcess itself surfaces
	// proc.waitErr on that path; the placeholder never reaches the wire.)
	if got := exitCode(nil); got != -1 {
		t.Fatalf("exitCode(nil) = %d, want -1", got)
	}
}
