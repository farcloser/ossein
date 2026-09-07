//go:build linux

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/guest/internal/vmexec"
	"github.com/farcloser/ossein/internal/rootfsblob"
	pb "github.com/farcloser/ossein/internal/sandbox"
)

// maxprocsOnce drops GOMAXPROCS to 1 exactly once, after the rootfs is
// materialized. The boot value stays at NumCPU until then; afterwards PID 1
// only supervises the container and proxies its stdio, so there is no reason
// to spread the Go runtime (GC, sysmon, scheduler Ps) across both vCPUs and
// steal cycles from the workload. Blocking I/O (the vsock stdio proxy) still
// parallelizes via the runtime's syscall threads.
//
//nolint:gochecknoglobals // process-wide one-shot; the GOMAXPROCS drop must happen exactly once per process
var maxprocsOnce sync.Once

// RootfsJob is the boot-time rootfs materialization vminitd starts at init
// from the kernel-cmdline plan, BEFORE the agent serves — so it overlaps the
// host's dial/handshake and everything after it. The Copy RPC merely awaits
// it. serial identifies the blob's virtio-blk device.
type RootfsJob struct {
	serial string
	dest   string
	opts   string
	done   chan struct{}
	err    error
}

// BeginRootfsMaterialization reads the ossein.rootfs=<device>:<dest>:<opts>
// kernel parameter and, when present, starts materializing the rootfs in the
// background. Returns nil when the cmdline carries no plan (a VM booted for
// something other than running a container); the Copy RPC then refuses.
func BeginRootfsMaterialization() *RootfsJob {
	raw, err := os.ReadFile(procCmdline)
	if err != nil {
		log.Printf("rootfs plan: reading cmdline: %v", err)

		return nil
	}

	for field := range strings.FieldsSeq(string(raw)) {
		plan, ok := strings.CutPrefix(field, rootfsPlanParam)
		if !ok {
			continue
		}

		parts := strings.SplitN(plan, ":", rootfsPlanParts)
		if len(parts) != rootfsPlanParts {
			log.Printf("rootfs plan: malformed %q", plan)

			return nil
		}

		job := &RootfsJob{serial: parts[0], dest: parts[1], opts: parts[2], done: make(chan struct{})}

		go job.run()

		return job
	}

	return nil
}

func (j *RootfsJob) run() {
	defer close(j.done)

	j.err = materializeRootfs(j.serial, j.dest, j.opts)
	if j.err != nil {
		log.Printf("rootfs materialization: %v", j.err)
	}
}

// Copy implements the streaming Copy RPC. Only COPY_IN of an archive is
// wired, and it moves no data: materialization runs from init (see
// BeginRootfsMaterialization), and this awaits its completion after checking
// that the host's request names the same plan the kernel cmdline delivered —
// a mismatch means host and guest disagree about the boot, which is a bug,
// not a request to serve.
//
// The RPC keeps its archive-shaped name and is_archive flag because the proto
// is vendored from upstream and describes a general copy-in. What ossein
// actually does no longer resembles one; see materializeRootfs.
func (a *Agent) Copy(ctx context.Context, req *pb.CopyRequest, stream *connect.ServerStream[pb.CopyResponse]) error {
	if req.GetDirection() != pb.CopyRequest_COPY_IN {
		return rpcErrorf(connect.CodeInvalidArgument, "copy: only COPY_IN is implemented")
	}

	if !req.GetIsArchive() {
		return rpcErrorf(connect.CodeUnimplemented, "copy: only archive COPY_IN is implemented")
	}

	job := a.rootfs
	if job == nil {
		return rpcErrorf(connect.CodeFailedPrecondition, "copy: no rootfs plan on the kernel cmdline")
	}

	if req.GetPath() != job.dest || req.GetDevice() != job.serial {
		return rpcErrorf(connect.CodeInvalidArgument,
			"copy: request (%s from %s) does not match the boot plan (%s from %s)",
			req.GetPath(), req.GetDevice(), job.dest, job.serial)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("copy: %w", ctx.Err())
	case <-job.done:
	}

	if job.err != nil {
		return fmt.Errorf("copy: %w", job.err)
	}

	return stream.Send(&pb.CopyResponse{Status: pb.CopyResponse_COMPLETE}) //nolint:wrapcheck
}

// materializeRootfs makes the container rootfs appear at dest, as mounts.
//
// The host caches the flattened image as an EROFS filesystem image and
// attaches it read-only as a virtio-blk device at VM creation, so there is no
// host-side data path and nothing to unpack: a tmpfs goes down at dest to
// carry the writable layer, the blob device is mounted read-only as the
// overlay lower, and the merged overlay lands at dest. Cost is two mounts —
// ~100µs, flat in image size — against the 619 ms a rust-sized extraction
// took, and the image no longer has to fit in guest RAM (see
// the rootfs-materialization notes, a local engineering journal).
//
// There is deliberately no fallback. ossein used to also ship a tar+lz4 codec
// whose blobs the guest decoded and extracted into the tmpfs, kept as an
// escape hatch for a guest kernel without EROFS. That scenario cannot arise:
// the kernel is embedded in the binary, pinned by tag and sha256,
// cosign-verified, and its config is asserted against a golden — so the only
// way to reach an EROFS-less guest is to deliberately pass --kernel, where a
// loud mount failure is the right answer. Carrying a whole second
// materialization path for it meant carrying a tar extractor in PID 1, and
// deleting an extractor that never runs removes that entire class of
// archive-parsing exposure from the root-privileged agent. Git history holds
// it if a second path is ever wanted again.
func materializeRootfs(serial, dest, opts string) error {
	if err := os.MkdirAll(dest, stdDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}

	// Same option translation the Mount RPC applies (nodev et al. become
	// flags, the rest mount data). These flags carry the confinement the host
	// asked for and must reach every mount below — see stackOverlay.
	flags, data := vmexec.ParseMountOptions(strings.Split(opts, ","))
	if err := unix.Mount("tmpfs", dest, "tmpfs", flags, data); err != nil {
		return fmt.Errorf("mount rootfs tmpfs on %s: %w", dest, err)
	}

	devicePath, err := resolveVirtioBlkPath(serial, deviceWaitBudget)
	if err != nil {
		return fmt.Errorf("resolve rootfs device %q: %w", serial, err)
	}

	// Classify before mounting. The kernel would reject a non-EROFS blob with
	// a bare EINVAL naming nothing; reading the superblock first costs a
	// single short read and lets the failure say what the blob actually is.
	// The classifier is shared with the host (internal/rootfsblob), which
	// tests its own writer's output against it.
	format, err := sniffRootfsBlob(devicePath)
	if err != nil {
		return err
	}

	if format != rootfsblob.FormatEROFS {
		return fmt.Errorf("%w: %v (the image cache served bytes this guest cannot mount)",
			errUnknownBlobFormat, format)
	}

	return mountEROFSRootfs(devicePath, dest, flags)
}

// sniffRootfsBlob reads the head of the blob device and classifies it.
func sniffRootfsBlob(devicePath string) (rootfsblob.Format, error) {
	device, err := openDeviceRetry(devicePath, deviceWaitBudget)
	if err != nil {
		return rootfsblob.FormatTar, fmt.Errorf("open rootfs device %s: %w", devicePath, err)
	}

	defer func() { _ = device.Close() }()

	head := make([]byte, rootfsblob.SniffLen)

	// ReadFull, not Read: a single Read on a block device may return fewer
	// bytes than asked for even when the rest is right there, and a short
	// head classifies as "not EROFS" — so one Read could send a perfectly
	// good image down the unmountable path.
	//
	// Coming up short is nonetheless legal and not an error: a blob smaller
	// than a superblock simply cannot be EROFS, and Sniff says so rather than
	// guessing. Both EOF shapes mean exactly that.
	read, err := io.ReadFull(device, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return rootfsblob.FormatTar, fmt.Errorf("read rootfs device %s: %w", devicePath, err)
	}

	return rootfsblob.Sniff(head[:read]), nil
}

// rootfsPlanParam is the kernel cmdline parameter carrying the rootfs
// materialization plan; rootfsPlanParts is its ":"-separated arity
// (serial:dest:opts — the blob device is identified by its virtio SERIAL,
// never a vdX name: probe order is nondeterministic under async device
// probing). Keep in lockstep with the host (pkg/container).
const (
	rootfsPlanParam = "ossein.rootfs="
	rootfsPlanParts = 3
)

// serialSourcePrefix marks a Mount RPC source as a virtio serial to resolve
// rather than a device path. Keep in lockstep with the host (pkg/container).
const serialSourcePrefix = "virtio-serial:"

// resolveVirtioBlkPath maps a virtio-blk serial to its /dev node by scanning
// /sys/block/*/serial, waiting out async probing. The sysfs entry and the
// devtmpfs node appear together at disk registration; the small open retry
// in openDeviceRetry covers the sliver between them.
func resolveVirtioBlkPath(serial string, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)

	for {
		entries, err := os.ReadDir("/sys/block")
		if err == nil {
			for _, entry := range entries {
				raw, readErr := os.ReadFile(
					"/sys/block/" + entry.Name() + "/serial",
				)
				if readErr != nil {
					continue
				}

				if strings.TrimSpace(string(raw)) == serial {
					return "/dev/" + entry.Name(), nil
				}
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: no virtio-blk device with serial %q", os.ErrNotExist, serial)
		}

		time.Sleep(devicePollInterval)
	}
}

// deviceWaitBudget/devicePollInterval bound the wait for an async-probed
// device node to appear in devtmpfs (mirrors vminitd's init-time waits).
const (
	deviceWaitBudget   = 2 * time.Second
	devicePollInterval = time.Millisecond
)

// openDeviceRetry opens a device node, waiting for it to be created if its
// driver is still probing. Only "does not exist" retries; real open errors
// surface immediately.
func openDeviceRetry(path string, budget time.Duration) (*os.File, error) {
	deadline := time.Now().Add(budget)

	for {
		device, err := os.Open(path) // #nosec G304 -- the path comes from the host's kernel cmdline, not an archive
		if err == nil {
			return device, nil
		}

		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			//nolint:wrapcheck // caller adds the device context
			return nil, err
		}

		time.Sleep(devicePollInterval)
	}
}

// overlayData builds the overlayfs mount options for a stack whose upper and
// work dirs live under dest.
func overlayData(dest, lower string) string {
	return "lowerdir=" + lower + ",upperdir=" + dest + overlayUpperDir + ",workdir=" + dest + overlayWorkDir
}

// makeOverlayDirs creates the lower/upper/work mountpoints under dest.
func makeOverlayDirs(dest string) error {
	for _, sub := range []string{overlayLowerDir, overlayUpperDir, overlayWorkDir} {
		if err := os.MkdirAll(dest+sub, stdDirMode); err != nil {
			return fmt.Errorf("create overlay dir %s: %w", dest+sub, err)
		}
	}

	return nil
}

// stackOverlay mounts the merged overlay AT dest, shadowing the tmpfs that
// carries its upper and work dirs by path while pinning their dentries.
//
// flags are the rootfs mount options from the boot plan, and passing them here
// is load-bearing rather than tidy: they carry nodev, and the container root
// is this overlay, not the tmpfs underneath it. Mount the overlay without them
// and every device node in the image becomes live again, which is precisely
// the confinement pkg/container's rootfsMountOpts is relying on.
func stackOverlay(dest, lower string, flags uintptr) error {
	if err := unix.Mount(fsTypeOverlay, dest, fsTypeOverlay, flags, overlayData(dest, lower)); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", dest, err)
	}

	return nil
}

// mountEROFSRootfs materializes the rootfs as mounts: the blob device is
// mounted read-only as the overlay lower, upper/work live on the
// already-mounted rootfs tmpfs at dest, and the merged overlay lands at dest —
// where the container root, and every host write into the rootfs, already
// point.
func mountEROFSRootfs(devicePath, dest string, flags uintptr) error {
	start := time.Now()

	if err := makeOverlayDirs(dest); err != nil {
		return err
	}

	// The lower is read-only twice over — the device is attached read-only and
	// EROFS has no writer — but MS_RDONLY is still stated, because flags also
	// carries nodev and dropping the rest of it here would be silent.
	lower := dest + overlayLowerDir
	if err := unix.Mount(devicePath, lower, "erofs", flags|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount erofs %s on %s: %w", devicePath, lower, err)
	}

	if err := stackOverlay(dest, lower, flags); err != nil {
		return err
	}

	log.Printf("copy-in: erofs mounts dur=%s", time.Since(start).Round(time.Microsecond))

	maxprocsOnce.Do(func() { goruntime.GOMAXPROCS(1) })

	return nil
}

// procCmdline is where the kernel exposes the boot cmdline the plan parser
// reads.
const procCmdline = "/proc/cmdline"

// Overlay stack layout under the rootfs tmpfs at dest, and the fs type name.
const (
	fsTypeOverlay   = "overlay"
	overlayLowerDir = "/lower"
	overlayUpperDir = "/upper"
	overlayWorkDir  = "/work"
)
