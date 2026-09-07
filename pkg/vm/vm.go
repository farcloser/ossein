//go:build darwin && arm64

// Package vm leverages Virtualization.framework microVM via Code-Hex/vz:
// the ossein guest is always kernel + initfs (vminitd as PID 1,
// per the vendored apple/containerization contract in proto/).
//
// Boot contract (derived from containerization's Kernel+Commandline.swift, but NOT a copy of
// it — see the per-parameter notes on cmdline() for what we drop, add, and why):
//
//	console=hvc0 panic=-1 mitigations=off init=/sbin/vminitd ro rootfstype=ext4 root=/dev/vda
//
// with initfs.ext4 attached read-only as the FIRST virtio-blk device (/dev/vda).
package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/farcloser/ossein/internal/protocol"
	"github.com/farcloser/ossein/third_party/vz"
)

// Disk is an extra virtio-blk attachment (after the initfs at /dev/vda).
type Disk struct {
	Path     string
	ReadOnly bool
	// Serial is the virtio block-device identifier the guest reads back at
	// /sys/block/<name>/serial. It is the ONLY stable way to refer to a disk:
	// vdX names follow probe-completion order, which async device probing
	// makes nondeterministic. Max 20 bytes (Virtualization.framework limit).
	Serial string
}

// Share is a virtio-fs directory share, mounted in-guest by tag.
type Share struct {
	Tag      string
	Dir      string
	ReadOnly bool
}

// Config describes one microVM.
type Config struct {
	Kernel    string // uncompressed arm64 Image (kernel-arm64)
	Initfs    string // initfs.cpio containing vminitd (unpacked in RAM as the initramfs root)
	CPUs      uint
	MemoryMiB uint64

	ConsoleLog string // file receiving the guest console (hvc0); empty = discard
	// Network attaches a virtio-net device to Virtualization.framework's own NAT
	// (vmnet shared mode): the guest lands on a dedicated host-local subnet with
	// outbound access, and gets its address by DHCP from Apple's server. False
	// leaves the VM with no NIC at all.
	Network bool
	Disks   []Disk
	Shares  []Share
	Rosetta bool

	// ExtraKernelArgs are per-VM kernel cmdline additions (e.g. the
	// ossein.rootfs= materialization plan the guest acts on at init).
	ExtraKernelArgs []string
}

// VM wraps a running/startable vz VirtualMachine.
type VM struct {
	vm *vz.VirtualMachine
	// network is the per-VM vmnet network created for this machine, nil when
	// the spec asked for no networking. Its addressing is what the host
	// applies to the guest statically (see Network).
	network *Network
}

// Network returns the per-VM network, or nil when the VM has no NIC.
func (m *VM) Network() *Network { return m.network }

func cmdline(extra []string) string {
	args := []string{
		// hvc0 is virtio-console: nothing reaches ConsoleLog until the virtio device probes,
		// and the printk backlog replayed beforehand is dropped (upstream registers the hvc
		// console before the port is on pdrvdata.consoles, so put_chars() returns -EPIPE and
		// hvc_console_print() discards it). So the log starts ~"crng init done" and an early
		// panic leaves it EMPTY — use `dmesg` IN THE GUEST for the real boot log; the printk
		// buffer is intact, only the console path is broken.
		// Two fixes were tried and BOTH ARE DEAD — don't retry (see SCHED-PIPE-INVESTIGATION.md):
		//   `earlycon`      — rejected, "Malformed early option": VZ exposes no UART and its DT
		//                     names no stdout-path, so there is no early console to attach to.
		//   patching virtio_console to publish the port before hvc_alloc() — captures the full
		//                     log but HANGS BOOT 20-45% of the time: the replay then reaches
		//                     __send_to_port(), which spins waiting for a host that isn't
		//                     servicing the queue yet. Losing the log is upstream's deliberate
		//                     price for not hanging.
		"console=hvc0",
		// NOT set: `tsc=reliable`. It is x86-only (__setup("tsc=") exists solely in
		// arch/x86/kernel/tsc.c) and arm64's arch timer needs no such assertion. It was not
		// merely inert — the kernel rejected it out loud and leaked it into PID 1's environment:
		//   Unknown kernel command line parameters "tsc=reliable", will be passed to user space.
		// Cargo from x86 microVM lore; removed 2026-07-15.
		//
		// A guest panic otherwise hangs the VM forever (PANIC_TIMEOUT=0 => /proc/sys/kernel/panic
		// = 0), holding its RAM and host process until something external notices. For a
		// single-purpose throwaway VM an immediate reboot is strictly better: VZ surfaces it and
		// the runtime tears down.
		"panic=-1",
		// Single-tenant, throwaway microVM: CPU side-channel mitigations (KPTI,
		// Spectre barriers) only defend against cross-tenant/persistent attackers
		// we don't have, and they tax every syscall/context-switch. Turn them off
		// for a syscall-path speedup (biggest on container/buildkit workloads).
		"mitigations=off",
		// NOT set: `nohlt` (force-poll idle; CONFIG_GENERIC_IDLE_POLL_SETUP=y, so it works —
		// `idle=poll` is x86-only and silently ignored here). It pegs every vCPU at 100% when
		// idle. Superseded by kernel/patches/0002 (polling idle), which gets a better number
		// (sched-pipe 6.7µs → 0.56µs, ahead of OrbStack's 0.89) at 0% idle-VM host cost,
		// because its poll window is bounded and then parks in WFI. Tune with `idle_poll_ns=<n>`
		// (default 50us; 0 restores upstream behaviour). See SCHED-PIPE-INVESTIGATION.md.
		// The initfs packer writes the guest binary to exactly this path
		// (internal/protocol.InitPath), so the two cannot drift apart.
		// rdinit (not init): the root is the initramfs itself — no root=,
		// no rootfstype, no ro; there is no root block device at all.
		"rdinit=" + protocol.InitPath,
		// Probe virtio-pci devices in parallel: serially the 6 devices cost
		// ~8ms EACH, dominating the ~63ms kernel boot; async they enable
		// within ~1ms and the kernel hands off to init at ~17ms. The name
		// must be the registered driver name "virtio-pci" — the underscore
		// spelling silently matches nothing. This is only safe because
		// nothing references a device by probe-order name anymore: the root
		// is the initramfs (no root device), the rootfs blob and cache
		// disks are resolved by virtio SERIAL in the guest, and vminitd
		// tolerates late devices everywhere it touches one (console
		// reattach, vsock listen retry, blob resolve retry, DHCP link
		// wait). Measured 2026-07-31: with a block root this exact knob
		// panic-looped 1-4 boots per run on the vdX name lottery — do not
		// reintroduce a fixed-name device reference.
		"driver_async_probe=virtio-pci",
	}
	args = append(args, extra...)

	// OSSEIN_CMDLINE: extra kernel args, whitespace-split. Debug/bench knob (e.g.
	// idle_poll_ns=0 to A/B the polling-idle patch) — same env style as OSSEIN_KERNEL.
	if ext := os.Getenv("OSSEIN_CMDLINE"); ext != "" {
		args = append(args, strings.Fields(ext)...)
	}

	return strings.Join(args, " ")
}

// New assembles the VM configuration. It does not start it.
func New(cfg Config) (*VM, error) {
	boot, err := vz.NewLinuxBootLoader(cfg.Kernel,
		vz.WithCommandLine(cmdline(cfg.ExtraKernelArgs)),
		// The initfs boots as an initramfs (RAM root, no block device): the
		// kernel unpacks the cpio and runs rdinit= from it. This is what
		// removes the root-device identity race that made async device
		// probing unshippable — there is no root device to race.
		vz.WithInitrd(cfg.Initfs),
	)
	if err != nil {
		return nil, fmt.Errorf("bootloader: %w", err)
	}

	vmc, err := vz.NewVirtualMachineConfiguration(boot, cfg.CPUs, cfg.MemoryMiB*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}

	if err := configureStorage(vmc, cfg); err != nil {
		return nil, err
	}

	if err := configureIO(vmc, cfg); err != nil {
		return nil, err
	}

	network, err := configureNetwork(vmc, cfg)
	if err != nil {
		return nil, err
	}

	if err := configureShares(vmc, cfg); err != nil {
		return nil, err
	}

	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("configuration invalid: %w", err)
	}

	machine, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		return nil, fmt.Errorf("new vm: %w", err)
	}

	return &VM{vm: machine, network: network}, nil
}

// configureStorage attaches the disks. Attach ORDER no longer implies guest
// names: with async device probing vdX assignment is nondeterministic, so
// every disk carries a Serial and the guest resolves it via sysfs.
func configureStorage(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	var storage []vz.StorageDeviceConfiguration

	for _, disk := range cfg.Disks {
		// Fsync mode honors guest flush requests without forcing full
		// host-cache write-through on every write — the standard choice for
		// cache disks: durable when the guest asks, fast otherwise.
		attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			disk.Path,
			disk.ReadOnly,
			vz.DiskImageCachingModeAutomatic,
			vz.DiskImageSynchronizationModeFsync,
		)
		if err != nil {
			return fmt.Errorf("disk %s: %w", disk.Path, err)
		}

		blockDev, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return fmt.Errorf("blk %s: %w", disk.Path, err)
		}

		if disk.Serial != "" {
			if err := blockDev.SetBlockDeviceIdentifier(disk.Serial); err != nil {
				return fmt.Errorf("blk %s serial %q: %w", disk.Path, disk.Serial, err)
			}
		}

		storage = append(storage, blockDev)
	}

	vmc.SetStorageDevicesVirtualMachineConfiguration(storage)

	return nil
}

// configureIO wires the vsock control plane, the serial console log, and the
// always-on entropy + balloon devices.
func configureIO(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	// vsock: the control plane (vminitd RPC), stdio streams, copy-in, proxies.
	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}

	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	// Console (hvc0) to a log file — the only window into early boot failures.
	consolePath := cfg.ConsoleLog
	if consolePath == "" {
		consolePath = os.DevNull
	}

	serialAtt, err := vz.NewFileSerialPortAttachment(consolePath, false)
	if err != nil {
		return fmt.Errorf("console attachment: %w", err)
	}

	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAtt)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}

	vmc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	// Entropy + balloon: cheap, always on.
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("entropy: %w", err)
	}

	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("balloon: %w", err)
	}

	vmc.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	return nil
}

// configureNetwork wires the single virtio-net device to Virtualization.frame-
// work's own NAT. Apple runs the gateway, the DHCP server and the NAT itself on
// a dedicated host-local subnet (vmnet shared mode, a bridgeN interface on the
// host) — so ossein carries no userspace TCP/IP stack and no packet plumbing:
// the guest DHCPs at boot and talks to the world directly.
//
// NAT specifically needs no entitlement beyond com.apple.security.virtualization
// (verified: a VM configured this way starts under exactly ossein's entitlement
// file). BRIDGED mode is the one requiring com.apple.vm.networking, and ossein
// deliberately does not use it: bridged would put every container on the user's
// real LAN.
//
// Each VM gets a locally-administered random MAC so Apple's DHCP server hands
// out distinct leases to concurrent instances.
func configureNetwork(vmc *vz.VirtualMachineConfiguration, cfg Config) (*Network, error) {
	if !cfg.Network {
		return nil, nil //nolint:nilnil // "no NIC" is a valid, non-error outcome
	}

	network, err := newNetwork()
	if err != nil {
		return nil, err
	}

	att, err := vz.NewVmnetNetworkDeviceAttachment(network.network)
	if err != nil {
		return nil, fmt.Errorf("%w: vmnet attachment: %w", ErrNetwork, err)
	}

	netdev, err := vz.NewVirtioNetworkDeviceConfiguration(att)
	if err != nil {
		return nil, fmt.Errorf("net device: %w", err)
	}

	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("mac: %w", err)
	}

	netdev.SetMACAddress(mac)
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netdev})

	return network, nil
}

// configureShares attaches the virtio-fs shares (+ Rosetta, a special
// directory share).
func configureShares(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	var fsDevices []vz.DirectorySharingDeviceConfiguration

	for _, share := range cfg.Shares {
		fsDev, err := vz.NewVirtioFileSystemDeviceConfiguration(share.Tag)
		if err != nil {
			return fmt.Errorf("virtiofs %s: %w", share.Tag, err)
		}

		dir, err := vz.NewSharedDirectory(share.Dir, share.ReadOnly)
		if err != nil {
			return fmt.Errorf("shared dir %s: %w", share.Dir, err)
		}

		single, err := vz.NewSingleDirectoryShare(dir)
		if err != nil {
			return fmt.Errorf("share %s: %w", share.Dir, err)
		}

		fsDev.SetDirectoryShare(single)
		fsDevices = append(fsDevices, fsDev)
	}

	if cfg.Rosetta {
		dev, err := rosettaShare()
		if err != nil {
			return err
		}

		fsDevices = append(fsDevices, dev)
	}

	if len(fsDevices) > 0 {
		vmc.SetDirectorySharingDevicesVirtualMachineConfiguration(fsDevices)
	}

	return nil
}

// rosettaShare ensures Rosetta is available (auto-installing if needed) and
// builds its directory-share device.
func rosettaShare() (vz.DirectorySharingDeviceConfiguration, error) {
	switch availability := vz.LinuxRosettaDirectoryShareAvailability(); availability {
	case vz.LinuxRosettaAvailabilityNotSupported:
		return nil, fmt.Errorf("%w: not supported on this host", ErrRosetta)
	case vz.LinuxRosettaAvailabilityNotInstalled:
		if err := vz.LinuxRosettaDirectoryShareInstallRosetta(); err != nil {
			return nil, fmt.Errorf("%w: auto-install failed: %w", ErrRosetta, err)
		}
	case vz.LinuxRosettaAvailabilityInstalled:
		// Ready to use — nothing to do.
	default:
		return nil, fmt.Errorf("%w: unknown availability state %v", ErrRosetta, availability)
	}

	dev, err := vz.NewVirtioFileSystemDeviceConfiguration("rosetta")
	if err != nil {
		return nil, fmt.Errorf("rosetta device: %w", err)
	}

	rosetta, err := vz.NewLinuxRosettaDirectoryShare()
	if err != nil {
		return nil, fmt.Errorf("rosetta share: %w", err)
	}

	dev.SetDirectoryShare(rosetta)

	return dev, nil
}

// Start boots the VM and returns once it reports running. No wait loop is
// needed: vz v3.7.1's Start() blocks on the framework's completion handler
// (virtualization.go: `return <-errCh`), which fires with the machine already
// Running — a single post-start assertion covers the impossible case.
func (m *VM) Start() error {
	if err := m.vm.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if state := m.vm.State(); state != vz.VirtualMachineStateRunning {
		return fmt.Errorf("%w: not running after start (state=%v)", ErrLifecycle, state)
	}

	return nil
}

// Stop tears the VM down with a direct hard stop. The guest is ephemeral (tmpfs
// rootfs) and any durable state — cache disks — is synced and unmounted by
// Instance.Close BEFORE this runs, so there is nothing to shut down gracefully.
// vminitd installs no ACPI power handler, so a graceful RequestStop is never
// acknowledged and would just block until its ~10s timeout on every teardown
// before we hard-stop anyway.
//
// CanStop can lag by a beat around state transitions (e.g. still Starting), so
// poll briefly rather than silently declaring a live VM dead; if the machine
// never becomes stoppable, say so — the caller must not proceed believing the
// VM is gone.
func (m *VM) Stop() error {
	const (
		stopWindow = 2 * time.Second
		stopPoll   = 50 * time.Millisecond
	)

	deadline := time.Now().Add(stopWindow)

	for {
		if m.vm.State() == vz.VirtualMachineStateStopped {
			return nil
		}

		if m.vm.CanStop() {
			if err := m.vm.Stop(); err != nil {
				return fmt.Errorf("stopping vm: %w", err)
			}

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w: not stoppable within %s (state=%v)", ErrLifecycle, stopWindow, m.vm.State())
		}

		time.Sleep(stopPoll)
	}
}

// halfCloser is the shutdown(SHUT_WR) half of a duplex conn.
//
// The assertion below is the contract Connect and Listen advertise: the conns
// they hand out can be shut down one direction at a time, so a relay between a
// vsock hop and anything else can propagate a half-close instead of tearing
// the whole connection down (pkg/container's bidiPipe is the consumer). vz
// gained CloseWrite in ossein's fork; if a refresh of that fork ever drops it,
// this fails to compile here rather than silently degrading every proxied
// connection into "closes when the client stops writing".
type halfCloser interface {
	net.Conn
	CloseWrite() error
}

var _ halfCloser = (*vz.VirtioSocketConnection)(nil)

// Connect dials a guest vsock port. The guest side must be listening. The
// returned conn supports CloseWrite (see halfCloser).
func (m *VM) Connect(port uint32) (net.Conn, error) {
	dev, err := m.socketDevice()
	if err != nil {
		return nil, err
	}

	conn, err := dev.Connect(port)
	if err != nil {
		return nil, fmt.Errorf("%w: connect port %d: %w", ErrVsock, port, err)
	}

	return conn, nil
}

// ConnectRetry dials a guest vsock port, retrying until timeout — used for
// the vminitd control channel right after boot. The guest is usually up
// within a few ms, so retries start at 2ms and back off mildly (×1.5, capped
// at 50ms) instead of a fixed coarse poll that would tax every boot.
func (m *VM) ConnectRetry(ctx context.Context, port uint32, timeout time.Duration) (net.Conn, error) {
	// A failed vsock connect to a not-yet-listening guest costs microseconds,
	// while every unit of backoff is added BOOT LATENCY once the guest turns
	// ready inside the gap: with the old 50ms cap, readiness at ~150ms was
	// detected up to 50ms late (and run-to-run boot times wobbled by exactly
	// that). Cap the delay low: a handful of extra no-op dials in exchange
	// for bounded, small detection latency.
	const (
		initialDelay = 2 * time.Millisecond
		maxDelay     = 8 * time.Millisecond
	)

	deadline := time.Now().Add(timeout)
	delay := initialDelay

	var last error

	for time.Now().Before(deadline) {
		conn, err := m.Connect(port)
		if err == nil {
			return conn, nil
		}

		last = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vsock port %d: %w", port, ctx.Err())
		case <-time.After(delay):
		}

		delay = min(delay*3/2, maxDelay)
	}

	return nil, fmt.Errorf("vsock port %d not answering after %s: %w", port, timeout, last)
}

// Listen opens a host-side listener on a guest vsock port (guest connects out).
func (m *VM) Listen(port uint32) (net.Listener, error) {
	dev, err := m.socketDevice()
	if err != nil {
		return nil, err
	}

	listener, err := dev.Listen(port)
	if err != nil {
		return nil, fmt.Errorf("%w: listen port %d: %w", ErrVsock, port, err)
	}

	return listener, nil
}

func (m *VM) socketDevice() (*vz.VirtioSocketDevice, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, fmt.Errorf("%w: no device on VM", ErrVsock)
	}

	return devs[0], nil
}
