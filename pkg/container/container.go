//go:build darwin && arm64

// Package container orchestrates one Linux container in one throwaway
// microVM: image → tmpfs rootfs (vminitd Copy extract) → OCI spec →
// vminitd create/start/wait — the ossein core loop.
package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mycophonic/primordium/filesystem/dirs"
	blobcache "github.com/mycophonic/primordium/store/cache"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/farcloser/ossein/internal/protocol"
	"github.com/farcloser/ossein/pkg/guest"
	"github.com/farcloser/ossein/pkg/image"
	"github.com/farcloser/ossein/pkg/ocispec"
	"github.com/farcloser/ossein/pkg/vm"
)

// Fixed vsock port plan (host side listens; guest connects out).
const (
	portStdin  uint32 = 62000
	portStdout uint32 = 62001
	portStderr uint32 = 62002
	// portProxyBase is where guest-unix-socket proxies (ProxyVsock INTO) live.
	portProxyBase uint32 = 63000

	// rootfsBlobSerial identifies the flattened rootfs blob's virtio-blk
	// device. SERIAL, not a /dev/vdX name: with async device probing, vdX
	// assignment follows probe-completion order and is nondeterministic —
	// the guest resolves the serial via /sys/block/<name>/serial. Max 20
	// bytes (Virtualization.framework limit).
	rootfsBlobSerial = "osrootfs"

	// cacheDiskSerialFmt names persistent cache disks the same way
	// ("oscache0", "oscache1", …), resolved guest-side by the Mount RPC.
	cacheDiskSerialFmt = "oscache%d"

	// serialSourcePrefix marks a Mount RPC source as a virtio serial to
	// resolve rather than a device path. Keep in lockstep with the guest
	// agent's Mount.
	serialSourcePrefix = "virtio-serial:"

	// rootfsPlanParam is the kernel cmdline parameter carrying the rootfs
	// materialization plan ("<serial>:<dest>:<tmpfs-opts>"). vminitd parses
	// it and starts extracting at init; keep in lockstep with the guest
	// (guestagent.BeginRootfsMaterialization).
	rootfsPlanParam = "ossein.rootfs="

	dirPerm         = 0o755  // guest directory mode for mountpoints/staging
	instanceDirPerm = 0o750  // host per-instance state dir mode
	resolvPerm      = 0o644  // guest resolv.conf-style config file mode
	fsTypeExt4      = "ext4" // persistent cache disk filesystem
	fsTypeTmpfs     = "tmpfs"

	// defaultCPUs/defaultMemoryMiB apply when the spec leaves them zero.
	defaultCPUs      uint   = 2
	defaultMemoryMiB uint64 = 2048

	// guestIface is the guest's single NIC; the guest agent DHCPs on it.
	guestIface = "eth0"

	// rosettaTag names the Rosetta virtio-fs share AND the binfmt handler.
	rosettaTag = "rosetta"

	// consoleErrFmt decorates boot errors with the console log path — the
	// primary debugging artifact when a VM fails to come up.
	consoleErrFmt = "%w (console log: %s)"

	// logKeyInstance / logKeyErr are the shared slog keys.
	logKeyInstance = "instance"
	logKeyErr      = "err"

	// rootfsReserveMiB is RAM held back from the tmpfs rootfs for the kernel and
	// container processes. tmpfs pages ARE guest RAM and the guest has no swap,
	// so the rootfs is sized to (memory - reserve): at the 4 GiB run default this
	// yields a 3 GiB rootfs, and it scales 1:1 with --memory above the reserve.
	rootfsReserveMiB uint64 = 1024

	// closeFlushTimeout bounds Close's cache-disk flush (Sync + Umounts).
	// 60s mirrors `ossein stop`'s default --grace for the same shutdown.
	closeFlushTimeout = 60 * time.Second
)

// rootfsSizeFromMemory sizes the tmpfs rootfs from the VM's RAM (the kernel's
// default tmpfs is only 50% of RAM): everything above the kernel/agent
// reserve, but never less than half. The floor keeps tiny VMs viable AND
// makes the curve monotonic — the bare mem-reserve rule gave --memory 1100 a
// 76 MiB rootfs while --memory 1024 got 512 MiB: more memory, less rootfs.
func rootfsSizeFromMemory(mem uint64) uint64 {
	half := mem / 2
	if mem > rootfsReserveMiB && mem-rootfsReserveMiB > half {
		return mem - rootfsReserveMiB
	}

	return half
}

// Artifacts locates the two pinned guest artifacts.
type Artifacts struct {
	Kernel string
	Initfs string
}

// Mount is a host dir shared into the container (virtio-fs + bind).
type Mount struct {
	Host     string
	Dest     string
	ReadOnly bool
}

// DiskMount attaches a host ext4 image as a virtio-blk device and mounts it,
// read-write, inside the container rootfs at GuestPath (e.g. /var/lib/buildkit
// for a persistent buildkit cache). Unlike Mount (virtio-fs, ephemeral host
// share), a DiskMount is real block storage that survives the VM. Images attach
// after the initfs, so the first is /dev/vdb, the second /dev/vdc, and so on.
type DiskMount struct {
	ImagePath string // host ext4 image
	GuestPath string // absolute mountpoint within the container rootfs
}

// RunSpec describes one container run.
type RunSpec struct {
	// InstanceID forces the instance id (and thus the RuntimeDir()/<id> state
	// dir). Empty mints a fresh one. The buildkit detach launcher sets it so the
	// backgrounded child shares one instance dir with its parent.
	InstanceID string
	Image      string
	Platform   string   // "linux/amd64" | "linux/arm64"; empty = host (arm64)
	Pull       string   // image.Pull{Always,Missing,Never}; empty = missing
	Command    []string // override of image CMD (docker semantics)
	Env        []string // extra env (appended to image env)
	Cwd        string
	User       string // numeric uid[:gid]; empty = image config / root
	TTY        bool
	Privileged bool
	Network    bool
	CPUs       uint
	MemoryMiB  uint64
	Mounts     []Mount     // host dir → guest path (virtio-fs)
	Disks      []DiskMount // host ext4 image → guest mountpoint (virtio-blk, persistent)
	ConsoleLog string      // guest console log path; empty = temp file

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Instance is a running container/VM pair.
type Instance struct {
	ID string
	// Dir is the per-instance runtime directory (RuntimeDir()/<id>) that holds
	// everything for this instance in one place: console log, buildkit log, pid,
	// socket. Created by Boot.
	Dir   string
	agent *guest.Agent
	vm    *vm.VM
	// rootfs is the in-guest rootfs path.
	rootfs string
	// rootfsPin holds the flattened rootfs blob pinned in the content cache
	// (against GC) while the VM has it attached as a read-only disk.
	rootfsPin *blobcache.PinnedFile

	// mountedDisks are the guest mountpoints of persistent cache disks, flushed
	// and unmounted on Close so their writes reach the host image before the VM
	// stops.
	mountedDisks []string
	// outRelays tracks the stdout/stderr relay goroutines armed by
	// StartProcess, so Wait can drain the last buffered vsock bytes before
	// reporting exit — otherwise the caller races the relays and output is
	// truncated. Stdin is deliberately not tracked: its relay blocks reading
	// the host's stdin and only dies with the instance.
	outRelays sync.WaitGroup
	// spec and img are captured at Boot so StartProcess cannot be handed a
	// different spec than the one whose mounts/disks the VM was built from.
	spec RunSpec
	img  *image.Image
	// proxySeq allocates guest vsock ports for ExposeUnix so multiple proxies
	// on one instance never collide.
	proxySeq atomic.Uint32
}

// NewID mints a fresh instance id. Exported so a parent process (e.g. the
// buildkit detach launcher) can choose the id up front and thread it to the
// backgrounded child via RunSpec.InstanceID, so both agree on one InstanceDir.
func NewID() string {
	var b [4]byte

	_, _ = rand.Read(b[:])

	return "bc-" + hex.EncodeToString(b[:])
}

// InstanceDir returns (creating it) the per-instance runtime directory
// RuntimeDir()/<id>. Everything for one instance — console log, buildkit log,
// pid, socket — lives here, so an instance's state has exactly one location.
func InstanceDir(instanceID string) (string, error) {
	root, err := dirs.RuntimeDir()
	if err != nil {
		return "", fmt.Errorf("locating runtime dir: %w", err)
	}

	dir := filepath.Join(root, instanceID)
	if err := os.MkdirAll(dir, instanceDirPerm); err != nil {
		return "", fmt.Errorf("creating instance dir: %w", err)
	}

	return dir, nil
}

func guestRootfs(instanceID string) string { return "/run/container/" + instanceID + "/rootfs" }

// rootfsMountOpts is the tmpfs option set for a container rootfs.
//
// nodev is load-bearing: ossein applies no devices cgroup, and the rootfs tar
// can carry device nodes (guestagent's extractTar honors TypeBlock/TypeChar),
// so an image could otherwise ship a node for a disk attached to the VM and
// open it directly. nodev makes any such node inert.
//
// Deliberately NOT nosuid: images legitimately ship setuid binaries (su, ping)
// and docker does not strip them. The container's own /dev is a separate tmpfs
// (see pkg/ocispec) and must stay dev-capable for /dev/null and friends.
func rootfsMountOpts(sizeMiB uint64) []string {
	return []string{"mode=0755", "nodev", fmt.Sprintf("size=%dm", sizeMiB)}
}

// Boot pulls the image, boots the VM, materializes the rootfs, and configures
// guest networking — everything up to (not including) process creation.
func Boot(ctx context.Context, art Artifacts, cache image.Cache, spec RunSpec) (*Instance, *image.Image, error) {
	instanceID := spec.InstanceID
	if instanceID == "" {
		instanceID = NewID()
	}

	// One scoped logger for the whole boot: every stage line is correlated by
	// instance id (main sets the base default; this derives from it locally, so
	// no process-global mutation).
	logger := slog.Default().With(logKeyInstance, instanceID)

	// Everything for this instance lives under one directory.
	dir, err := InstanceDir(instanceID)
	if err != nil {
		return nil, nil, err
	}

	// The platform decides everything arch-related, Docker-style: an amd64
	// target on our arm64 host implies Rosetta (attach the share + register
	// the binfmt handler); arm64 runs native.
	canonPlatform, arch, err := image.CanonicalPlatform(spec.Platform)
	if err != nil {
		return nil, nil, err
	}

	rosetta := arch == "amd64"

	// Stage-timing convention, held by EVERY timing line in this package:
	// a line is emitted when a stage COMPLETES, its message names the stage
	// in the past tense, and it carries dur= (that stage's own cost) plus
	// at= (offset from boot start when it completed). Host-side, Boot is
	// strictly sequential — the rootfs blob must exist before the VM can be
	// created — so at[n] = at[n-1] + dur[n] holds line over line, and any
	// drift exposes hidden work. The GUEST, however, works ahead: vminitd
	// starts both the rootfs materialization (kernel-cmdline plan) and its
	// DHCP exchange at init, overlapping the agent handshake — so the
	// "rootfs extracted" and "guest network up" stages JOIN guest work
	// (their dur is residual wait, not the work's cost; the guest console
	// carries the real copy-in split). The opening "resolving image" line
	// is the single deliberate exception to completion-shape: it marks t=0
	// and announces the run parameters before the first potentially slow
	// stage.
	bootStart := time.Now()
	lastLap := bootStart
	lap := func() (slog.Attr, slog.Attr) {
		now := time.Now()
		dur := slog.Duration("dur", now.Sub(lastLap))
		lastLap = now

		return dur, slog.Duration("at", now.Sub(bootStart))
	}

	pull := spec.Pull
	if pull == "" {
		pull = image.PullMissing
	}

	logger.Info("resolving image", "image", spec.Image, "platform", canonPlatform, "rosetta", rosetta, "pull", pull)

	img, err := image.Resolve(ctx, cache, spec.Image, canonPlatform, pull)
	if err != nil {
		return nil, nil, err
	}

	stageDur, stageAt := lap()
	logger.Info("image resolved",
		slog.String("digest", img.Digest), slog.String("platform", canonPlatform), stageDur, stageAt)

	// The flattened rootfs blob must exist as a complete file BEFORE the VM
	// exists: it is attached as a read-only virtio-blk disk at VM creation
	// (Virtualization.framework has no hot-attach). A warm run pins the
	// cached blob in about a millisecond; a cold pull runs the whole
	// fetch+flatten here, before any VM sits waiting on it — which is why
	// this stage is timed apart from the resolve. The pin holds the blob
	// against cache GC until Close.
	rootfsBlob, err := img.RootfsFile()
	if err != nil {
		return nil, nil, err
	}

	stageDur, stageAt = lap()
	logger.Info("rootfs blob pinned", slog.Int64("sizeMiB", rootfsBlob.Size>>20), stageDur, stageAt)

	cpus := spec.CPUs
	if cpus == 0 {
		cpus = defaultCPUs
	}

	mem := spec.MemoryMiB
	if mem == 0 {
		mem = defaultMemoryMiB
	}

	consoleLog := spec.ConsoleLog
	if consoleLog == "" {
		consoleLog = filepath.Join(dir, "console.log")
	}

	rootfsSizeMiB := rootfsSizeFromMemory(mem)

	// The whole materialization plan rides the kernel cmdline: vminitd mounts
	// the tmpfs and extracts the blob AT INIT, overlapping the agent
	// handshake and network setup — the host never issues rootfs mkdir/mount
	// RPCs, and copyInRootfs merely awaits the result.
	rootfsPath := guestRootfs(instanceID)
	rootfsPlan := fmt.Sprintf("%s%s:%s:%s", rootfsPlanParam, rootfsBlobSerial, rootfsPath,
		strings.Join(rootfsMountOpts(rootfsSizeMiB), ","))

	// Networking is Virtualization.framework's own NAT now: no host-side stack
	// to start, own or tear down — the VM either has a NIC on Apple's subnet or
	// it has none.
	machine, err := vm.New(vmConfig(art, spec, cpus, mem, consoleLog, rosetta, rootfsBlob.Path, rootfsPlan))
	if err != nil {
		_ = rootfsBlob.Release()

		return nil, nil, err
	}

	stageDur, stageAt = lap()
	logger.Info("microVM created",
		slog.Uint64("cpus", uint64(cpus)), slog.Uint64("memoryMiB", mem),
		slog.String("console", consoleLog), stageDur, stageAt)

	if err := machine.Start(); err != nil {
		return nil, nil, fmt.Errorf(consoleErrFmt, err, consoleLog)
	}

	inst := &Instance{
		ID: instanceID, Dir: dir, vm: machine, rootfs: rootfsPath,
		spec: spec, img: img, rootfsPin: rootfsBlob,
	}
	// Teardown must run to completion regardless of ctx (it is often already
	// cancelled — that's why we're failing), so Close uses a fresh context.
	fail := func(err error) (*Instance, *image.Image, error) { //nolint:contextcheck // deliberate teardown context
		inst.Close(context.Background())

		return nil, nil, fmt.Errorf(consoleErrFmt, err, consoleLog)
	}

	// Control channel. ConnectRetry honors the dial ctx, so cancelling Boot
	// interrupts the retry loop; guest.Dial's handshake timeout is the overall
	// patience bound.
	logger.Debug("dialing guest agent", "vsockPort", protocol.VsockPort)

	agent, err := guest.Dial(ctx, func(dialCtx context.Context) (net.Conn, error) {
		return machine.ConnectRetry(dialCtx, protocol.VsockPort, 20*time.Second)
	})
	if err != nil {
		return fail(err)
	}

	inst.agent = agent

	stageDur, stageAt = lap()
	logger.Info("guest agent up", stageDur, stageAt)

	if err := inst.copyInRootfs(ctx); err != nil {
		return fail(err)
	}

	if err := inst.mountDisks(ctx, spec.Disks, logger); err != nil {
		return fail(err)
	}

	stageDur, stageAt = lap()
	logger.Info("rootfs extracted", stageDur, stageAt)

	if err := inst.configureLoopback(ctx); err != nil {
		return fail(err)
	}

	if spec.Network {
		netCfg, err := inst.configureGuestNetwork(ctx)
		if err != nil {
			return fail(err)
		}

		stageDur, stageAt = lap()
		logger.Info("guest network up", slog.String("cidr", netCfg.CIDR),
			slog.String("gateway", netCfg.Gateway), slog.Any("dns", netCfg.Nameservers),
			stageDur, stageAt)
	}

	if rosetta {
		if err := setupRosetta(ctx, agent); err != nil {
			return fail(err)
		}
	}

	stageDur, _ = lap()
	logger.Info("boot complete", stageDur, slog.Duration("total", time.Since(bootStart)))

	return inst, img, nil
}

// vmConfig assembles the vz configuration from the run spec and resolved
// defaults.
func vmConfig(
	art Artifacts, spec RunSpec, cpus uint, mem uint64, consoleLog string, rosetta bool,
	rootfsBlobPath, rootfsPlan string,
) vm.Config {
	shares := make([]vm.Share, 0, len(spec.Mounts))
	for idx, mount := range spec.Mounts {
		shares = append(shares, vm.Share{
			Tag: fmt.Sprintf("share%d", idx), Dir: mount.Host, ReadOnly: mount.ReadOnly,
		})
	}

	// Every disk carries a SERIAL: attach order stops implying guest names
	// the moment devices probe in parallel (see vm.Disk.Serial).
	disks := make([]vm.Disk, 0, len(spec.Disks)+1)
	disks = append(disks, vm.Disk{Path: rootfsBlobPath, ReadOnly: true, Serial: rootfsBlobSerial})

	for idx, diskMount := range spec.Disks {
		disks = append(disks, vm.Disk{
			Path: diskMount.ImagePath, ReadOnly: false,
			Serial: fmt.Sprintf(cacheDiskSerialFmt, idx),
		})
	}

	return vm.Config{
		Kernel:          art.Kernel,
		Initfs:          art.Initfs,
		CPUs:            cpus,
		MemoryMiB:       mem,
		ConsoleLog:      consoleLog,
		Network:         spec.Network,
		Shares:          shares,
		Disks:           disks,
		Rosetta:         rosetta,
		ExtraKernelArgs: []string{rootfsPlan},
	}
}

// setupRosetta mounts the Rosetta share and registers the binfmt handler
// (flags "F" pre-opens the binary so it resolves across namespaces).
func setupRosetta(ctx context.Context, agent *guest.Agent) error {
	if err := agent.Mkdir(ctx, "/run/rosetta", true, dirPerm); err != nil {
		return err
	}

	if err := agent.Mount(ctx, "virtiofs", rosettaTag, "/run/rosetta", nil); err != nil {
		return err
	}

	return agent.SetupEmulator(ctx,
		rosettaTag, "/run/rosetta/rosetta", "0",
		`\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00`,
		`\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff`,
		"F",
	)
}

// Doctor boots a bare microVM (no image, no network), completes a vminitd
// RPC round-trip, exercises a tmpfs mount, and tears down — the M1
// acceptance test, network-free.
func Doctor(ctx context.Context, art Artifacts, consoleLog string) error {
	if consoleLog == "" {
		dir, err := InstanceDir("doctor")
		if err != nil {
			return err
		}

		consoleLog = filepath.Join(dir, "console.log")
	}

	machine, err := vm.New(vm.Config{
		Kernel:     art.Kernel,
		Initfs:     art.Initfs,
		CPUs:       1,
		MemoryMiB:  512,
		ConsoleLog: consoleLog,
	})
	if err != nil {
		return err
	}

	if err := machine.Start(); err != nil {
		return fmt.Errorf(consoleErrFmt, err, consoleLog)
	}
	defer func() { _ = machine.Stop() }()

	agent, err := guest.Dial(ctx, func(dialCtx context.Context) (net.Conn, error) {
		return machine.ConnectRetry(dialCtx, protocol.VsockPort, 20*time.Second)
	})
	if err != nil {
		return fmt.Errorf("guest handshake: %w (console log: %s)", err, consoleLog)
	}
	defer func() { _ = agent.Close() }()

	if err := agent.Mkdir(ctx, "/run/doctor", true, dirPerm); err != nil {
		return err
	}

	if err := agent.Mount(ctx, fsTypeTmpfs, fsTypeTmpfs, "/run/doctor", []string{"mode=0755"}); err != nil {
		return err
	}

	return agent.WriteFile(ctx, "/run/doctor/ping", []byte("pong"), resolvPerm)
}

// StartProcess creates + starts the container init process using the spec and
// image captured at Boot, wiring stdio through host-side vsock relays.
func (i *Instance) StartProcess(ctx context.Context) error {
	startTime := time.Now()
	defer func() {
		slog.Default().Info("process started", slog.String(logKeyInstance, i.ID),
			slog.Duration("dur", time.Since(startTime)))
	}()

	specJSON, err := i.buildOCISpec(ctx)
	if err != nil {
		return err
	}

	// Host-side stdio listeners must exist before CreateProcess: the guest
	// connects out to them. Track them so every error path below closes what
	// was already opened — the ports are fixed, so a leaked listener would
	// poison any retry on a still-live instance.
	stdio := guest.StdioPorts{}

	var (
		relays    []func()
		listeners []net.Listener
	)

	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}

	if i.spec.Stdin != nil {
		listener, err := i.vm.Listen(portStdin)
		if err != nil {
			return err
		}

		listeners = append(listeners, listener)
		port := portStdin
		stdio.Stdin = &port

		// The stdin relay is detached from the RPC ctx by design: host stdin
		// may outlive any request, and the guest-side close gets its own
		// deadline inside the relay.
		relays = append(relays, i.stdinRelay(listener)) //nolint:contextcheck // deliberate: see comment
	}

	if i.spec.Stdout != nil {
		listener, err := i.vm.Listen(portStdout)
		if err != nil {
			closeAll()

			return err
		}

		listeners = append(listeners, listener)
		port := portStdout
		stdio.Stdout = &port

		relays = append(relays, i.outputRelay(listener, i.spec.Stdout))
	}

	if i.spec.Stderr != nil && !i.spec.TTY {
		listener, err := i.vm.Listen(portStderr)
		if err != nil {
			closeAll()

			return err
		}

		listeners = append(listeners, listener)
		port := portStderr
		stdio.Stderr = &port

		relays = append(relays, i.outputRelay(listener, i.spec.Stderr))
	}

	if err := i.agent.CreateProcess(ctx, i.ID, specJSON, stdio); err != nil {
		closeAll()

		return err
	}
	// Arm the relays: guest connects during/after start. From here the relays
	// own their listeners (each closes its own after Accept), and a failed
	// start tears the whole VM down, which unblocks any pending Accept. The
	// Add happens here — before any goroutine and before StartProcess can
	// return — so a CreateProcess failure leaves outRelays untouched and Wait
	// never blocks on relays that were never armed.
	i.outRelays.Add(len(relays))

	for _, relay := range relays {
		go relay()
	}

	if _, err := i.agent.StartProcess(ctx, i.ID); err != nil {
		return err
	}

	return nil
}

// Wait blocks until the init process exits, drains the stdout/stderr relays,
// and returns the exit code. The guest wires the workload's stdio straight to
// the vsock conns, so each relay ends exactly when the last guest-side holder
// of that stream exits — docker semantics: a background child keeping stdio
// open keeps Wait alive (the drain, not the exit-code RPC). Cancelling ctx
// abandons the drain (tail output may be lost) but still returns the code.
// Only valid after a successful StartProcess.
func (i *Instance) Wait(ctx context.Context) (int32, error) {
	code, err := i.agent.WaitProcess(ctx, i.ID)
	if err != nil {
		return code, err
	}

	drained := make(chan struct{})

	go func() { i.outRelays.Wait(); close(drained) }()

	select {
	case <-drained:
	case <-ctx.Done():
	}

	return code, nil
}

// Kill signals the init process.
func (i *Instance) Kill(ctx context.Context, sig int32) error {
	return i.agent.KillProcess(ctx, i.ID, sig)
}

// Resize resizes the container TTY.
func (i *Instance) Resize(ctx context.Context, rows, cols uint32) error {
	return i.agent.ResizeProcess(ctx, i.ID, rows, cols)
}

// ExposeUnix exposes a unix socket that lives inside the CONTAINER on a host
// unix socket, via a guest-agent vsock proxy: host listener → vsock port →
// guest unix socket. containerPath is the path as the container sees it; the
// guest resolves paths in its root namespace, so it is prefixed with the
// container rootfs. Each call allocates a fresh guest port and proxy id, so
// multiple exposures on one instance coexist. The returned closer tears down
// both ends (host listener + guest proxy).
func (i *Instance) ExposeUnix(ctx context.Context, containerPath, hostPath string) (func(), error) {
	seq := i.proxySeq.Add(1)
	port := portProxyBase + seq - 1
	proxyID := fmt.Sprintf("proxy-%s-%d", i.ID, seq)

	guestPath := i.rootfs + containerPath
	if err := i.agent.ProxyVsockOutOf(ctx, proxyID, port, guestPath); err != nil {
		return nil, err
	}

	// From here on the guest holds a live vsock listener for this proxy; every
	// error return must retire it, or it lingers until VM teardown — bounded
	// for the CLI, unbounded for a library caller that retries with a
	// different hostPath. Same shape as the closer below: its own deadline,
	// because the caller's ctx may already be dead on these paths.
	stopProxy := func() { //nolint:contextcheck // retirement runs on its own deadline (see above)
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = i.agent.StopVsockProxy(stopCtx, proxyID)
	}

	// Clear a stale socket file, but never hijack a live one: if something
	// still answers on it, the caller pointed two instances at one path. Only
	// two outcomes mean "free": no file (ENOENT) or a file nothing listens on
	// (ECONNREFUSED). Anything else — a cancelled ctx, EACCES, ENFILE — is NOT
	// evidence the socket is dead, and removing it on such an error would
	// unlink another live instance's endpoint out from under its clients.
	probe := net.Dialer{Timeout: time.Second}

	conn, err := probe.DialContext(ctx, "unix", hostPath)

	switch {
	case err == nil:
		_ = conn.Close()

		stopProxy()

		return nil, fmt.Errorf("%w: %s", ErrSocketInUse, hostPath)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, unix.ECONNREFUSED):
		// Free: nothing there, or a stale file left by a dead process.
	default:
		stopProxy()

		return nil, fmt.Errorf("probing host socket %s: %w", hostPath, err)
	}

	_ = os.Remove(hostPath)

	var listenCfg net.ListenConfig

	listener, err := listenCfg.Listen(ctx, "unix", hostPath)
	if err != nil {
		stopProxy()

		return nil, fmt.Errorf("host socket %s: %w", hostPath, err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed by the closer below
			}

			go func() {
				defer func() { _ = conn.Close() }()

				gconn, err := i.vm.Connect(port)
				if err != nil {
					return
				}

				defer func() { _ = gconn.Close() }()

				bidiPipe(conn, gconn)
			}()
		}
	}()

	// The closer outlives the ExposeUnix ctx by design (teardown must run
	// even after the caller's ctx died), hence its own short deadline.
	return func() {
		_ = listener.Close()
		_ = os.Remove(hostPath)

		stopProxy()
	}, nil
}

// Close tears everything down: VM first (kills all guest state), then the
// host-side network stack.
func (i *Instance) Close(ctx context.Context) {
	var flushDur, agentDur, vmDur time.Duration

	if i.agent != nil {
		// Persist cache disks before the VM dies: sync flushes the guest page
		// cache to the virtio-blk devices, then unmounting each detaches ext4
		// cleanly so vm.Stop's flush carries the writes through to the host image.
		// Without this, buildkit's cache writes are lost with the tmpfs VM. These
		// are the ONLY barrier protecting durable data, so failures are loud.
		if len(i.mountedDisks) > 0 {
			// Bounded: every caller passes context.Background(), and an
			// unbounded flush turns a wedged guest (D-state sync(2) on
			// virtio-blk) into a hang nothing but an external SIGKILL ends —
			// a library caller just hangs. Generous rather than snappy,
			// matching `ossein stop`'s default grace: this is the only
			// barrier protecting durable data, and a real flush of a
			// gigabyte-dirty cache is allowed to be slow.
			flushCtx, cancelFlush := context.WithTimeout(ctx, closeFlushTimeout)
			defer cancelFlush()

			flushStart := time.Now()

			if err := i.agent.Sync(flushCtx); err != nil {
				slog.Default().Warn("guest sync failed; cache disk writes may be lost",
					logKeyInstance, i.ID, logKeyErr, err)
			}

			for _, mnt := range i.mountedDisks {
				err := i.agent.Umount(flushCtx, mnt)
				if err != nil {
					// One retry: buildkitd may still be releasing the mount.
					err = i.agent.Umount(flushCtx, mnt)
				}

				if err != nil {
					slog.Default().Warn("cache disk unmount failed; image may be torn mid-write",
						logKeyInstance, i.ID, "mount", mnt, logKeyErr, err)
				}
			}

			flushDur = time.Since(flushStart)
		}

		agentStart := time.Now()
		_ = i.agent.Close()
		agentDur = time.Since(agentStart)
	}

	// Stopping through the framework costs ~50ms. It is only owed when
	// durable state was attached (cache disks, whose flush above must land
	// before the device detaches); a throwaway VM holds nothing that
	// outlives it and dies with this process anyway (VZ VMs are in-process),
	// so the polite stop is skipped and vm_stop=0 marks it in the log.
	if i.vm != nil && len(i.mountedDisks) > 0 {
		vmStart := time.Now()

		if err := i.vm.Stop(); err != nil {
			slog.Default().Warn("vm stop", logKeyInstance, i.ID, logKeyErr, err)
		}

		vmDur = time.Since(vmStart)
	}

	// The VM is gone; nothing reads the rootfs blob anymore — release the GC
	// pin on the cache entry.
	if i.rootfsPin != nil {
		if err := i.rootfsPin.Release(); err != nil {
			slog.Default().Warn("rootfs blob unpin", logKeyInstance, i.ID, logKeyErr, err)
		}

		i.rootfsPin = nil
	}

	slog.Default().Info("teardown complete", slog.String(logKeyInstance, i.ID),
		slog.Duration("dur", flushDur+agentDur+vmDur),
		slog.Duration("cache_flush", flushDur), slog.Duration("agent_close", agentDur),
		slog.Duration("vm_stop", vmDur))
}

// buildOCISpec assembles and marshals the container's OCI runtime spec from
// the boot-time spec and image config.
func (i *Instance) buildOCISpec(ctx context.Context) ([]byte, error) {
	extraMounts, err := i.mountSharesInGuest(ctx, i.spec.Mounts)
	if err != nil {
		return nil, err
	}

	args := ocispec.Command(i.img.Config.Entrypoint, i.img.Config.Cmd, i.spec.Command)
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: image %s has no entrypoint/cmd", ErrNoCommand, i.spec.Image)
	}

	cwd := i.spec.Cwd
	if cwd == "" {
		cwd = i.img.Config.WorkingDir
	}

	user := i.spec.User
	if user == "" {
		user = i.img.Config.User
	}

	oci, err := ocispec.Build(ocispec.Params{
		ContainerID: i.ID,
		RootfsPath:  i.rootfs,
		Args:        args,
		Env:         append(append([]string{}, i.img.Config.Env...), i.spec.Env...),
		Cwd:         cwd,
		User:        user,
		TTY:         i.spec.TTY,
		Privileged:  i.spec.Privileged,
		ExtraMounts: extraMounts,
	})
	if err != nil {
		return nil, err
	}

	return ocispec.Marshal(oci)
}

// stdinRelay copies host stdin into the guest and closes the guest's stdin
// when the host side EOFs. The outer func (tracked on outRelays) only accepts
// the guest connection; the copy runs detached because host stdin may never
// EOF. The CloseProcessStdin RPC gets its own short deadline so a dead
// control channel cannot pin the copy goroutine forever.
func (i *Instance) stdinRelay(listener net.Listener) func() {
	return func() {
		defer i.outRelays.Done()

		conn, err := listener.Accept()
		_ = listener.Close()

		if err != nil {
			return
		}

		go func() {
			defer func() { _ = conn.Close() }()

			_, _ = io.Copy(conn, i.spec.Stdin)

			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_ = i.agent.CloseProcessStdin(closeCtx, i.ID)
		}()
	}
}

// outputRelay copies one guest output stream to the host writer. The copy is
// synchronous within the tracked func — that is the drain point Wait joins:
// the guest closes the conn when the last stream holder exits.
func (i *Instance) outputRelay(listener net.Listener, sink io.Writer) func() {
	return func() {
		defer i.outRelays.Done()

		conn, err := listener.Accept()
		_ = listener.Close()

		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = io.Copy(sink, conn)
	}
}

// configureLoopback brings `lo` up. Always wanted, network or not — the caller
// branches on spec.Network for everything else.
func (i *Instance) configureLoopback(ctx context.Context) error {
	return i.agent.LinkUp(ctx, "lo")
}

// guestNetwork is the addressing applied to the guest, for logging.
type guestNetwork struct {
	CIDR        string
	Gateway     string
	Nameservers []string
}

// configureGuestNetwork applies the network ossein created for this VM.
//
// There is no DHCP anywhere in this path. The VM has its own vmnet network
// (see pkg/vm.Network), so the host picked the subnet and the guest address
// before the VM booted, and configuration is three netlink RPCs plus the two
// files that need the container rootfs — about a millisecond, against the
// 85-300ms a lease acquisition used to cost. It is also immune to what other
// VM products do to the machine: nothing here reads host-wide vmnet state or
// talks to macOS's bootpd.
//
// Nameservers are the HOST's resolvers, not the gateway. The framework does
// run a DNS proxy on the gateway, but it needs ~3s to start answering
// (measured), which would put a multi-second stall back on the boot path it
// was just removed from. The host's own resolvers answer in single-digit
// milliseconds through the NAT, and because they are whatever macOS is
// configured with, VPN and internal-DNS setups keep resolving. The known
// limitation is macOS SCOPED split-DNS (per-domain resolvers): those flatten
// to the primary list, the same trade every containers-on-Mac runtime makes.
func (i *Instance) configureGuestNetwork(ctx context.Context) (guestNetwork, error) {
	network := i.vm.Network()
	if network == nil {
		return guestNetwork{}, fmt.Errorf("%w: the VM has no network device", ErrNoNetwork)
	}

	cfg := guestNetwork{
		CIDR:        network.GuestCIDR(),
		Gateway:     network.Gateway.String(),
		Nameservers: hostNameservers(network.Gateway.String()),
	}

	if err := i.agent.LinkUp(ctx, guestIface); err != nil {
		return guestNetwork{}, err
	}

	if err := i.agent.AddrAdd(ctx, guestIface, cfg.CIDR); err != nil {
		return guestNetwork{}, err
	}

	if err := i.agent.RouteAddDefault(ctx, guestIface, cfg.Gateway); err != nil {
		return guestNetwork{}, err
	}

	if len(cfg.Nameservers) > 0 {
		if err := i.agent.ConfigureDNS(ctx, i.rootfs, cfg.Nameservers); err != nil {
			return guestNetwork{}, err
		}
	}

	err := i.agent.ConfigureHosts(ctx, i.rootfs, []guest.HostsEntry{
		{IP: "127.0.0.1", Hostnames: []string{"localhost"}},
		{IP: network.Guest.String(), Hostnames: []string{i.ID}},
	})
	if err != nil {
		return guestNetwork{}, err
	}

	return cfg, nil
}

// hostNameservers reads the host's resolvers from /etc/resolv.conf, which on
// macOS mirrors the primary resolver set (including a VPN's when one is
// primary), and keeps only those a GUEST can actually reach.
//
// The filter is the load-bearing part. A resolver on loopback or a
// link-local address means "on this machine" — which, copied verbatim into
// the guest, means the guest itself, and DNS dies silently. That is not
// exotic: dnsmasq, cloudflared, and several VPN clients all publish
// 127.0.0.1 as the host's resolver. gateway is returned when nothing usable
// survives: the framework runs a DNS proxy there which is correct but takes
// ~3s to answer after boot (measured), so it is the fallback rather than the
// default.
//
// Known limitation either way: macOS SCOPED resolvers (per-domain servers
// from scutil --dns) are not represented in resolv.conf, so internal domains
// that resolve on the host may not resolve in the guest.
func hostNameservers(gateway string) []string {
	raw, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		slog.Default().Warn("could not read host resolvers; falling back to the network's DNS proxy",
			logKeyErr, err)

		return []string{gateway}
	}

	var servers []string

	for line := range strings.Lines(string(raw)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}

		address, parseErr := netip.ParseAddr(fields[1])
		if parseErr != nil || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsUnspecified() {
			continue
		}

		servers = append(servers, address.String())
	}

	if len(servers) == 0 {
		slog.Default().Warn("no guest-reachable host resolver; falling back to the network's DNS proxy")

		return []string{gateway}
	}

	return servers
}

// mountDisks mounts each persistent cache disk (ext4) inside the container
// rootfs before the workload starts — e.g. buildkitd then finds a warm
// /var/lib/buildkit instead of empty tmpfs. Disks are addressed by SERIAL
// (resolved guest-side): probe order stopped implying names with async
// device probing.
func (i *Instance) mountDisks(ctx context.Context, disks []DiskMount, logger *slog.Logger) error {
	for idx, disk := range disks {
		dev := serialSourcePrefix + fmt.Sprintf(cacheDiskSerialFmt, idx)
		guestMount := i.rootfs + disk.GuestPath

		if err := i.agent.Mkdir(ctx, guestMount, true, dirPerm); err != nil {
			return fmt.Errorf("mkdir cache mountpoint %s: %w", disk.GuestPath, err)
		}

		if err := i.agent.Mount(ctx, fsTypeExt4, dev, guestMount, nil); err != nil {
			return fmt.Errorf("mount cache disk %s at %s: %w", dev, disk.GuestPath, err)
		}

		i.mountedDisks = append(i.mountedDisks, guestMount)

		logger.Info("mounted cache disk", "device", dev, "guest", disk.GuestPath)
	}

	return nil
}

// copyInRootfs AWAITS the rootfs materialization vminitd started at init
// from the kernel-cmdline plan (mount tmpfs, resolve the blob device by
// serial, extract). By the time the agent answers this RPC the extraction
// has been running since early guest boot — often it is already done — so
// the stage duration logged around this call measures residual wait, not the
// extraction itself; the guest's copy-in console line has the true cost.
func (i *Instance) copyInRootfs(ctx context.Context) error {
	return i.agent.CopyInArchive(ctx, i.rootfs, rootfsBlobSerial)
}

// mountSharesInGuest makes each virtio-fs share visible under /run/virtiofs
// and returns bind mounts for the OCI spec. Tags follow Boot's share naming.
//
// The virtio-fs *guest* driver accepts no cache= option (cache mode is negotiated
// host-side at FUSE_INIT, which Apple's Virtualization.framework controls, not us)
// and rejects dax (VZ exposes no DAX window). Confirmed empirically — every
// cache=/dax= mount returns EINVAL. As a result execve of a binary living on a
// -v share re-reads it from the host per spawn (~600us slower than a rootfs
// exec); this is a VZ virtio-fs limit, and it stays off the container hot path
// (rootfs is tmpfs, the buildkit cache is virtio-blk), so only exec-from-mount is
// affected. One knob IS ours: vminitd raises each share's bdi read_ahead_kb from
// the 128 default to 1024 right after mounting (measured +26% sequential-read
// bandwidth; see tuneVirtiofsReadahead in the guest agent).
func (i *Instance) mountSharesInGuest(ctx context.Context, mounts []Mount) ([]specs.Mount, error) {
	var out []specs.Mount

	for idx, mount := range mounts {
		staging := fmt.Sprintf("/run/virtiofs/%d", idx)
		if err := i.agent.Mkdir(ctx, staging, true, dirPerm); err != nil {
			return nil, err
		}

		if err := i.agent.Mount(ctx, "virtiofs", fmt.Sprintf("share%d", idx), staging, nil); err != nil {
			return nil, err
		}

		opts := []string{"rbind"}
		if mount.ReadOnly {
			opts = append(opts, "ro")
		}

		out = append(out, specs.Mount{
			Type: "bind", Source: staging, Destination: mount.Dest, Options: opts,
		})
	}

	return out, nil
}

// closeWriter is the shutdown(SHUT_WR) half of a duplex conn. Both conns
// bidiPipe relays between implement it: the host's *net.UnixConn always did,
// and vz's VirtioSocketConnection does since ossein's fork added CloseWrite
// (pkg/vm asserts it at compile time). The fallback branch below therefore has
// no production caller today — it is kept because the interface, not the
// concrete type, is what this function is written against.
type closeWriter interface {
	CloseWrite() error
}

// bidiPipe relays both directions and ends only when BOTH are done.
//
// Half-close propagates in BOTH directions: when either side stops writing,
// the other sees a clean EOF while its own write side stays open. That is what
// makes EOF-framed protocols (HTTP/1.0-style, `nc -N`) work through
// ExposeUnix, alongside the length- and message-framed ones that never needed
// it — gRPC/h2, the in-repo consumer, never half-closes mid-stream.
//
// It was not always so, and the shape of the fallback is the record of why.
// Upstream vz kept the framework's fd in an unexported net.Conn and exposed
// neither CloseWrite nor SyscallConn, so the host→guest direction could only
// signal "the client is done" by closing the whole connection — cutting off a
// guest that was still replying. Tearing it down was nonetheless the right
// call over doing nothing: without a signal the guest never learns the client
// is gone, the opposite pump blocks on a read that never completes, this
// function never returns, and the conn pair its caller closes on return leaks
// — one pair per client, for the life of the instance. That is still exactly
// what happens for any conn lacking CloseWrite, which is why the branch stays.
func bidiPipe(left, right net.Conn) {
	var relay sync.WaitGroup

	pump := func(dst, src net.Conn) {
		defer relay.Done()

		_, _ = io.Copy(dst, src)

		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}

	relay.Add(2)

	go pump(left, right)
	go pump(right, left)

	relay.Wait()
}
