//go:build linux

package guestagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/mdlayher/vsock"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/farcloser/ossein/guest/internal/vmexec"
	pb "github.com/farcloser/ossein/internal/sandbox"
)

// specDir holds per-process OCI configs handed to the stage2 re-exec.
const specDir = "/run/ossein"

// specFileMode keeps per-process OCI configs readable by root only.
const specFileMode = 0o600

// decimalBase is the base for stdio port numbers on the stage2 command line.
const decimalBase = 10

// managedProcess is one container process the agent has created. The runtime
// work happens in a re-exec'd `stage2` child (the vmexec port); the agent holds
// its handle plus the start gate and reaps its exit.
type managedProcess struct {
	cmd      *exec.Cmd
	startW   *os.File // write a byte to release stage2's pre-exec gate
	specPath string   // per-process OCI config under specDir; removed on reap

	// TTY only: the pty master (for window resize) and the stdin relay
	// connection (for CloseProcessStdin). nil for non-TTY processes.
	ttyMaster   *os.File
	stdinCloser io.Closer

	waitOnce sync.Once
	waitErr  error
	exitCode int32
	exitedAt time.Time
	done     chan struct{}
}

// processTable is the agent's registry of live processes, keyed by
// containerID/id.
type processTable struct {
	mu sync.Mutex
	m  map[string]*managedProcess
}

func newProcessTable() *processTable { return &processTable{m: map[string]*managedProcess{}} }

func procKey(id, containerID string) string {
	if containerID == "" {
		containerID = id
	}

	return containerID + "/" + id
}

// CreateProcess sets up a container process up to just before exec: it re-execs
// /proc/self/exe as `stage2` in the spec's namespaces (Cloneflags), wires the
// container's stdio to the host's vsock ports, runs the full childSetup
// (mounts, pivot_root, caps, uid), then blocks on a start gate. It returns once
// the child reports ready.
func (a *Agent) CreateProcess(ctx context.Context, req *pb.CreateProcessRequest) (*pb.CreateProcessResponse, error) {
	key := procKey(req.GetId(), req.GetContainerID())

	a.procs.mu.Lock()
	_, exists := a.procs.m[key]
	a.procs.mu.Unlock()

	if exists {
		return nil, rpcErrorf(connect.CodeAlreadyExists, "process %s already exists", key)
	}

	var spec specs.Spec
	if err := json.Unmarshal(req.GetConfiguration(), &spec); err != nil {
		return nil, rpcErrorf(connect.CodeInvalidArgument, "parse OCI spec: %v", err)
	}

	if err := checkUnsupported(&spec); err != nil {
		return nil, err
	}

	flags, err := cloneFlags(&spec)
	if err != nil {
		return nil, rpcErrorf(connect.CodeInvalidArgument, "%v", err)
	}

	// 0o755 matches the standard /run directory mode; the specs inside are 0o600.
	if err := os.MkdirAll(specDir, stdDirMode); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "create spec dir: %v", err)
	}

	specPath := filepath.Join(specDir, strings.ReplaceAll(key, "/", "_")+".json")
	if err := os.WriteFile(specPath, req.GetConfiguration(), specFileMode); err != nil {
		_ = os.Remove(specPath) // a failed write can leave a partial file

		return nil, rpcErrorf(connect.CodeInternal, "write spec: %v", err)
	}

	// Everything created between here and a successful cmd.Start must be
	// released on failure: pipes, the console socketpair, and the spec file.
	var opened []*os.File

	abort := func(err error) error {
		for _, f := range opened {
			_ = f.Close()
		}

		_ = os.Remove(specPath)

		return err
	}

	// Pipes shared with stage2: start (agent→child, release exec), ready and
	// error (child→agent). They become fds 3,4,5 in the child via ExtraFiles.
	startR, startW, err := osPipe()
	if err != nil {
		return nil, abort(err)
	}

	opened = append(opened, startR, startW)

	readyR, readyW, err := osPipe()
	if err != nil {
		return nil, abort(err)
	}

	opened = append(opened, readyR, readyW)

	errR, errW, err := osPipe()
	if err != nil {
		return nil, abort(err)
	}

	opened = append(opened, errR, errW)

	// Re-exec'ing our own binary as stage2 is the runtime's design: the child
	// runs trusted code (this executable) with agent-built arguments, and its
	// lifetime is owned by the process table (WaitProcess/KillProcess), not this
	// RPC's context — CommandContext would kill it when CreateProcess returns.
	// #nosec G204 -- see above
	cmd := exec.Command("/proc/self/exe", stage2Args(specPath, &spec, req)...) //nolint:noctx // see above
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, Setsid: true}
	cmd.ExtraFiles = []*os.File{startR, readyW, errW} // → child fds 3,4,5
	// stage2 failures before its error pipe is usable (flag parse, very early
	// setup) must not vanish; the agent's stderr is the serial console.
	cmd.Stderr = os.Stderr

	// TTY: a socketpair over which stage2 hands us the pty master (fd 6).
	// CLOEXEC keeps the agent-side end out of unrelated children; ExtraFiles
	// dup()s the child end past the flag.
	tty := spec.Process != nil && spec.Process.Terminal

	var consoleAgent, consoleChild *os.File

	if tty {
		pair, perr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if perr != nil {
			return nil, abort(rpcErrorf(connect.CodeInternal, "console socketpair: %v", perr))
		}

		consoleAgent = os.NewFile(uintptr(pair[0]), "console-agent")
		consoleChild = os.NewFile(uintptr(pair[1]), "console-child")
		opened = append(opened, consoleAgent, consoleChild)
		cmd.ExtraFiles = append(cmd.ExtraFiles, consoleChild)
	}

	if err := cmd.Start(); err != nil {
		return nil, abort(rpcErrorf(connect.CodeInternal, "start stage2: %v", err))
	}

	// Parent keeps only its ends.
	_ = startR.Close()

	_ = readyW.Close()

	_ = errW.Close()
	if consoleChild != nil {
		_ = consoleChild.Close()
	}
	defer func() { _ = readyR.Close(); _ = errR.Close() }()

	proc := &managedProcess{cmd: cmd, startW: startW, specPath: specPath, done: make(chan struct{})}

	// kill releases every process-side resource after a post-Start failure:
	// closing startW unblocks a healthy child's start gate, SIGKILL covers a
	// wedged one, and cmd.Wait reaps either way.
	kill := func() {
		_ = startW.Close()
		if consoleAgent != nil {
			_ = consoleAgent.Close()
		}

		if proc.ttyMaster != nil {
			_ = proc.ttyMaster.Close()
		}

		if proc.stdinCloser != nil {
			_ = proc.stdinCloser.Close()
		}

		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(specPath)
	}

	if err := awaitStage2Ready(ctx, readyR, errR); err != nil {
		kill()

		return nil, err
	}

	if tty {
		masterFD, ferr := recvFD(consoleAgent)
		_ = consoleAgent.Close()
		consoleAgent = nil

		if ferr != nil {
			kill()

			return nil, rpcErrorf(connect.CodeInternal, "receive pty master: %v", ferr)
		}

		proc.ttyMaster = os.NewFile(uintptr(masterFD), "pty-master")
		if err := startTTYRelay(proc, req); err != nil {
			kill()

			return nil, rpcErrorf(connect.CodeInternal, "tty relay: %v", err)
		}
	}

	a.procs.mu.Lock()
	if _, dup := a.procs.m[key]; dup {
		a.procs.mu.Unlock()
		kill()

		return nil, rpcErrorf(connect.CodeAlreadyExists, "process %s already exists", key)
	}

	a.procs.m[key] = proc
	a.procs.mu.Unlock()

	return &pb.CreateProcessResponse{}, nil
}

// awaitStage2Ready blocks until the child signals ready, dies during setup, or
// the RPC is cancelled. The pty master (TTY) is buffered in the socketpair, so
// it is safe to receive after ready; this way a setup failure surfaces via the
// error pipe, not a stuck recvFD.
func awaitStage2Ready(ctx context.Context, readyR, errR *os.File) error {
	readyCh := make(chan error, 1)
	go func() {
		_, rerr := io.ReadFull(readyR, make([]byte, 1))
		readyCh <- rerr
	}()

	select {
	case <-ctx.Done():
		// The context's status error must reach the client unwrapped so its
		// code (Canceled/DeadlineExceeded) survives.
		return rpcContextError(ctx.Err())
	case rerr := <-readyCh:
		if rerr == nil {
			return nil
		}

		msg, _ := io.ReadAll(errR)

		detail := strings.TrimSpace(string(msg))
		if detail == "" {
			detail = rerr.Error()
		}

		return rpcErrorf(connect.CodeInternal, "stage2 setup: %s", detail)
	}
}

// checkUnsupported rejects the spec features this runtime does not implement
// AND cannot safely ignore. It is deliberately not a general gate: stage2 also
// drops linux.devices, rootfsPropagation and the selinux/apparmor labels, and
// those fail CLOSED — a caller that asked for them gets less access than it
// wanted, which is surprising but never unsafe. The checks below are the cases
// where silence would be wrong. Cgroup limits, because sizing is the host's
// job: it sizes the microVM and the workload owns all of it, so a guest-side
// limit would contradict the host's accounting. Seccomp, because ignoring it
// returns a container with MORE syscall surface than the spec asked for: a
// fail-open security control must never be dropped quietly, and a caller that
// believes it is filtered is worse off than one told it cannot be.
func checkUnsupported(spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}

	if spec.Linux.CgroupsPath != "" {
		return rpcErrorf(
			connect.CodeUnimplemented,
			"linux.cgroupsPath is not supported by this runtime",
		)
	}

	if res := spec.Linux.Resources; res != nil && !reflect.DeepEqual(*res, specs.LinuxResources{}) {
		return rpcErrorf(
			connect.CodeUnimplemented,
			"linux.resources is not supported by this runtime",
		)
	}

	if spec.Linux.Seccomp != nil {
		return rpcErrorf(
			connect.CodeUnimplemented,
			"linux.seccomp is not supported by this runtime: no syscall filtering is applied, "+
				"so the profile would be silently ignored",
		)
	}

	return nil
}

// startTTYRelay pipes the pty master to the host's stdin/stdout vsock ports.
// The stdin connection is retained so CloseProcessStdin can end it. req.Stderr
// is intentionally unused here: a pty merges the workload's stdout and stderr
// into the single master stream.
func startTTYRelay(proc *managedProcess, req *pb.CreateProcessRequest) error {
	master := proc.ttyMaster

	if req.Stdout != nil {
		out, err := vsock.Dial(vsock.Host, req.GetStdout(), nil)
		if err != nil {
			return fmt.Errorf("dial stdout vsock :%d: %w", req.GetStdout(), err)
		}

		go func() { _, _ = io.Copy(out, master); _ = out.Close() }()
	}

	if req.Stdin != nil {
		stdinConn, err := vsock.Dial(vsock.Host, req.GetStdin(), nil)
		if err != nil {
			return fmt.Errorf("dial stdin vsock :%d: %w", req.GetStdin(), err)
		}

		proc.stdinCloser = stdinConn
		go func() { _, _ = io.Copy(master, stdinConn) }()
	}

	return nil
}

// resizeErrFmt is the status message format for pty resize failures.
const resizeErrFmt = "resize: %v"

// ResizeProcess resizes the pty (TTY processes only; a no-op otherwise).
func (a *Agent) ResizeProcess(_ context.Context, req *pb.ResizeProcessRequest) (*pb.ResizeProcessResponse, error) {
	proc, err := a.lookup(req.GetId(), req.GetContainerID())
	if err != nil {
		return nil, err
	}

	if proc.ttyMaster == nil {
		return &pb.ResizeProcessResponse{}, nil
	}

	// The kernel's winsize fields are u16; terminal dimensions beyond 65535
	// do not exist, so the narrowing conversions cannot lose real values.
	// #nosec G115 -- see above
	winsize := &unix.Winsize{
		Row: uint16(req.GetRows()),
		Col: uint16(req.GetColumns()),
	}

	rawConn, err := proc.ttyMaster.SyscallConn()
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, resizeErrFmt, err)
	}

	var ioErr error

	if err := rawConn.Control(
		func(fd uintptr) { ioErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, winsize) },
	); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, resizeErrFmt, err)
	}

	if ioErr != nil {
		return nil, rpcErrorf(connect.CodeInternal, resizeErrFmt, ioErr)
	}

	return &pb.ResizeProcessResponse{}, nil
}

// CloseProcessStdin ends stdin. For TTY it closes the stdin relay; for non-TTY
// the host already closes its vsock end, which the container's fd 0 sees as EOF.
func (a *Agent) CloseProcessStdin(
	_ context.Context,
	req *pb.CloseProcessStdinRequest,
) (*pb.CloseProcessStdinResponse, error) {
	proc, err := a.lookup(req.GetId(), req.GetContainerID())
	if err != nil {
		return nil, err
	}

	if proc.stdinCloser != nil {
		_ = proc.stdinCloser.Close()
	}

	return &pb.CloseProcessStdinResponse{}, nil
}

// recvFD receives a single file descriptor over a SCM_RIGHTS unix socket. The
// received fd arrives close-on-exec so it cannot leak into other children.
func recvFD(sock *os.File) (int, error) {
	oob := make([]byte, unix.CmsgSpace(4))

	// Only the ancillary length and the error matter for an SCM_RIGHTS-only
	// read; the data byte, flags and source address are deliberately dropped.
	//nolint:dogsled // see above
	_, oobn, _, _, err := unix.Recvmsg(
		int(sock.Fd()),
		make([]byte, 1),
		oob,
		unix.MSG_CMSG_CLOEXEC,
	)
	if err != nil {
		return -1, fmt.Errorf("recvmsg: %w", err)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parse control message: %w", err)
	}

	if len(scms) == 0 {
		return -1, rpcErrorf(connect.CodeInternal, "no control message")
	}

	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("parse unix rights: %w", err)
	}

	if len(fds) == 0 {
		return -1, rpcErrorf(connect.CodeInternal, "no fd in control message")
	}

	return fds[0], nil
}

// StartProcess releases the pre-exec gate and returns the child pid.
func (a *Agent) StartProcess(_ context.Context, req *pb.StartProcessRequest) (*pb.StartProcessResponse, error) {
	proc, err := a.lookup(req.GetId(), req.GetContainerID())
	if err != nil {
		return nil, err
	}

	if _, err := proc.startW.Write([]byte{1}); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "release start gate: %v", err)
	}

	_ = proc.startW.Close()

	// Linux pids are bounded by PID_MAX_LIMIT (2^22), far inside int32.
	return &pb.StartProcessResponse{Pid: int32(proc.cmd.Process.Pid)}, nil // #nosec G115 -- see above
}

// WaitProcess blocks until the process exits and returns its exit code and
// exit time. Reaping also retires the process: the table entry, the pty
// master/stdin relay, and the per-process spec file are all released.
func (a *Agent) WaitProcess(_ context.Context, req *pb.WaitProcessRequest) (*pb.WaitProcessResponse, error) {
	proc, err := a.lookup(req.GetId(), req.GetContainerID())
	if err != nil {
		return nil, err
	}

	proc.waitOnce.Do(func() {
		proc.waitErr = proc.cmd.Wait()
		proc.exitedAt = time.Now()
		proc.exitCode = exitCode(proc.cmd.ProcessState)

		if proc.ttyMaster != nil {
			_ = proc.ttyMaster.Close()
		}

		if proc.stdinCloser != nil {
			_ = proc.stdinCloser.Close()
		}

		_ = os.Remove(proc.specPath)

		a.procs.mu.Lock()
		delete(a.procs.m, procKey(req.GetId(), req.GetContainerID()))
		a.procs.mu.Unlock()

		close(proc.done)
	})
	<-proc.done

	// A failed wait (e.g. ECHILD) leaves no ProcessState: there is no real
	// exit code to report, and fabricating 0 would tell the host the workload
	// succeeded. Surface the wait error instead.
	if proc.waitErr != nil && proc.cmd.ProcessState == nil {
		return nil, rpcErrorf(connect.CodeInternal, "wait %s: %v", req.GetId(), proc.waitErr)
	}

	return &pb.WaitProcessResponse{ExitCode: proc.exitCode, ExitedAt: timestamppb.New(proc.exitedAt)}, nil
}

// exitCode maps a reaped process state to the wire exit code: a signal-killed
// workload reports 128+signal (the shell convention — 137 for OOM-kill/SIGKILL,
// 143 for SIGTERM), never Go's -1.
// signalExitBase is the shell convention base for signal deaths (128+signal).
const signalExitBase = 128

func exitCode(state *os.ProcessState) int32 {
	// nil state means Wait itself failed (ECHILD and kin) — there is no
	// status. WaitProcess returns proc.waitErr on that path; this placeholder
	// is never sent on the wire.
	if state == nil {
		return -1
	}

	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		// Signal numbers top out at 64 (SIGRTMAX); 128+signal always fits int32.
		return signalExitBase + int32(ws.Signal()) // #nosec G115 -- see above
	}

	// Wait exit codes are 0-255 by definition; the conversion cannot overflow.
	return int32(state.ExitCode()) // #nosec G115 -- see above
}

// KillProcess sends a signal to the process.
func (a *Agent) KillProcess(_ context.Context, req *pb.KillProcessRequest) (*pb.KillProcessResponse, error) {
	proc, err := a.lookup(req.GetId(), req.GetContainerID())
	if err != nil {
		return nil, err
	}

	if err := proc.cmd.Process.Signal(syscall.Signal(req.GetSignal())); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "kill: %v", err)
	}

	return &pb.KillProcessResponse{}, nil
}

func (a *Agent) lookup(processID, containerID string) (*managedProcess, error) {
	a.procs.mu.Lock()
	defer a.procs.mu.Unlock()

	proc, ok := a.procs.m[procKey(processID, containerID)]
	if !ok {
		return nil, rpcErrorf(connect.CodeNotFound, "no such process %s", procKey(processID, containerID))
	}

	return proc, nil
}

// stage2Args builds the re-exec argv for the runtime child. For a TTY the agent
// owns stdio (it relays the pty master), so only --tty is passed; otherwise the
// child dials the stdio vsock ports itself.
func stage2Args(specPath string, spec *specs.Spec, req *pb.CreateProcessRequest) []string {
	args := []string{vmexec.Stage2Command, "--spec", specPath}

	if spec.Process != nil && spec.Process.Terminal {
		return append(args, "--tty")
	}

	if req.Stdin != nil {
		args = append(args, "--stdin", strconv.FormatUint(uint64(req.GetStdin()), decimalBase))
	}

	if req.Stdout != nil {
		args = append(args, "--stdout", strconv.FormatUint(uint64(req.GetStdout()), decimalBase))
	}

	if req.Stderr != nil {
		args = append(args, "--stderr", strconv.FormatUint(uint64(req.GetStderr()), decimalBase))
	}

	return args
}

// cloneFlags maps the spec's pathless namespaces to clone flags. Namespaces
// with a path (join-existing) are the exec path, not create. Only PID, IPC,
// UTS and mount namespaces are supported — the microVM is the isolation
// boundary for network, user, cgroup and time — so any other namespace entry
// is an error, never a silent drop. Default matches vmexec.
func cloneFlags(spec *specs.Spec) (uintptr, error) {
	// A mount namespace is non-negotiable regardless of the spec: stage2
	// always re-plumbs mounts (MS_SLAVE, pivot_root), which must never reach
	// the VM's real mount tree.
	flags := uintptr(unix.CLONE_NEWNS)

	if spec.Linux == nil || len(spec.Linux.Namespaces) == 0 {
		return flags | unix.CLONE_NEWPID | unix.CLONE_NEWUTS, nil
	}

	table := map[specs.LinuxNamespaceType]uintptr{
		specs.PIDNamespace:   unix.CLONE_NEWPID,
		specs.MountNamespace: unix.CLONE_NEWNS,
		specs.UTSNamespace:   unix.CLONE_NEWUTS,
		specs.IPCNamespace:   unix.CLONE_NEWIPC,
	}

	for _, namespace := range spec.Linux.Namespaces {
		flag, ok := table[namespace.Type]
		if !ok {
			return 0, fmt.Errorf("%w: %q", errUnsupportedNamespace, namespace.Type)
		}

		if namespace.Path == "" {
			flags |= flag
		}
	}

	return flags, nil
}

func osPipe() (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, rpcErrorf(connect.CodeInternal, "pipe: %v", err)
	}

	return r, w, nil
}
