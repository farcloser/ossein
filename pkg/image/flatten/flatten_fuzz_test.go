package flatten_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/farcloser/ossein/pkg/image/flatten"
)

// fuzzSeedLayer is a small, well-formed layer exercising every entry kind the
// flattener reasons about: a directory, a file, a symlink, a hardlink, and a
// whiteout. The engine mutates from here.
func fuzzSeedLayer() []byte {
	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)
	entries := []struct {
		hdr  tar.Header
		body string
	}{
		{hdr: tar.Header{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "usr/a", Typeflag: tar.TypeReg, Mode: 0o644}, body: "hello"},
		{hdr: tar.Header{Name: "usr/b", Typeflag: tar.TypeLink, Linkname: "usr/a"}},
		{hdr: tar.Header{Name: "lib", Typeflag: tar.TypeSymlink, Linkname: "usr/lib"}},
		{hdr: tar.Header{Name: "etc/.wh.old", Typeflag: tar.TypeReg}},
	}

	for _, entry := range entries {
		entry.hdr.Size = int64(len(entry.body))
		_ = writer.WriteHeader(&entry.hdr)
		_, _ = io.WriteString(writer, entry.body)
	}

	_ = writer.Close()

	return buf.Bytes()
}

// FuzzExtract feeds the flattener a single arbitrary layer and holds it to the
// one contract a malformed archive must never break: an error, never a panic
// or a hang, and whatever tar does come out names each entry at most once.
func FuzzExtract(f *testing.F) {
	f.Add(fuzzSeedLayer())
	f.Add([]byte{})
	f.Add([]byte("not a tar at all"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		})
		if err != nil {
			return // a blob the registry library itself refuses never reaches Extract
		}

		img, err := mutate.AppendLayers(empty.Image, layer)
		if err != nil {
			return
		}

		out := flatten.Extract(img)
		defer func() { _ = out.Close() }()

		seen := map[string]bool{}
		reader := tar.NewReader(out)

		for {
			hdr, err := reader.Next()
			if errors.Is(err, io.EOF) {
				return
			}

			if err != nil {
				return // surfaced on the reader, as documented
			}

			if seen[hdr.Name] {
				t.Fatalf("flattened tar names %q twice", hdr.Name)
			}

			seen[hdr.Name] = true

			if _, err := io.Copy(io.Discard, reader); err != nil {
				return
			}
		}
	})
}
