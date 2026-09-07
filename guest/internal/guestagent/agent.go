//go:build linux

// Package guestagent implements the SandboxContext service inside the
// guest microVM. The host (ossein) is the client; every method here is a remote
// operation the host invokes to build up and run the container: filesystem
// setup (Mkdir/Mount/WriteFile), rootfs copy-in (Copy), network configuration,
// vsock proxies, and the container process lifecycle
// (Create/Start/Wait/Kill/Resize) backed by the stage2 runtime.
//
// Agent methods take and return the plain protobuf types; the connect wire
// shapes live in connectshim.go, which adapts them. That split keeps the
// handlers free of transport types — the same reason they were readable under
// Connect — and means a transport change touches one file, not twenty.
package guestagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/vmexec"
	pb "github.com/farcloser/ossein/internal/sandbox"
)

// Agent is the SandboxContext server implementation.
type Agent struct {
	procs   *processTable
	proxies *proxyTable
	// rootfs is the boot-time materialization started by vminitd before the
	// agent serves; nil when the kernel cmdline carried no plan. Copy awaits
	// it.
	rootfs *RootfsJob
}

// New returns a ready agent. rootfs may be nil (no plan on the cmdline).
func New(rootfs *RootfsJob) *Agent {
	return &Agent{procs: newProcessTable(), proxies: newProxyTable(), rootfs: rootfs}
}

// Getenv returns a guest environment variable. The host's dial handshake is one
// call to this RPC — reading protocol.RevEnvVar — which proves liveness AND
// protocol compatibility in a single round-trip, so it must never regress. An
// unset key yields an absent value, not an error.
func (*Agent) Getenv(_ context.Context, req *pb.GetenvRequest) (*pb.GetenvResponse, error) {
	resp := &pb.GetenvResponse{}
	if val, ok := os.LookupEnv(req.GetKey()); ok {
		resp.Value = &val
	}

	return resp, nil
}

// stdDirMode and stdFileMode are the standard world-readable modes for
// directories and files created inside the container filesystem; the
// container's non-root processes must be able to traverse/read them.
const (
	stdDirMode  = 0o755
	stdFileMode = 0o644
)

// unixToFileMode converts raw unix mode bits to an os.FileMode, keeping
// setuid/setgid/sticky — os.ModePerm alone would strip them.
func unixToFileMode(mode uint32) os.FileMode {
	fileMode := os.FileMode(mode) & os.ModePerm
	if mode&unix.S_ISUID != 0 {
		fileMode |= os.ModeSetuid
	}

	if mode&unix.S_ISGID != 0 {
		fileMode |= os.ModeSetgid
	}

	if mode&unix.S_ISVTX != 0 {
		fileMode |= os.ModeSticky
	}

	return fileMode
}

// Mkdir creates a directory, optionally with parents (like MkdirAll). The full
// requested mode applies, setgid/sticky included (the agent runs umask 0).
func (*Agent) Mkdir(_ context.Context, req *pb.MkdirRequest) (*pb.MkdirResponse, error) {
	perm := unixToFileMode(req.GetPerms())

	var err error
	if req.GetAll() {
		err = os.MkdirAll(req.GetPath(), perm)
	} else {
		err = os.Mkdir(req.GetPath(), perm)
	}

	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "mkdir %s: %v", req.GetPath(), err)
	}

	return &pb.MkdirResponse{}, nil
}

// Mount performs a mount(2) with the given type/source/destination and OCI-style
// string options split into flags and data. A source of the form
// "virtio-serial:<id>" names a virtio-blk device by SERIAL and is resolved
// via sysfs first — device names follow probe-completion order and are
// nondeterministic under async probing, so the host never sends a vdX path.
func (*Agent) Mount(_ context.Context, req *pb.MountRequest) (*pb.MountResponse, error) {
	flags, data := vmexec.ParseMountOptions(req.GetOptions())

	source := req.GetSource()
	if serial, ok := strings.CutPrefix(source, serialSourcePrefix); ok {
		resolved, err := resolveVirtioBlkPath(serial, deviceWaitBudget)
		if err != nil {
			return nil, rpcErrorf(connect.CodeNotFound, "mount: %v", err)
		}

		source = resolved
	}

	if err := unix.Mount(source, req.GetDestination(), req.GetType(), flags, data); err != nil {
		return nil, rpcErrorf(connect.CodeInternal,
			"mount %s (%s) on %s: %v", source, req.GetType(), req.GetDestination(), err)
	}

	if req.GetType() == "virtiofs" {
		tuneVirtiofsReadahead(req.GetDestination())
	}

	return &pb.MountResponse{}, nil
}

// virtiofsReadaheadKB is the per-mount readahead we set on virtio-fs shares. The virtio-fs bdi
// ships read_ahead_kb=128 (vs 8192 for virtio-blk), which throttles sequential reads off a
// share. Reading a file whole, binary on tmpfs so only the data path is measured:
//
//	128 (default) -> 578us, 4130 MiB/s
//	1024          -> 458us, 5220 MiB/s   (-21% latency, +26% bandwidth)
//	8192          -> 474us, 5040 MiB/s   (over-reads: slightly WORSE than 1024)
//
// 1024 is the measured optimum, not a compromise — 8192 pulls pages that go unused and costs
// bandwidth back, which also matters on a fixed-RAM, no-swap guest. Metadata (open+fstat) is
// unaffected: readahead is a data-path knob. Applies to every share, Rosetta included (harmless
// — it is read-only executable data). It does NOT fix exec-from-mount (that is per-spawn demand
// paging of the binary, a separate VZ virtio-fs cost documented at the host mount site).
const virtiofsReadaheadKB = 1024

// tuneVirtiofsReadahead raises read_ahead_kb on the bdi backing a freshly-mounted virtio-fs
// share. Best-effort: a failure here is logged and never fails the mount.
func tuneVirtiofsReadahead(mountpoint string) {
	var stat unix.Stat_t
	if err := unix.Stat(mountpoint, &stat); err != nil {
		log.Printf("warning: virtio-fs readahead: stat %s: %v", mountpoint, err)

		return
	}

	// The FUSE/virtio-fs bdi is named by the mount's superblock device number:
	// /sys/class/bdi/<major>:<minor>/read_ahead_kb.
	bdi := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(stat.Dev), unix.Minor(stat.Dev))
	if err := os.WriteFile(bdi, []byte(strconv.Itoa(virtiofsReadaheadKB)), 0); err != nil {
		log.Printf("warning: virtio-fs readahead: write %s: %v", bdi, err)
	}
}

// WriteFile writes data to path, honoring the create-parents / create-if-missing
// / append flags from the request.
func (*Agent) WriteFile(_ context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	path := req.GetPath()
	flags := req.GetFlags() // nil-safe getters below

	if flags.GetCreateParentDirs() {
		// 0o755 is the standard mode for implicitly created parent directories
		// inside the container filesystem; 0o750 would break non-root workloads.
		if err := os.MkdirAll(filepath.Dir(path), stdDirMode); err != nil {
			return nil, rpcErrorf(connect.CodeInternal, "create parents for %s: %v", path, err)
		}
	}

	openFlags := os.O_WRONLY | os.O_TRUNC
	if flags.GetCreateIfMissing() {
		openFlags |= os.O_CREATE
	}

	if flags.GetAppend() {
		openFlags = openFlags&^os.O_TRUNC | os.O_APPEND
	}

	mode := unixToFileMode(req.GetMode())

	// Opening a host-chosen path is this RPC's entire purpose: the host drives
	// the whole guest filesystem over SandboxContext.
	file, err := os.OpenFile(path, openFlags, mode) // #nosec G304 -- see above
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	// OpenFile's mode only applies on create; enforce it for existing files.
	if err := file.Chmod(mode); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "chmod %s: %v", path, err)
	}

	if _, err := file.Write(req.GetData()); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "write %s: %v", path, err)
	}

	return &pb.WriteFileResponse{}, nil
}
