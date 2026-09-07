package rootfsblob_test

import (
	"bytes"
	"testing"

	"github.com/farcloser/ossein/internal/rootfsblob"
)

// The two markers, restated here rather than imported: the fuzz target checks
// the classifier against the documented byte contract, not against itself.
//
//nolint:gochecknoglobals // immutable test fixtures.
var (
	fuzzEROFSMagic = []byte{0xE2, 0xE1, 0xF5, 0xE0}
	fuzzLZ4Magic   = []byte{0x04, 0x22, 0x4D, 0x18}
)

// FuzzSniff pins the classifier to its contract for every input: total (every
// head names a format), exactly the documented byte tests and nothing looser,
// and a function of the first SniffLen bytes only.
func FuzzSniff(f *testing.F) {
	erofs := make([]byte, rootfsblob.SniffLen)
	copy(erofs[rootfsblob.EROFSSuperOffset:], fuzzEROFSMagic)
	f.Add(erofs)
	f.Add(append(append([]byte{}, fuzzLZ4Magic...), 0, 0, 0, 0))
	f.Add([]byte("ustar\x0000"))
	f.Add([]byte{})
	f.Add(make([]byte, rootfsblob.SniffLen-1)) // one byte short of the EROFS marker

	f.Fuzz(func(t *testing.T, head []byte) {
		got := rootfsblob.Sniff(head)
		if got.String() == "unknown" {
			t.Fatalf("Sniff returned an unnamed format %d", got)
		}

		wantEROFS := len(head) >= rootfsblob.SniffLen &&
			bytes.Equal(head[rootfsblob.EROFSSuperOffset:rootfsblob.SniffLen], fuzzEROFSMagic)
		wantLZ4 := !wantEROFS && bytes.HasPrefix(head, fuzzLZ4Magic)

		switch {
		case wantEROFS && got != rootfsblob.FormatEROFS:
			t.Fatalf("EROFS magic at %d: got %s", rootfsblob.EROFSSuperOffset, got)
		case wantLZ4 && got != rootfsblob.FormatLZ4:
			t.Fatalf("lz4 frame magic at 0: got %s", got)
		case !wantEROFS && !wantLZ4 && got != rootfsblob.FormatTar:
			t.Fatalf("no marker: got %s, want tar (the fallback)", got)
		default:
			// classified as the contract says
		}

		if len(head) > rootfsblob.SniffLen && rootfsblob.Sniff(head[:rootfsblob.SniffLen]) != got {
			t.Fatal("classification depends on bytes past SniffLen")
		}
	})
}
