// Command build-initfs assembles the guest initfs as an UNCOMPRESSED newc cpio
// archive — the kernel unpacks it as the initramfs root, in RAM, with no block
// device involved. That deviceless root is load-bearing: virtio-blk names
// (vdX) follow probe-completion order, so a block-rooted kernel under async
// device probing raced its own root name and panic-looped; an initramfs root
// removes the race class entirely (and one virtio device with it).
//
// The archive holds exactly one payload — the guest binary at
// protocol.InitPath (the kernel boots it via rdinit=) — plus the mountpoint
// directories PID 1 mounts over and the /dev/console node the kernel needs to
// give init its stdio (the kernel does NOT automount devtmpfs for an
// initramfs root, and a console that races async probing is reattached later
// by vminitd's ensureConsole). The container's own userland comes from its
// image at runtime, never from here.
//
// Uncompressed on purpose: the cpio is //go:embed'ed into ossein and handed
// to Virtualization.framework verbatim, so kernel decompression support
// (CONFIG_RD_*) would buy image size at boot-time CPU cost — and the binary
// inside is the only content, which barely compresses anyway. The newc
// writer is ~80 lines of stdlib; no dependency earns its keep here.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/farcloser/ossein/internal/protocol"
)

const (
	// initBinaryMode makes the init binary executable inside the archive.
	initBinaryMode = 0o755

	// Mode bits stat(2)-style: S_IFDIR/S_IFREG/S_IFCHR composed with perms.
	modeDir  = 0o040000
	modeReg  = 0o100000
	modeChar = 0o020000

	// Device nodes the archive must carry. /dev/console (char 5,1) is where
	// the kernel points init's stdio; /dev/null (char 1,3) is what the Go
	// RUNTIME opens onto fds 0-2 when they arrive closed — and if that open
	// fails it is a fatal error, so a missing /dev/null kills PID 1 before
	// main() with exit status 2 and no message (measured: exactly what
	// async device probing produced, because the console is not registered
	// yet at exec time). vminitd mounts devtmpfs over /dev right after, so
	// these two nodes only have to survive the first instants.
	consoleMajor = 5
	consoleMinor = 1
	nullMajor    = 1
	nullMinor    = 3

	// newcHeaderLen is the fixed byte length of a newc record header.
	newcHeaderLen = 110

	// outMode is the archive file's mode: a build artifact, world-readable.
	outMode = 0o644
)

func main() {
	inPath := flag.String("in", "", "path to the compiled guest binary to embed")
	out := flag.String("out", "", "output initfs.cpio path")
	// The default is the shared constant the host's kernel cmdline is built from
	// (rdinit=<protocol.InitPath>), so the packed path and the boot path are one
	// declaration. Overridable only for experiments.
	initPath := flag.String("init-path", protocol.InitPath,
		"path of the init binary inside the archive (must match the kernel rdinit= cmdline)")

	flag.Parse()

	if *inPath == "" || *out == "" {
		log.Fatal("build-initfs: -in and -out are required")
	}

	if err := build(*inPath, *out, *initPath); err != nil {
		log.Fatalf("build-initfs: %v", err)
	}

	log.Printf("build-initfs: wrote %s with init at %s", *out, *initPath)
}

func build(inPath, out, initPath string) error {
	// Reading the caller-specified guest binary is this build tool's purpose.
	binary, err := os.ReadFile(inPath) // #nosec G304 -- see above
	if err != nil {
		return fmt.Errorf("reading guest binary: %w", err)
	}

	archive := newCpio()

	// Mountpoint directories PID 1 mounts over, parents-first.
	for _, dir := range []string{
		"proc", "sys", "sys/fs", "sys/fs/cgroup", "dev", "run", "tmp", "etc", "sbin",
	} {
		archive.dir(dir)
	}

	// The kernel opens /dev/console from the archive for init's stdio, and
	// falls back to /dev/null (see the const block) when the console has not
	// registered yet. Both nodes are mandatory.
	archive.charDev("dev/console", consoleMajor, consoleMinor)
	archive.charDev("dev/null", nullMajor, nullMinor)

	archive.file(trimSlash(initPath), initBinaryMode, binary)
	archive.trailer()

	if err := os.WriteFile(out, archive.buf.Bytes(), outMode); err != nil {
		return fmt.Errorf("writing archive: %w", err)
	}

	return nil
}

func trimSlash(path string) string {
	if len(path) > 0 && path[0] == '/' {
		return path[1:]
	}

	return path
}

// cpio writes an SVR4 "newc" (magic 070701) archive: a 110-byte ASCII-hex
// header, the NUL-terminated name padded to 4 bytes, then data padded to 4
// bytes, for every entry, ending with the TRAILER!!! record. This is the
// format the kernel's initramfs unpacker documents in
// Documentation/driver-api/early-userspace/buffer-format.rst.
type cpio struct {
	buf bytes.Buffer
	ino uint32
}

func newCpio() *cpio { return &cpio{ino: 1} }

func (c *cpio) dir(name string) { c.entry(name, modeDir|0o755, 0, 0, nil) }

func (c *cpio) file(name string, perm uint32, data []byte) {
	c.entry(name, modeReg|perm, 0, 0, data)
}

func (c *cpio) charDev(name string, major, minor uint32) {
	c.entry(name, modeChar|0o600, major, minor, nil)
}

func (c *cpio) trailer() {
	// The trailer's fields are conventionally zero (unpackers ignore them);
	// only the magic name matters.
	c.entry("TRAILER!!!", 0, 0, 0, nil)
}

func (c *cpio) entry(name string, mode, rdevMajor, rdevMinor uint32, data []byte) {
	ino := c.ino
	c.ino++

	fmt.Fprintf(&c.buf,
		"070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino,         // ino
		mode,        // mode
		0,           // uid (root)
		0,           // gid (root)
		1,           // nlink
		0,           // mtime (reproducible: epoch)
		len(data),   // filesize
		0,           // devmajor
		0,           // devminor
		rdevMajor,   // rdevmajor
		rdevMinor,   // rdevminor
		len(name)+1, // namesize incl. NUL
		0,           // check (always zero for newc)
	)
	c.buf.WriteString(name)
	c.buf.WriteByte(0)
	c.pad4(newcHeaderLen + len(name) + 1)

	if len(data) > 0 {
		c.buf.Write(data)
		c.pad4(len(data))
	}
}

// pad4 aligns the archive to a 4-byte boundary given the length of the
// just-written span.
func (c *cpio) pad4(span int) {
	for range (4 - span%4) % 4 {
		c.buf.WriteByte(0)
	}
}
