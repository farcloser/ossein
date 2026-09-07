// Package rootfsblob holds the rootfs blob's self-describing format contract:
// the byte patterns the HOST writes into the cached blob and the GUEST reads
// back to decide how to materialize it.
//
// It lives in internal/ for the reason internal/protocol does — both worlds
// hardcode this independently and a mismatch breaks the pairing — but it is a
// different contract from the wire protocol, so it is a different package. The
// blob is not sent over the wire at all: the host attaches it as a read-only
// virtio-blk device and the guest reads it off the block layer.
//
// Why the format is sniffed rather than announced. The codec is a host-side
// choice that already rides the content-cache identifier (digest + codec +
// flatten generation), so a given blob's format is fixed at the moment it is
// written and can never change under a running guest. Sending it out of band
// as well would create a second source of truth that could disagree with the
// bytes — and the one path where that matters, a cache entry produced by a
// different ossein version, is exactly where the out-of-band channel would be
// least trustworthy. The bytes are authoritative; this package is the single
// place both sides agree on how to read them.
package rootfsblob

import "bytes"

// Format is what a rootfs blob contains.
type Format int

const (
	// FormatTar is a bare uncompressed tar. No codec produces one any more —
	// it is the fallback for anything unrecognized, which is deliberate: an
	// unknown blob is offered to the tar reader, whose error names an actual
	// structural problem, rather than to a speculative decoder whose error
	// would name the wrong layer. A blob reaching this branch means the cache
	// served something no current codec writes.
	FormatTar Format = iota
	// FormatLZ4 is a tar in an lz4 frame — the guest decodes and extracts.
	FormatLZ4
	// FormatEROFS is a mountable filesystem image — the guest mounts it
	// read-only under a tmpfs-upper overlay and extracts nothing.
	FormatEROFS
)

func (f Format) String() string {
	switch f {
	case FormatEROFS:
		return "erofs"
	case FormatLZ4:
		return "lz4"
	case FormatTar:
		return "tar"
	default:
		return "unknown"
	}
}

// EROFSSuperOffset is where an EROFS image carries its superblock, and
// erofsMagic is EROFS_SUPER_MAGIC_V1 (0xE0F5E1E2) as it appears there,
// little-endian.
const EROFSSuperOffset = 1024

//nolint:gochecknoglobals // immutable constant bytes; Go has no const []byte
var (
	erofsMagic = []byte{0xE2, 0xE1, 0xF5, 0xE0}
	// lz4Magic is the lz4 frame magic (0x184D2204, little-endian) that opens
	// every frame the host's encoder emits.
	lz4Magic = []byte{0x04, 0x22, 0x4D, 0x18}
)

// SniffLen is how many leading bytes Sniff needs to classify every format.
// A reader must be able to buffer at least this much before dispatching.
const SniffLen = EROFSSuperOffset + 4

// Sniff classifies the head of a rootfs blob. head may be shorter than
// SniffLen — a short device read, or simply a blob smaller than a superblock —
// in which case the formats whose marker lies beyond it are correctly ruled
// out rather than guessed at.
//
// EROFS is tested first and at a FIXED offset, so it can never be confused
// with a compressed stream whose framing lives at byte 0, nor matched by an
// archive that happens to contain those four bytes anywhere else.
func Sniff(head []byte) Format {
	if len(head) >= EROFSSuperOffset+len(erofsMagic) &&
		bytes.Equal(head[EROFSSuperOffset:EROFSSuperOffset+len(erofsMagic)], erofsMagic) {
		return FormatEROFS
	}

	if len(head) >= len(lz4Magic) && bytes.Equal(head[:len(lz4Magic)], lz4Magic) {
		return FormatLZ4
	}

	return FormatTar
}
