//go:build darwin && arm64

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// osseinBinaryName is the runtime this shim fronts. It is found, in order, via
// OSSEIN_BIN, next to this executable (how a release archive and the aqua
// package lay the two binaries out), then PATH.
const osseinBinaryName = "ossein"

var (
	errNoOssein     = errors.New("ossein binary not found")
	errBuildkitHost = errors.New("ossein buildkit did not print a BUILDKIT_HOST")
	errUnsupported  = errors.New("not supported by ossein-docker")
	errBuildFailed  = errors.New("build failed")
)

// osseinBinary locates the ossein binary.
func osseinBinary() (string, error) {
	if explicit := os.Getenv("OSSEIN_BIN"); explicit != "" {
		// #nosec G703 -- OSSEIN_BIN is the user's own choice of binary, by design
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%w: OSSEIN_BIN=%q: %w", errNoOssein, explicit, err)
		}

		return explicit, nil
	}

	if self, err := os.Executable(); err == nil {
		// os.Executable follows the symlink an aqua proxy resolves through, so
		// this is the package directory, where both binaries of one release live.
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			sibling := filepath.Join(filepath.Dir(resolved), osseinBinaryName)
			if _, err := os.Stat(sibling); err == nil {
				return sibling, nil
			}
		}
	}

	found, err := exec.LookPath(osseinBinaryName)
	if err != nil {
		return "", fmt.Errorf("%w: not next to %s, not on PATH, OSSEIN_BIN unset",
			errNoOssein, filepath.Base(os.Args[0]))
	}

	return found, nil
}

// osseinArgv prefixes ossein's global --log-level so the two binaries log the
// same way, then the subcommand and its arguments.
func osseinArgv(level string, args ...string) []string {
	return append([]string{osseinBinaryName, "--log-level", level}, args...)
}

// execOssein REPLACES this process with ossein (execve): no signal forwarding,
// no exit-code translation, no second process on a TTY — the workload's exit
// status is the shell's, exactly as with `ossein run` typed directly. It only
// returns on failure to exec.
func execOssein(level string, args ...string) error {
	bin, err := osseinBinary()
	if err != nil {
		return err
	}

	if err := unix.Exec(bin, osseinArgv(level, args...), os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}

	return nil // unreachable: Exec does not return on success
}

// ensureBuildkit returns the BUILDKIT_HOST address for this directory's
// builder. An explicit BUILDKIT_HOST in the environment wins (the user
// started their own, or points at a remote one); otherwise
// `ossein buildkit --detach` starts the per-project microVM — or, when one
// already serves this project's cache, prints its address again — and its
// stdout contract (`export BUILDKIT_HOST=unix://…`) is parsed. ossein's
// stderr and stdin stay attached: its progress, its consent prompts, its
// errors are the user's to see.
func ensureBuildkit(ctx context.Context, level string) (string, error) {
	if host := os.Getenv("BUILDKIT_HOST"); host != "" {
		return host, nil
	}

	bin, err := osseinBinary()
	if err != nil {
		return "", err
	}

	argv := osseinArgv(level, "buildkit", "--detach")
	cmd := exec.CommandContext(ctx, bin, argv[1:]...) // #nosec G204 -- bin is the located ossein binary, argv ours

	var stdout bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ossein buildkit --detach: %w", err)
	}

	return parseBuildkitHost(stdout.String())
}

// parseBuildkitHost extracts the address from ossein's detach output.
func parseBuildkitHost(out string) (string, error) {
	const prefix = "export BUILDKIT_HOST="

	for line := range strings.SplitSeq(out, "\n") {
		if host, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok && host != "" {
			return host, nil
		}
	}

	return "", fmt.Errorf("%w (stdout was %q)", errBuildkitHost, strings.TrimSpace(out))
}
