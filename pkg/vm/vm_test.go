//go:build darwin && arm64

package vm //nolint:testpackage // cmdline is unexported by design; the boot-contract test must live in-package.

import "testing"

// The boot contract documented on cmdline(): console on hvc0, immediate
// reboot on panic, mitigations off, vminitd as PID 1 out of the initramfs
// (no root device), parallel device probing. A regression here bricks every
// boot.
const bootContract = "console=hvc0 panic=-1 mitigations=off rdinit=/sbin/vminitd driver_async_probe=virtio-pci"

func TestCmdline(t *testing.T) {
	t.Setenv("OSSEIN_CMDLINE", "")

	if got := cmdline(nil); got != bootContract {
		t.Fatalf("cmdline(nil) = %q, want %q", got, bootContract)
	}
}

func TestCmdlineExtraArgs(t *testing.T) {
	// Per-VM args (the ossein.rootfs= materialization plan) come right after
	// the contract args.
	t.Setenv("OSSEIN_CMDLINE", "")

	extra := []string{"ossein.rootfs=/dev/vdb:/run/container/x/rootfs:mode=0755,nodev,size=3072m"}
	want := bootContract + " " + extra[0]

	if got := cmdline(extra); got != want {
		t.Fatalf("cmdline(extra) = %q, want %q", got, want)
	}
}

func TestCmdlineOsseinCmdlineExtension(t *testing.T) {
	// Whitespace-split (any run of spaces/tabs), appended AFTER the contract
	// and per-VM args so later kernel parameters win.
	t.Setenv("OSSEIN_CMDLINE", "  idle_poll_ns=0\tloglevel=7 ")

	want := bootContract + " ossein.rootfs=osrootfs:/run/rootfs:mode=0755 idle_poll_ns=0 loglevel=7"

	if got := cmdline([]string{"ossein.rootfs=osrootfs:/run/rootfs:mode=0755"}); got != want {
		t.Fatalf("cmdline() = %q, want %q", got, want)
	}
}
