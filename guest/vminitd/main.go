//go:build linux

// Command vminitd is the ossein guest binary: one static linux/arm64 executable
// that is PID 1 inside the microVM. It is named for where it lands in the initfs
// (/sbin/vminitd, matching the kernel's init= cmdline). As PID 1 it mounts the
// base filesystems and serves the SandboxContext API over vsock, so the
// host drives the whole container lifecycle remotely (see guest/internal/guestagent).
//
// It is a multi-call binary. The kernel execs it as PID 1 (via init=), which
// selects the agent role here; the agent re-execs it as `stage2` — the port of
// Apple's vmexec — to set up and exec each container process. Any other
// argument is an error rather than a silent no-op.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/guestagent"
	"github.com/farcloser/ossein/guest/internal/httpd"
	"github.com/farcloser/ossein/guest/internal/vmexec"
	"github.com/farcloser/ossein/internal/protocol"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

func main() {
	// GOMAXPROCS stays at the boot default (NumCPU) through rootfs extraction —
	// the zstd decoder's concurrency depends on it — and drops to 1 once the
	// copy-in completes (see guestagent's maxprocsOnce for the rationale).
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("guest: ")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case vmexec.Stage2Command:
			// The re-exec'd container runtime child (vmexec port). It execs the
			// workload on success; reaching here means it failed.
			if err := vmexec.Stage2(); err != nil {
				log.Fatalf("stage2: %v", err)
			}

			return
		default:
			// argv comes from the agent's own re-exec of this binary, not from
			// any remote input, and %q escapes it anyway.
			log.Fatalf("unknown subcommand %q", os.Args[1]) // #nosec G706 -- see above
		}
	}

	// runInit never returns on success. If it fails we exit nonzero, which as
	// PID 1 makes the kernel panic — deliberately loud, and captured in the
	// serial console log the host keeps.
	log.Fatalf("init: %v", runInit())
}

// deviceWaitBudget bounds how long init waits for an async-probed device to
// appear (console, vsock; the rootfs device and NIC have their own waits).
// Devices probe within ~1ms of each other in practice — the budget is a
// safety bound for a wedged device, not an expected wait.
const deviceWaitBudget = 2 * time.Second

// devicePollInterval is the poll cadence while waiting for a device.
const devicePollInterval = time.Millisecond

// ensureConsole points fds 0-2 at /dev/console, waiting briefly for the
// console device to finish probing. Ground fds on /dev/null first so that,
// even if the console never appears, fds 0-2 exist and later opens cannot
// land on them.
func ensureConsole() {
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		for fd := range 3 {
			_ = unix.Dup2(int(null.Fd()), fd)
		}
	}

	deadline := time.Now().Add(deviceWaitBudget)

	for {
		console, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
		if err == nil {
			for fd := range 3 {
				_ = unix.Dup2(int(console.Fd()), fd)
			}

			if console.Fd() > 2 {
				_ = console.Close()
			}

			return
		}

		if time.Now().After(deadline) {
			return // headless: fds stay on /dev/null
		}

		time.Sleep(devicePollInterval)
	}
}

// listenVsockRetry is vsock.Listen with a bounded wait for the virtio-vsock
// device to finish probing.
func listenVsockRetry(port uint32, budget time.Duration) (*vsock.Listener, error) {
	deadline := time.Now().Add(budget)

	for {
		listener, err := vsock.Listen(port, nil)
		if err == nil {
			return listener, nil
		}

		if time.Now().After(deadline) {
			//nolint:wrapcheck // caller adds the port context
			return nil, err
		}

		time.Sleep(devicePollInterval)
	}
}

// serverHeaderTimeout bounds how long a client may dawdle sending request
// headers. The only client is the host over vsock, so this is a hygiene bound
// (and what makes the linter's slowloris check moot), not a real defence.
const serverHeaderTimeout = 30 * time.Second

// maxRequestBytes caps a single RPC request body (see the handler mux).
const maxRequestBytes = 4 << 20 // 4 MiB

// runInit performs PID-1 setup and serves the agent until the VM is stopped.
func runInit() error {
	// FIRST, before anything logs: with async device probing the hvc0
	// console may register after the kernel execs init, in which case init
	// starts with NO stdio at all — every log line (including the copy-in
	// metering the bench tooling reads) would vanish, and worse, the next
	// opened files would occupy fds 0-2 and get scribbled on by log writes.
	// Reattach deliberately, waiting briefly for the console to probe.
	ensureConsole()

	// Advertise the protocol revision the host's Dial handshake reads back. Both
	// ends take it from internal/protocol, so a mismatch is now only possible
	// between DIFFERENT builds — which is exactly what the handshake is for.
	if err := os.Setenv(protocol.RevEnvVar, protocol.Revision); err != nil {
		return fmt.Errorf("advertising proto rev: %w", err)
	}

	// PID 1 must not be killed by a broken stdio/vsock pipe.
	signal.Ignore(syscall.SIGPIPE)

	// Zero umask so extracted file modes (setuid/setgid/sticky) land exactly as
	// the archive specifies, not masked by an inherited umask.
	unix.Umask(0)

	if err := mountBaseFilesystems(); err != nil {
		return fmt.Errorf("base mounts: %w", err)
	}

	if err := setupAgentCgroup(); err != nil {
		return fmt.Errorf("agent cgroup: %w", err)
	}

	// Start materializing the container rootfs NOW, from the kernel-cmdline
	// plan — before the agent serves, so extraction overlaps the host's
	// dial/handshake and network setup instead of waiting for an RPC. After
	// setupAgentCgroup on purpose: the tmpfs pages it writes are charged to
	// the agent cgroup, not the root one.
	rootfsJob := guestagent.BeginRootfsMaterialization()

	// The vsock device may still be probing (async device probes race init);
	// a PID-1 failure here would panic=-1 into a silent reboot lottery, so
	// wait for the device rather than die.
	listener, err := listenVsockRetry(protocol.VsockPort, deviceWaitBudget)
	if err != nil {
		return fmt.Errorf("vsock listen :%d: %w", protocol.VsockPort, err)
	}

	// The control channel is the HOST's alone. AF_VSOCK is not namespaced and
	// containers get no netns, so without this a container process could dial
	// the guest's own CID (the kernel is built with CONFIG_VSOCKETS_LOOPBACK)
	// and reach this unauthenticated root API — Mount/WriteFile/CreateProcess —
	// collapsing the container boundary onto the VM boundary.
	guarded := hostOnlyListener{listener}

	agent := guestagent.New(rootfsJob)
	shim := guestagent.NewConnectShim(agent)

	// connect over plain HTTP/1.1 on the vsock listener. No h2c: every RPC here
	// is unary except Copy, which is server-streaming — and Connect streams that
	// over HTTP/1.1 chunked encoding. Only BIDI streaming would force HTTP/2,
	// and nothing in this contract is bidi. Concurrency is per-connection:
	// WaitProcess blocks for the container's whole life while Kill, Resize and
	// CloseProcessStdin still get served, because the client opens another
	// connection rather than queueing behind it.
	mux := http.NewServeMux()
	// The read ceiling enforces the "bulk data belongs on Copy" rule stated
	// above: Copy moves bytes via an attached device, so no legitimate request
	// approaches this. grpc's old 4 MiB default did the same job implicitly;
	// Connect applies no limit unless told, and an unbounded WriteFile would
	// otherwise buffer arbitrarily large bodies in PID 1's memory. Here and
	// not in internal/httpd, so the error names the RPC (httpd.go says so).
	mux.Handle(sandboxconnect.NewSandboxContextHandler(shim, connect.WithReadMaxBytes(maxRequestBytes)))

	// internal/httpd, not http.Server: naming http.Server links crypto/tls (its
	// conn.serve type-asserts to *tls.Conn), which costs 1.50 MiB in a guest that
	// speaks vsock and will never negotiate TLS. See that package's doc.
	server := &httpd.Server{
		Handler:           mux,
		ReadHeaderTimeout: serverHeaderTimeout,
		Logf:              log.Printf,
	}

	log.Printf("serving SandboxContext on vsock :%d", protocol.VsockPort)

	// Serve never returns nil — its only exit is an accept failure (which
	// includes the listener closing) — so the wrap is unconditional and a
	// nil-check would be vacuous (staticcheck SA4023 proves it).
	return fmt.Errorf("serve: %w", server.Serve(guarded))
}

// hostOnlyListener drops any accepted connection whose peer is not the host.
// Rejecting inside Accept (rather than in a Connect interceptor) keeps the guard
// ahead of any request parsing, so a local caller never gets far enough to send
// a request, and http.Serve keeps serving: a rejected peer is a dropped
// connection, not a server error.
type hostOnlyListener struct {
	net.Listener
}

func (l hostOnlyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			//nolint:wrapcheck // http.Serve inspects this error (net.Error); wrapping hides it.
			return nil, err
		}

		addr, ok := conn.RemoteAddr().(*vsock.Addr)
		if ok && addr.ContextID == vsock.Host {
			return conn, nil
		}

		// Not the host: a container process, or an address type we cannot
		// vouch for. Refuse both — this must fail closed.
		log.Printf("rejected non-host vsock peer %s on :%d", conn.RemoteAddr(), protocol.VsockPort)
		_ = conn.Close()
	}
}

// mountBaseFilesystems mounts the pseudo-filesystems the agent needs, mirroring
// Apple's vminitd: proc, a tmpfs on /run (the root is read-only ext4, so /run
// must be writable for container state), sysfs, then cgroup2. The mountpoint
// directories come baked into the initfs (see tools/build-initfs); mounting onto them
// does not write the read-only root.
func mountBaseFilesystems() error {
	const nodev = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC

	// proc first: anything reading /proc (and Go itself) wants it present.
	if err := unix.Mount("proc", "/proc", "proc", nodev, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	// /dev is best-effort: the kernel normally auto-mounts devtmpfs
	// (CONFIG_DEVTMPFS_MOUNT), so an already-mounted /dev returns EBUSY.
	if err := unix.Mount(
		"devtmpfs",
		"/dev",
		"devtmpfs",
		unix.MS_NOSUID,
		"mode=0755",
	); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("warning: mount devtmpfs on /dev: %v", err)
	}

	required := []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"sysfs", "/sys", "sysfs", nodev, ""},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", nodev, ""}, // needs /sys first
	}
	for _, m := range required {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}

	// binfmt_misc is best-effort: only --rosetta (SetupEmulator) needs it, and
	// it depends on /proc being mounted (above).
	if err := unix.Mount("binfmt_misc", "/proc/sys/fs/binfmt_misc", "binfmt_misc", nodev, ""); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		log.Printf("warning: mount binfmt_misc: %v", err)
	}

	return nil
}

// setupAgentCgroup places PID 1 in its own cgroup v2 with the memory controller
// enabled, mirroring Apple's vminitd (/sys/fs/cgroup/vminitd). Nothing on the
// host reaches into it: this agent never self-throttles (Apple's capped itself
// at 80 MiB, which is why their host lifted memory.high before extraction — the
// RPC that did so is gone). What the cgroup still buys is accounting: the
// container-materialization tmpfs pages are charged here, not to the root cgroup.
func setupAgentCgroup() error {
	const cgRoot = "/sys/fs/cgroup"

	// cgroupDirMode matches the perms cgroupfs gives its own auto-created
	// groups; the kernel, not the mode, gates writes to cgroup control files.
	const cgroupDirMode = 0o755

	// Enable the memory controller for child cgroups so /vminitd gets memory.*
	// files. The root cgroup is exempt from the no-internal-processes rule, so
	// this succeeds with PID 1 still resident in the root cgroup.
	if err := echoTo(cgRoot+"/cgroup.subtree_control", "+memory"); err != nil {
		return fmt.Errorf("enable memory controller: %w", err)
	}

	agentCgroup := cgRoot + "/vminitd"
	if err := os.Mkdir(agentCgroup, cgroupDirMode); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create %s: %w", agentCgroup, err)
	}

	if err := echoTo(agentCgroup+"/cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return fmt.Errorf("join %s: %w", agentCgroup, err)
	}

	return nil
}

// echoTo writes val to a cgroup control file (O_WRONLY, no create/truncate,
// which cgroupfs pseudo-files require).
func echoTo(path, val string) error {
	// The only paths passed in are fixed cgroupfs control files assembled from
	// constants by PID 1 itself.
	ctrlFile, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- see above
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = ctrlFile.Close() }()

	if _, err := ctrlFile.WriteString(val); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// NOTE: no blanket wait4(-1) reaper here — it would race os/exec's cmd.Wait for
// container processes and steal their exit status. Container descendants are
// reaped inside each container's own PID namespace (its PID 1), and the only
// direct children of the agent are the stage2 processes, which the process
// table reaps via cmd.Wait in WaitProcess.
