//go:build linux

package vmexec

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/mdlayher/vsock"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Child fd numbers shared with the agent (order matches the agent's ExtraFiles).
const (
	fdStart      = 3 // agent→child: one byte releases the pre-exec gate
	fdReady      = 4 // child→agent: one byte signals setup complete
	fdError      = 5 // child→agent: setup error message
	fdConsoleOut = 6 // child→agent: the pty master fd (TTY only), via SCM_RIGHTS
)

// Stage2Command is the argv[0+1] token that selects the stage2 role of the
// multi-call guest binary. Three places must agree on it — guest/vminitd
// dispatches on it, guestagent passes it when re-execing, and Stage2 names its
// flag set with it — so it is declared once, here, where the role is
// implemented. It is guest-internal, NOT part of the host↔guest protocol: the
// host never sees this argv.
const Stage2Command = "stage2"

// slash is the path separator and, alone, the filesystem root.
const slash = "/"

// stdDirMode is the standard world-traversable mode for directories created in
// the container filesystem (mountpoints, cwd); 0o750 would break non-root
// workloads.
const stdDirMode = 0o755

// Stage2 is the entry point for the `stage2` subcommand. On success it execs the
// workload (never returns); on failure it reports the error to the agent.
func Stage2() error {
	runtime.LockOSThread() // credential/cap/exec work must stay on one thread

	flagSet := flag.NewFlagSet(Stage2Command, flag.ContinueOnError)
	specPath := flagSet.String("spec", "", "path to the OCI runtime spec JSON")
	stdin := flagSet.Uint("stdin", 0, "host vsock port for stdin (0 = unwired)")
	stdout := flagSet.Uint("stdout", 0, "host vsock port for stdout")
	stderr := flagSet.Uint("stderr", 0, "host vsock port for stderr")

	tty := flagSet.Bool("tty", false, "allocate a pty; master is sent to the agent")
	if err := flagSet.Parse(os.Args[2:]); err != nil {
		return fmt.Errorf("parse stage2 flags: %w", err)
	}

	errPipe := os.NewFile(fdError, "error")
	if err := run(*specPath, *stdin, *stdout, *stderr, *tty); err != nil {
		_, _ = io.WriteString(errPipe, err.Error())
		_ = errPipe.Close()

		return err
	}

	return nil
}

// run mirrors vmexec's childSetup order. tty deliberately branches the setup:
// a pty comes from the container's devpts (after pivot), vsock stdio does not
// (before pivot) — splitting the function would duplicate the whole sequence.
//
//revive:disable-next-line:flag-parameter
func run(specPath string, stdin, stdout, stderr uint, tty bool) error {
	spec, err := loadSpec(specPath)
	if err != nil {
		return err
	}

	if spec.Process == nil || spec.Root == nil {
		return errInvalidSpec
	}

	// Non-TTY stdio is wired before pivot (vsock needs no filesystem). TTY stdio
	// is set up after the rootfs (the pty comes from the container's devpts).
	if !tty {
		if err := wireStdio(stdin, stdout, stderr); err != nil {
			return err
		}
	}

	if err := setupRootfs(spec); err != nil {
		return err
	}

	if tty {
		if err := setupConsole(); err != nil {
			return err
		}
	}

	if spec.Hostname != "" {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
	}

	if err := applySysctls(spec); err != nil {
		return err
	}
	// Masked/readonly paths need CAP_SYS_ADMIN (still held as root here) and must
	// precede the uid/cap drop; mask first so a doubly-listed path is hidden.
	if err := applyMaskedPaths(spec); err != nil {
		return err
	}

	if err := applyReadonlyPaths(spec); err != nil {
		return err
	}

	if err := setCloexecOnExtraFDs(); err != nil {
		return err
	}

	if err := setRLimits(spec.Process.Rlimits); err != nil {
		return err
	}

	caps, err := prepareCaps(spec.Process.Capabilities)
	if err != nil {
		return err
	}

	fixStdioPerms(spec.Process.User.UID)

	// After the pivot (so /etc/passwd is the image's), before the uid drop
	// (irrelevant to a world-readable file, but it keeps the read on the
	// privileged side of the fence with everything else that inspects the
	// rootfs). See home.go for why the workload must not start without HOME.
	spec.Process.Env = ensureHome(spec.Process.Env, spec.Process.User.UID)

	if err := setCredentials(spec.Process.User); err != nil {
		return err
	}

	if err := caps.finish(); err != nil {
		return err
	}

	if spec.Process.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("no_new_privs: %w", err)
		}
	}

	if err := setupCwd(spec.Process.Cwd); err != nil {
		return err
	}

	// Setup done: unblock CreateProcess, then wait for StartProcess before exec.
	signalReady()

	if err := waitStart(); err != nil {
		return err
	}

	return execProcess(spec.Process)
}

func loadSpec(path string) (*specs.Spec, error) {
	// The spec path is not attacker-chosen: the agent itself wrote the file
	// under /run/ossein and passed the path on the re-exec command line.
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the agent's own re-exec argv
	if err != nil {
		return nil, fmt.Errorf("read spec: %w", err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}

	return &spec, nil
}

// wireStdio dials each host vsock stdio port and dups the connection onto the
// target fd. dup3 flags 0 clears close-on-exec so the fd survives execve.
func wireStdio(stdin, stdout, stderr uint) error {
	for _, stdio := range []struct {
		port   uint
		target int
	}{{stdin, 0}, {stdout, 1}, {stderr, 2}} {
		if stdio.port == 0 {
			continue
		}

		// The port round-trips through a uint flag from the host's uint32 vsock
		// port, so the conversion back cannot overflow.
		conn, err := vsock.Dial(vsock.Host, uint32(stdio.port), nil) // #nosec G115 -- see above
		if err != nil {
			return fmt.Errorf("dial stdio vsock :%d: %w", stdio.port, err)
		}

		rawConn, err := conn.SyscallConn()
		if err != nil {
			_ = conn.Close()

			return fmt.Errorf("stdio vsock rawconn: %w", err)
		}

		var dupErr error

		if err := rawConn.Control(func(fd uintptr) { dupErr = unix.Dup3(int(fd), stdio.target, 0) }); err != nil {
			_ = conn.Close()

			return fmt.Errorf("stdio vsock control: %w", err)
		}

		_ = conn.Close()

		if dupErr != nil {
			return fmt.Errorf("dup stdio to %d: %w", stdio.target, dupErr)
		}

		// Go opens every socket non-blocking, and O_NONBLOCK is a property of the
		// open file description — which dup3 SHARES, so it survives onto the fd the
		// workload inherits. Left set, container stdio is non-blocking: once the
		// vsock send buffer fills (256 KiB), write(2) returns EAGAIN, and a program
		// that does not retry — most of them, writing to what POSIX promises is a
		// blocking fd — silently drops everything after it. That truncated any
		// output burst larger than the buffer while still exiting 0.
		if err := unix.SetNonblock(stdio.target, false); err != nil {
			return fmt.Errorf("clear O_NONBLOCK on stdio fd %d: %w", stdio.target, err)
		}
	}

	return nil
}

// setupConsole allocates a pty from the container's devpts, wires the slave to
// stdio, hands the master fd to the agent for relaying, and makes the pty the
// controlling terminal.
func setupConsole() error {
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open ptmx: %w", err)
	}
	defer func() { _ = unix.Close(master) }()

	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		return fmt.Errorf("unlockpt: %w", err)
	}

	ptn, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		return fmt.Errorf("ptsname: %w", err)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)

	slave, err := unix.Open(slavePath, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open pts: %w", err)
	}

	for _, target := range []int{0, 1, 2} {
		if err := unix.Dup3(slave, target, 0); err != nil {
			_ = unix.Close(slave)

			return fmt.Errorf("dup pts to %d: %w", target, err)
		}
	}

	_ = unix.Close(slave)

	// Hand the master to the agent (it outlives us) for the stdio relay.
	if err := sendFD(fdConsoleOut, master); err != nil {
		return fmt.Errorf("send console master: %w", err)
	}

	if err := unix.IoctlSetInt(0, unix.TIOCSCTTY, 0); err != nil {
		return fmt.Errorf("set controlling tty: %w", err)
	}

	return mountConsole(slavePath)
}

// sendFD passes a file descriptor to the agent over the SCM_RIGHTS socket at the
// given fd number.
func sendFD(sockFD, fd int) error {
	if err := unix.Sendmsg(sockFD, []byte{0}, unix.UnixRights(fd), nil, 0); err != nil {
		return fmt.Errorf("sendmsg fd: %w", err)
	}

	return nil
}

// consoleMode is the mode for a freshly created /dev/console bind target.
const consoleMode = 0o600

func mountConsole(slavePath string) error {
	const console = "/dev/console"
	if _, err := os.Stat(console); os.IsNotExist(err) {
		f, err := os.OpenFile(console, os.O_CREATE|os.O_RDWR, consoleMode)
		if err != nil {
			return fmt.Errorf("create /dev/console: %w", err)
		}

		_ = f.Close()
	}

	if err := unix.Mount(slavePath, console, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", slavePath, console, err)
	}

	return nil
}

// setupRootfs prepares and pivots into the container rootfs (vmexec's
// childRootSetup): slave the tree, bind the rootfs, apply spec mounts, create
// /dev symlinks and the ptmx symlink, pivot_root, optionally remount read-only,
// and reopen std fds pointing at the old /dev/null.
func setupRootfs(spec *specs.Spec) error {
	root := spec.Root.Path

	if err := unix.Mount("", slash, "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("mount slave /: %w", err)
	}

	if err := unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}

	for _, m := range spec.Mounts {
		if err := mountInto(root, m); err != nil {
			return fmt.Errorf("mount %s: %w", m.Destination, err)
		}
	}

	if err := makeDevNodes(root); err != nil {
		return err
	}

	configureConsole(root)
	setDevSymlinks(root)

	if err := pivotRoot(root); err != nil {
		return err
	}

	if spec.Root.Readonly {
		if err := remountReadonly(slash); err != nil {
			return err
		}
	}

	return reOpenDevNull()
}

func mountInto(root string, mnt specs.Mount) error {
	target := filepath.Join(root, mnt.Destination)
	// 0o755 is the standard mode for container mountpoint directories; the
	// workload must be able to traverse them (0o750 would break non-root users).
	if err := os.MkdirAll(target, stdDirMode); err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}

	flags, data := ParseMountOptions(mnt.Options)

	if err := unix.Mount(mnt.Source, target, mnt.Type, flags, data); err != nil {
		return fmt.Errorf("mount %s (%s): %w", mnt.Source, mnt.Type, err)
	}

	return nil
}

// makeDevNodes populates the container's private tmpfs /dev with the standard
// device set (runc's defaults). The spec mounts tmpfs — not the singleton
// devtmpfs — on /dev precisely so these nodes, and the console/ptmx tweaks
// below, stay namespace-local instead of mutating the VM's real /dev. Runs
// before the capability drop, so CAP_MKNOD is available. Nodes that already
// exist (e.g. a rootfs shipping its own) are kept.
func makeDevNodes(root string) error {
	nodes := []struct {
		name  string
		major uint32
		minor uint32
	}{
		{"null", 1, 3},
		{"zero", 1, 5},
		{"full", 1, 7},
		{"random", 1, 8},
		{"urandom", 1, 9},
		{"tty", 5, 0},
	}

	for _, node := range nodes {
		path := filepath.Join(root, "dev", node.name)
		if _, err := os.Lstat(path); err == nil {
			continue
		}

		// Mkdev of these fixed single-digit major/minor pairs is far below any
		// integer boundary; the int conversion cannot overflow.
		dev := unix.Mkdev(node.major, node.minor)
		if err := unix.Mknod(path, unix.S_IFCHR|0o666, int(dev)); err != nil { // #nosec G115 -- see above
			return fmt.Errorf("mknod /dev/%s: %w", node.name, err)
		}
	}

	return nil
}

// configureConsole replaces the devpts-provided /dev/ptmx with the standard
// symlink to pts/ptmx, so opening /dev/ptmx uses the container's devpts.
func configureConsole(root string) {
	ptmx := filepath.Join(root, "dev/ptmx")
	_ = os.Remove(ptmx)
	_ = os.Symlink("pts/ptmx", ptmx)
}

func setDevSymlinks(root string) {
	links := [][2]string{
		{"/proc/self/fd", "/dev/fd"},
		{"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"},
		{"/proc/self/fd/2", "/dev/stderr"},
		{"/dev/rtc0", "/dev/rtc"},
	}
	for _, l := range links {
		_ = os.Symlink(l[0], filepath.Join(root, l[1])) // best-effort; skip if present
	}
}

// pivotRoot switches the root to rootfs via runc's pivot_root(".", ".") method.
func pivotRoot(root string) error {
	oldRoot, err := unix.Open(slash, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open oldroot: %w", err)
	}
	defer func() { _ = unix.Close(oldRoot) }()

	newRoot, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open newroot: %w", err)
	}
	defer func() { _ = unix.Close(newRoot) }()

	if err := unix.Fchdir(newRoot); err != nil {
		return fmt.Errorf("fchdir newroot: %w", err)
	}

	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}

	if err := unix.Fchdir(oldRoot); err != nil {
		return fmt.Errorf("fchdir oldroot: %w", err)
	}

	if err := unix.Mount("", ".", "", unix.MS_SLAVE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("slave oldroot: %w", err)
	}

	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach oldroot: %w", err)
	}

	if err := unix.Chdir(slash); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	return nil
}

// statfsToMountFlags translates statfs ST_* flags into their mount(2) MS_*
// equivalents. Raw f_flags must never be OR'd into mount flags: the two flag
// spaces only partially overlap (e.g. ST_NOSYMFOLLOW collides with MS_MOVE).
//
//nolint:gochecknoglobals // immutable kernel-constant translation table
var statfsToMountFlags = []struct {
	st uintptr
	ms uintptr
}{
	{unix.ST_NOSUID, unix.MS_NOSUID},
	{unix.ST_NODEV, unix.MS_NODEV},
	{unix.ST_NOEXEC, unix.MS_NOEXEC},
	{unix.ST_NOATIME, unix.MS_NOATIME},
	{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
	{unix.ST_RELATIME, unix.MS_RELATIME},
	{unix.ST_SYNCHRONOUS, unix.MS_SYNCHRONOUS},
}

// remountReadonly bind-remounts a path read-only, preserving existing flags.
func remountReadonly(path string) error {
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	if err := unix.Mount("", path, "", flags, ""); err == nil {
		return nil
	}

	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}

	for _, f := range statfsToMountFlags {
		// Statfs flags are a kernel bitmask; the uintptr conversion is
		// width-preserving on linux/arm64 (both 64-bit) and only tested bitwise.
		if uintptr(stats.Flags)&f.st != 0 { // #nosec G115 -- see above
			flags |= f.ms
		}
	}

	if err := unix.Mount("", path, "", flags, ""); err != nil {
		return fmt.Errorf("remount %s: %w", path, err)
	}

	return nil
}

// reOpenDevNull redirects any std fd still pointing at the old root's /dev/null
// to the new root's /dev/null (vmexec's reOpenDevNull). A no-op for wired stdio.
func reOpenDevNull() error {
	nullFD, err := unix.Open("/dev/null", unix.O_RDWR, 0)
	if err != nil {
		return nil // no /dev/null yet; nothing to redirect
	}
	defer func() { _ = unix.Close(nullFD) }()

	var nullStat unix.Stat_t
	if unix.Fstat(nullFD, &nullStat) != nil {
		return nil
	}

	for _, fd := range []int{0, 1, 2} {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Rdev == nullStat.Rdev {
			_ = unix.Dup3(nullFD, fd, 0)
		}
	}

	return nil
}

func applySysctls(spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}

	for key, val := range spec.Linux.Sysctl {
		// Keys are dot-separated kernel parameter names; path characters would
		// escape /proc/sys after the dot→slash rewrite.
		if strings.Contains(key, slash) || strings.Contains(key, "..") {
			return fmt.Errorf("%w: %q", errInvalidSysctlKey, key)
		}

		path := "/proc/sys/" + strings.ReplaceAll(key, ".", slash)
		if err := os.WriteFile(path, []byte(val), 0); err != nil {
			return fmt.Errorf("sysctl %s=%s: %w", key, val, err)
		}
	}

	return nil
}

// applyMaskedPaths hides paths per linux.maskedPaths: directories get a
// read-only tmpfs, everything else a bind of /dev/null. Missing paths skip.
func applyMaskedPaths(spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}

	for _, maskedPath := range spec.Linux.MaskedPaths {
		info, err := os.Lstat(maskedPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("stat masked %s: %w", maskedPath, err)
		}

		// Never mount over a symlink's target: the link itself discloses
		// nothing, and following it would mask an unrelated path.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}

		if info.IsDir() {
			flags := uintptr(unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
			if err := unix.Mount("tmpfs", maskedPath, "tmpfs", flags, ""); err != nil {
				return fmt.Errorf("mask dir %s: %w", maskedPath, err)
			}
		} else if err := unix.Mount("/dev/null", maskedPath, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mask %s: %w", maskedPath, err)
		}
	}

	return nil
}

// applyReadonlyPaths makes paths read-only per linux.readonlyPaths. Missing
// paths skip.
func applyReadonlyPaths(spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}

	for _, roPath := range spec.Linux.ReadonlyPaths {
		if _, err := os.Stat(roPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("stat readonly %s: %w", roPath, err)
		}

		if err := unix.Mount(roPath, roPath, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind readonly %s: %w", roPath, err)
		}

		if err := remountReadonly(roPath); err != nil {
			return fmt.Errorf("remount readonly %s: %w", roPath, err)
		}
	}

	return nil
}

// rlimitByName maps OCI rlimit names to their unix resource numbers.
//
//nolint:gochecknoglobals // immutable kernel-constant lookup table
var rlimitByName = map[string]int{
	"RLIMIT_CPU": unix.RLIMIT_CPU, "RLIMIT_FSIZE": unix.RLIMIT_FSIZE, "RLIMIT_DATA": unix.RLIMIT_DATA,
	"RLIMIT_STACK": unix.RLIMIT_STACK, "RLIMIT_CORE": unix.RLIMIT_CORE, "RLIMIT_RSS": unix.RLIMIT_RSS,
	"RLIMIT_NPROC": unix.RLIMIT_NPROC, "RLIMIT_NOFILE": unix.RLIMIT_NOFILE, "RLIMIT_MEMLOCK": unix.RLIMIT_MEMLOCK,
	"RLIMIT_AS": unix.RLIMIT_AS, "RLIMIT_LOCKS": unix.RLIMIT_LOCKS, "RLIMIT_SIGPENDING": unix.RLIMIT_SIGPENDING,
	"RLIMIT_MSGQUEUE": unix.RLIMIT_MSGQUEUE, "RLIMIT_NICE": unix.RLIMIT_NICE, "RLIMIT_RTPRIO": unix.RLIMIT_RTPRIO,
	"RLIMIT_RTTIME": unix.RLIMIT_RTTIME,
}

func setRLimits(rlimits []specs.POSIXRlimit) error {
	for _, limit := range rlimits {
		res, ok := rlimitByName[limit.Type]
		if !ok {
			return fmt.Errorf("%w: %q", errUnknownRlimit, limit.Type)
		}

		lim := unix.Rlimit{Cur: limit.Soft, Max: limit.Hard}
		if err := unix.Setrlimit(res, &lim); err != nil {
			return fmt.Errorf("setrlimit %s: %w", limit.Type, err)
		}
	}

	return nil
}

// fixStdioPerms gives the requested user ownership of the std fds so an
// unprivileged process can still read/write its terminal.
func fixStdioPerms(uid uint32) {
	for _, fd := range []int{0, 1, 2} {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Uid != uid {
			_ = unix.Fchown(fd, int(uid), int(st.Gid))
		}
	}
}

// setCredentials applies supplementary groups, gid, then uid (uid last, since
// changing it drops the privilege needed to set the others).
func setCredentials(user specs.User) error {
	// Setgroups runs unconditionally, like runc: an empty AdditionalGids must
	// clear inherited supplementary groups, not keep them.
	gids := make([]int, len(user.AdditionalGids))
	for i, g := range user.AdditionalGids {
		gids[i] = int(g)
	}

	if err := unix.Setgroups(gids); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}

	if err := syscall.Setgid(int(user.GID)); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}

	if err := syscall.Setuid(int(user.UID)); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}

	return nil
}

// setCloexecOnExtraFDs marks every fd above stderr close-on-exec so agent pipes
// and stray descriptors don't leak into the container.
func setCloexecOnExtraFDs() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("list fds: %w", err)
	}

	for _, e := range entries {
		var fd int
		if _, err := fmt.Sscanf(e.Name(), "%d", &fd); err != nil || fd <= 2 {
			continue
		}

		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	}

	return nil
}

func signalReady() {
	ready := os.NewFile(fdReady, "ready")
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
}

func waitStart() error {
	start := os.NewFile(fdStart, "start")
	defer func() { _ = start.Close() }()

	if _, err := io.ReadFull(start, make([]byte, 1)); err != nil {
		return fmt.Errorf("await start: %w", err)
	}

	return nil
}

// setupCwd enters the workload's working directory. It runs after the uid/cap
// drop and before the ready signal, so an unusable cwd fails CreateProcess
// through the error pipe — OCI forbids a silent fallback to "/".
func setupCwd(cwd string) error {
	if cwd == "" {
		cwd = slash
	}

	// 0o755 keeps a created cwd traversable by the (possibly non-root)
	// workload user, matching runc's behavior for a missing cwd.
	if err := os.MkdirAll(cwd, stdDirMode); err != nil {
		return fmt.Errorf("create cwd %s: %w", cwd, err)
	}

	if err := unix.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}

	return nil
}

// execProcess resolves the entrypoint against the container PATH and execve's
// it — replacing this process with the workload. The working directory was
// already entered by setupCwd.
func execProcess(proc *specs.Process) error {
	if len(proc.Args) == 0 {
		return errEmptyArgs
	}

	path, err := findExecutable(proc.Args[0], proc.Cwd, proc.Env)
	if err != nil {
		return err
	}

	// Exec'ing the spec's argv is this process's entire purpose: stage2 exists
	// to set up the container and then become the workload. On success Exec
	// never returns.
	if err := syscall.Exec(path, proc.Args, proc.Env); err != nil { // #nosec G204 -- see above
		return fmt.Errorf("exec %s: %w", path, err)
	}

	return nil
}

// findExecutable resolves argv[0] like execvpe.
func findExecutable(name, cwd string, env []string) (string, error) {
	if strings.Contains(name, slash) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}

		if isExecutable(name) {
			return name, nil
		}

		return "", fmt.Errorf("%w: %q", errExecNotFound, name)
	}

	for dir := range strings.SplitSeq(pathFromEnv(env), ":") {
		if dir == "" {
			continue
		}

		if candidate := filepath.Join(dir, name); isExecutable(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("%w in PATH: %q", errExecNotFound, name)
}

// anyExecBit matches an executable bit for user, group or other.
const anyExecBit = 0o111

func isExecutable(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir() && info.Mode()&anyExecBit != 0
}

func pathFromEnv(env []string) string {
	for _, e := range env {
		if after, ok := strings.CutPrefix(e, "PATH="); ok {
			return after
		}
	}

	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// ParseMountOptions splits OCI mount options into a mount(2) flag bitmask and
// the comma-joined remainder (filesystem-specific data). It is the single
// implementation shared with the agent's Mount RPC.
func ParseMountOptions(options []string) (uintptr, string) {
	var (
		flags uintptr
		data  []string
	)

	for _, opt := range options {
		switch opt {
		case "ro":
			flags |= unix.MS_RDONLY
		case "rw":
			flags &^= unix.MS_RDONLY
		case "nosuid":
			flags |= unix.MS_NOSUID
		case "nodev":
			flags |= unix.MS_NODEV
		case "noexec":
			flags |= unix.MS_NOEXEC
		case "sync":
			flags |= unix.MS_SYNCHRONOUS
		case "noatime":
			flags |= unix.MS_NOATIME
		case "nodiratime":
			flags |= unix.MS_NODIRATIME
		case "relatime":
			flags |= unix.MS_RELATIME
		case "strictatime":
			flags |= unix.MS_STRICTATIME
		case "bind":
			flags |= unix.MS_BIND
		case "rbind":
			flags |= unix.MS_BIND | unix.MS_REC
		default:
			data = append(data, opt)
		}
	}

	return flags, strings.Join(data, ",")
}
