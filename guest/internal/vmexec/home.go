package vmexec

// HOME defaulting for the workload environment.
//
// This file is portable on purpose (no linux build tag): the passwd parsing
// is pure and gets unit-tested by the native `do test go` leg, so a regression
// here is caught without booting a VM. The only linux caller is stage2.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// passwdPath is the container's own account database: stage2 calls this
// after the pivot, so the path resolves inside the image rootfs.
const passwdPath = "/etc/passwd"

// rootHome is runc's fallback home when the uid has no passwd entry. Declared
// here rather than sharing stage2's `slash`: that file is linux-only and this
// one must build natively for its tests.
const rootHome = "/"

// passwdHomeField is the index of the home directory in a
// name:password:uid:gid:gecos:home:shell line.
const (
	passwdUIDField  = 2
	passwdHomeField = 5
	// decimal is the radix passwd stores uids in.
	decimal = 10
)

// ensureHome gives the workload a HOME when neither the image config nor the
// user (-e / --env-file) set one — runc's behavior (libcontainer setupUser),
// which docker, podman and containerd all inherit, so tooling assumes it:
// git refuses `git config --global` outright without HOME ("fatal: $HOME not
// set", exit 128), and gpg, pip, npm and friends misbehave in quieter ways.
// The value is the uid's home in the container's /etc/passwd, "/" when it
// has no entry — exactly runc's default. An explicit empty HOME= counts as
// unset, as in runc.
func ensureHome(env []string, uid uint32) []string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "HOME="); ok && value != "" {
			return env
		}
	}

	return append(env, "HOME="+lookupHome(passwdPath, uid))
}

// lookupHome returns the home directory of uid from a passwd-format file, or
// "/" when the file is unreadable, malformed, or has no entry for the uid.
// Never an error: a missing /etc/passwd is a legitimate (scratch-style) image.
func lookupHome(path string, uid uint32) string {
	file, err := os.Open(path) // #nosec G304 -- fixed path inside the container rootfs
	if err != nil {
		return rootHome
	}
	defer func() { _ = file.Close() }()

	want := strconv.FormatUint(uint64(uid), decimal)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) > passwdHomeField && fields[passwdUIDField] == want && fields[passwdHomeField] != "" {
			return fields[passwdHomeField]
		}
	}

	return rootHome
}
