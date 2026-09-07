// Package ocispec turns run parameters into the OCI runtime spec that vminitd
// executes (via vmexec) inside the microVM. It is deliberately VM-independent
// pure logic — no vsock, no Code-Hex/vz — so it unit-tests as a black box.
package ocispec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// defaultCaps is the docker/runc default (unprivileged) capability set.
// A function, not a package var, so each spec gets its own slice (no shared
// mutable global).
func defaultCaps() []string {
	return []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD",
		"CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
}

// allCaps is the full capability set granted to --privileged containers.
func allCaps() []string {
	return []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER",
		"CAP_FSETID", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP",
		"CAP_LINUX_IMMUTABLE", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST",
		"CAP_NET_ADMIN", "CAP_NET_RAW", "CAP_IPC_LOCK", "CAP_IPC_OWNER",
		"CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT", "CAP_SYS_PTRACE",
		"CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE",
		"CAP_SYS_RESOURCE", "CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD",
		"CAP_LEASE", "CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL", "CAP_SETFCAP",
		"CAP_MAC_OVERRIDE", "CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM",
		"CAP_BLOCK_SUSPEND", "CAP_AUDIT_READ", "CAP_PERFMON", "CAP_BPF",
		"CAP_CHECKPOINT_RESTORE",
	}
}

// Params describes one container process for Build.
type Params struct {
	ContainerID string
	RootfsPath  string
	Args        []string
	Env         []string
	Cwd         string
	User        string // numeric uid[:gid]; empty = root
	TTY         bool
	Privileged  bool
	ExtraMounts []specs.Mount
}

// Recurring mount vocabulary — named so the mount table below stays greppable
// without tripping repetition lint.
const (
	optNosuid   = "nosuid"
	optNoexec   = "noexec"
	optNodev    = "nodev"
	fsTmpfs     = "tmpfs"
	decimalBase = 10
)

// defaultMounts mirrors containerization's LinuxContainer.defaultMounts().
// Each mount gets its OWN options slice — sharing one across entries would let
// an append through one mount's Options mutate its siblings.
func defaultMounts() []specs.Mount {
	return []specs.Mount{
		{Type: "proc", Source: "proc", Destination: "/proc"},
		{Type: "sysfs", Source: "sysfs", Destination: "/sys", Options: []string{optNosuid, optNoexec, optNodev}},
		// tmpfs, NOT devtmpfs: devtmpfs is a kernel singleton, so mounting it
		// would hand every container the VM's real /dev (block devices
		// included) and make the guest runtime's console/ptmx tweaks VM-global.
		// The runtime creates the standard device nodes (null, zero, …) in
		// this empty tmpfs instead — runc's layout.
		{
			Type:        fsTmpfs,
			Source:      fsTmpfs,
			Destination: "/dev",
			Options:     []string{optNosuid, "strictatime", "mode=755", "size=65536k"},
		},
		{
			Type:        "mqueue",
			Source:      "mqueue",
			Destination: "/dev/mqueue",
			Options:     []string{optNosuid, optNoexec, optNodev},
		},
		{
			Type:        fsTmpfs,
			Source:      fsTmpfs,
			Destination: "/dev/shm",
			Options:     []string{optNosuid, optNoexec, optNodev, "mode=1777", "size=65536k"},
		},
		{
			Type:        "cgroup2",
			Source:      "none",
			Destination: "/sys/fs/cgroup",
			Options:     []string{optNosuid, optNoexec, optNodev},
		},
		{
			Type:        "devpts",
			Source:      "devpts",
			Destination: "/dev/pts",
			Options:     []string{optNosuid, optNoexec, "newinstance", "gid=5", "mode=0620", "ptmxmode=0666"},
		},
	}
}

// Build assembles the OCI runtime spec handed to vminitd. There is no network
// namespace: the microVM is the network boundary, so the container must see
// the guest's eth0 directly.
func Build(params Params) (*specs.Spec, error) {
	caps := defaultCaps()
	if params.Privileged {
		caps = allCaps()
	}

	uid, gid, err := parseUser(params.User)
	if err != nil {
		return nil, err
	}

	cwd := params.Cwd
	if cwd == "" {
		cwd = "/"
	}

	spec := &specs.Spec{
		Version:  specs.Version,
		Hostname: params.ContainerID,
		Root:     &specs.Root{Path: params.RootfsPath},
		Process: &specs.Process{
			Terminal: params.TTY,
			Args:     params.Args,
			Env:      defaultEnv(dedupEnv(params.Env), params),
			Cwd:      cwd,
			User:     specs.User{UID: uid, GID: gid},
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  caps,
				Effective: caps,
				Permitted: caps,
			},
		},
		Mounts: append(defaultMounts(), params.ExtraMounts...),
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
			},
		},
	}

	return spec, nil
}

// Marshal serializes the OCI spec for vminitd's Swift decoder. Most of their
// structs decode leniently (decodeIfPresent), but LinuxNamespace is
// synthesized Codable and requires the "path" key — which Go's specs-go drops
// via omitempty when empty. Patch it in post-marshal. The intermediate decode
// uses UseNumber so large int64s (e.g. RLIM_INFINITY in rlimits) survive the
// round-trip instead of losing precision as float64.
func Marshal(spec *specs.Spec) ([]byte, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshalling oci spec: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("re-parsing oci spec: %w", err)
	}

	if linux, ok := doc["linux"].(map[string]any); ok {
		if namespaces, ok := linux["namespaces"].([]any); ok {
			for _, entry := range namespaces {
				if nsMap, ok := entry.(map[string]any); ok {
					if _, has := nsMap["path"]; !has {
						nsMap["path"] = ""
					}
				}
			}
		}
	}

	patched, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-marshalling oci spec: %w", err)
	}

	return patched, nil
}

// parseUser handles numeric "uid[:gid]"; empty means root. Name-based users
// would need the guest's /etc/passwd — out of scope, fail loud instead of
// silently running as the wrong identity.
func parseUser(user string) (uid, gid uint32, err error) {
	if user == "" {
		return 0, 0, nil
	}

	parts := strings.SplitN(user, ":", 2)

	parsedUID, err := strconv.ParseUint(parts[0], decimalBase, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %q (use uid[:gid])", ErrInvalidUser, user)
	}

	parsedGID := parsedUID

	if len(parts) == 2 {
		group, err := strconv.ParseUint(parts[1], decimalBase, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: non-numeric group in %q", ErrInvalidUser, user)
		}

		parsedGID = group
	}

	return uint32(parsedUID), uint32(parsedGID), nil
}

// dedupEnv collapses duplicate environment entries by key, keeping the LAST
// occurrence's value while holding each key's first-seen position. Callers pass
// image env followed by user env, so this gives docker's -e semantics: user
// variables (and later -e flags) override image ones. Callers resolve entries
// without '=' before reaching here; if one slipped through it would simply
// dedup under its full text.
func dedupEnv(env []string) []string {
	value := make(map[string]string, len(env))
	order := make([]string, 0, len(env))

	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, seen := value[key]; !seen {
			order = append(order, key)
		}

		value[key] = entry
	}

	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, value[key])
	}

	return out
}

// defaultPath is docker's PATH for an image whose config sets none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// defaultTerm is what docker exports whenever it allocates a tty; curses
// programs (less, vim, top) refuse to start without a TERM at all.
const defaultTerm = "xterm"

// defaultEnv adds the variables the docker daemon guarantees a workload and
// that only the host knows: PATH when the image sets none, HOSTNAME (the
// container id, which is also the spec hostname) always, TERM only when a tty
// is allocated. Each is a fallback — an image or user (-e) value wins, so the
// call must follow dedupEnv. HOME is the fourth docker guarantee, but it is
// runc's (from the image's /etc/passwd), so the guest sets it (vmexec/home.go).
func defaultEnv(env []string, params Params) []string {
	env = ensureEnv(env, "PATH", defaultPath)
	env = ensureEnv(env, "HOSTNAME", params.ContainerID)

	if params.TTY {
		env = ensureEnv(env, "TERM", defaultTerm)
	}

	return env
}

// ensureEnv appends key=value unless env already carries key (with any value,
// empty included: an explicit -e TERM= is the user's to make).
func ensureEnv(env []string, key, value string) []string {
	prefix := key + "="

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return env
		}
	}

	return append(env, prefix+value)
}

// Command merges the image config with a user override, docker-style: a
// non-empty override replaces Cmd; Entrypoint is always preserved.
func Command(entrypoint, cmd, override []string) []string {
	if len(override) > 0 {
		return append(append([]string{}, entrypoint...), override...)
	}

	return append(append([]string{}, entrypoint...), cmd...)
}
