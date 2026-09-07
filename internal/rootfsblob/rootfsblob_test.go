package rootfsblob_test

import (
	"testing"

	"github.com/farcloser/ossein/internal/rootfsblob"
)

// erofsHead returns bytes shaped like the head of an EROFS image: the
// superblock magic at its fixed offset, nothing meaningful around it.
func erofsHead(trailing int) []byte {
	head := make([]byte, rootfsblob.SniffLen+trailing)
	copy(head[rootfsblob.EROFSSuperOffset:], []byte{0xE2, 0xE1, 0xF5, 0xE0})

	return head
}

// TestSniff pins the dispatch that decides mount-vs-extract on every boot. It
// is the only place the guest learns which codec the host used, so a
// misclassification is an unbootable rootfs reported as a parse error from the
// wrong layer.
func TestSniff(t *testing.T) {
	t.Parallel()

	lz4Head := append([]byte{0x04, 0x22, 0x4D, 0x18}, make([]byte, 64)...)

	// A tar header: a path at byte 0 and the ustar magic at 257.
	tarHead := make([]byte, 512)
	copy(tarHead, "etc/hostname")
	copy(tarHead[257:], "ustar")

	// The pathological case the fixed offset exists for: an archive whose
	// payload happens to contain the EROFS magic, but not at the superblock.
	tarWithErofsBytesInside := make([]byte, rootfsblob.SniffLen)
	copy(tarWithErofsBytesInside, "etc/hostname")
	copy(tarWithErofsBytesInside[600:], []byte{0xE2, 0xE1, 0xF5, 0xE0})

	for _, testCase := range []struct {
		name string
		head []byte
		want rootfsblob.Format
	}{
		{"erofs", erofsHead(0), rootfsblob.FormatEROFS},
		{"erofs with trailing bytes", erofsHead(4096), rootfsblob.FormatEROFS},
		{"lz4", lz4Head, rootfsblob.FormatLZ4},
		{"plain tar", tarHead, rootfsblob.FormatTar},
		{"empty", nil, rootfsblob.FormatTar},
		{"truncated below the superblock", make([]byte, rootfsblob.EROFSSuperOffset), rootfsblob.FormatTar},
		{"one byte short of the magic", make([]byte, rootfsblob.SniffLen-1), rootfsblob.FormatTar},
		{"erofs magic at offset 0 is not erofs", append([]byte{0xE2, 0xE1, 0xF5, 0xE0}, make([]byte, 2048)...), rootfsblob.FormatTar},
		{"erofs magic mid-archive is not erofs", tarWithErofsBytesInside, rootfsblob.FormatTar},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := rootfsblob.Sniff(testCase.head); got != testCase.want {
				t.Fatalf("Sniff = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestSniffLenCoversEveryMarker guards the constant callers size their read
// buffer from: if a format's marker ever moved past SniffLen, every caller
// would silently stop being able to detect it.
func TestSniffLenCoversEveryMarker(t *testing.T) {
	t.Parallel()

	if rootfsblob.SniffLen < rootfsblob.EROFSSuperOffset+4 {
		t.Fatalf("SniffLen %d cannot reach the EROFS superblock magic at %d",
			rootfsblob.SniffLen, rootfsblob.EROFSSuperOffset)
	}

	// Exactly SniffLen bytes must be enough — no caller reads more.
	if got := rootfsblob.Sniff(erofsHead(0)); got != rootfsblob.FormatEROFS {
		t.Fatalf("Sniff of exactly SniffLen bytes = %v, want FormatEROFS", got)
	}
}
