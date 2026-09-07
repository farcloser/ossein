package flatten_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/farcloser/ossein/pkg/image/flatten"
)

// entry is one tar member of a synthetic layer; body applies to regular files.
type entry struct {
	name     string
	typeflag byte
	linkname string
	body     string
	mode     int64
}

// extracted is one entry of the flattened output, in stream order.
type extracted struct {
	hdr  tar.Header
	body string
}

func makeLayerTar(t *testing.T, entries []entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	for _, item := range entries {
		mode := item.mode
		if mode == 0 {
			mode = 0o644
		}

		hdr := tar.Header{
			Name:     item.name,
			Typeflag: item.typeflag,
			Linkname: item.linkname,
			Mode:     mode,
			Size:     int64(len(item.body)),
		}
		if err := writer.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", item.name, err)
		}

		if item.body != "" {
			if _, err := writer.Write([]byte(item.body)); err != nil {
				t.Fatalf("write body %q: %v", item.name, err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close layer tar: %v", err)
	}

	return buf.Bytes()
}

// makeImage assembles an image from layers given BASE FIRST (the order a
// Dockerfile builds them).
func makeImage(t *testing.T, layers ...[]entry) v1.Image {
	t.Helper()

	img := empty.Image

	for _, layerEntries := range layers {
		raw := makeLayerTar(t, layerEntries)

		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		})
		if err != nil {
			t.Fatalf("layer from tar: %v", err)
		}

		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			t.Fatalf("append layer: %v", err)
		}
	}

	return img
}

func extractAll(t *testing.T, img v1.Image) []extracted {
	t.Helper()

	reader := flatten.Extract(img)
	defer func() { _ = reader.Close() }()

	var out []extracted

	tarReader := tar.NewReader(reader)

	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("read flattened stream: %v", err)
		}

		body, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read body of %q: %v", hdr.Name, err)
		}

		out = append(out, extracted{hdr: *hdr, body: string(body)})
	}

	return out
}

func find(entries []extracted, name string) (extracted, bool) {
	for _, item := range entries {
		if item.hdr.Name == name {
			return item, true
		}
	}

	return extracted{}, false
}

func names(entries []extracted) []string {
	out := make([]string, 0, len(entries))
	for _, item := range entries {
		out = append(out, item.hdr.Name)
	}

	return out
}

func indexOf(entries []extracted, name string) int {
	for idx, item := range entries {
		if item.hdr.Name == name {
			return idx
		}
	}

	return -1
}

// TestTopLayerWinsAndWhiteoutDeletes covers the base semantics: dedup by
// name (top version wins, exactly one entry per name), plain whiteouts
// delete, directories merge across layers.
func TestTopLayerWinsAndWhiteoutDeletes(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "etc", typeflag: tar.TypeDir},
			{name: "etc/keep", typeflag: tar.TypeReg, body: "keep"},
			{name: "etc/gone", typeflag: tar.TypeReg, body: "gone"},
			{name: "etc/config", typeflag: tar.TypeReg, body: "v1"},
		},
		[]entry{
			{name: "etc/.wh.gone", typeflag: tar.TypeReg},
			{name: "etc/config", typeflag: tar.TypeReg, body: "v2"},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "etc/gone"); found {
		t.Error("whited-out file survived the flatten")
	}

	config, found := find(out, "etc/config")
	if !found || config.body != "v2" {
		t.Errorf("etc/config = %+v, want top-layer v2", config)
	}

	keep, found := find(out, "etc/keep")
	if !found || keep.body != "keep" {
		t.Errorf("etc/keep = %+v, want preserved", keep)
	}

	got := names(out)
	slices.Sort(got)

	if dup := slices.Compact(slices.Clone(got)); len(dup) != len(got) {
		t.Errorf("duplicate names in output: %v", got)
	}

	if _, found := find(out, "etc/.wh.gone"); found {
		t.Error("whiteout marker leaked into the output")
	}
}

// TestOpaqueWhiteoutHidesLowerContents is the regression test for the fork's
// reason to exist: an opaque marker must hide the directory's lower-layer
// contents while its own layer's contents survive.
func TestOpaqueWhiteoutHidesLowerContents(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "dir", typeflag: tar.TypeDir},
			{name: "dir/old", typeflag: tar.TypeReg, body: "old"},
			{name: "dir/sub", typeflag: tar.TypeDir},
			{name: "dir/sub/deep", typeflag: tar.TypeReg, body: "deep"},
		},
		[]entry{
			{name: "dir", typeflag: tar.TypeDir},
			{name: "dir/.wh..wh..opq", typeflag: tar.TypeReg},
			{name: "dir/new", typeflag: tar.TypeReg, body: "new"},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "dir/old"); found {
		t.Error("opaque whiteout did not hide a lower-layer file")
	}

	if _, found := find(out, "dir/sub/deep"); found {
		t.Error("opaque whiteout did not hide a lower-layer subdirectory's file")
	}

	newFile, found := find(out, "dir/new")
	if !found || newFile.body != "new" {
		t.Errorf("dir/new = %+v, want same-layer file to survive the marker", newFile)
	}

	if _, found := find(out, "dir"); !found {
		t.Error("opaqued directory itself must survive")
	}

	if _, found := find(out, "dir/.wh..wh..opq"); found {
		t.Error("opaque marker leaked into the output")
	}
}

// TestWhiteoutThenRecreate: deleting a directory and recreating it later
// must not resurrect the old contents under the new directory.
func TestWhiteoutThenRecreate(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "d", typeflag: tar.TypeDir},
			{name: "d/old", typeflag: tar.TypeReg, body: "old"},
		},
		[]entry{
			{name: ".wh.d", typeflag: tar.TypeReg},
		},
		[]entry{
			{name: "d", typeflag: tar.TypeDir},
			{name: "d/new", typeflag: tar.TypeReg, body: "new"},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "d/old"); found {
		t.Error("file resurrected across a delete-then-recreate")
	}

	if _, found := find(out, "d/new"); !found {
		t.Error("recreated directory lost its file")
	}

	if _, found := find(out, "d"); !found {
		t.Error("recreated directory missing")
	}
}

// TestDanglingHardlinkMaterialized is the go-containerregistry#977 shape: a
// layer ships a file plus hardlinks to it, a later layer deletes the file's
// name. The inode survives under the links' names, so the first link becomes
// a regular file with the content and the second links to the first.
func TestDanglingHardlinkMaterialized(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "usr", typeflag: tar.TypeDir},
			{name: "usr/bin", typeflag: tar.TypeDir},
			{name: "usr/bin/git", typeflag: tar.TypeReg, body: "GIT", mode: 0o755},
			{name: "usr/lib", typeflag: tar.TypeDir},
			{name: "usr/lib/git-core", typeflag: tar.TypeLink, linkname: "usr/bin/git"},
			{name: "usr/lib/git-extra", typeflag: tar.TypeLink, linkname: "usr/bin/git"},
		},
		[]entry{
			{name: "usr/bin/.wh.git", typeflag: tar.TypeReg},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "usr/bin/git"); found {
		t.Error("whited-out link target survived")
	}

	core, found := find(out, "usr/lib/git-core")
	if !found {
		t.Fatal("first hardlink lost entirely")
	}

	if core.hdr.Typeflag != tar.TypeReg || core.body != "GIT" {
		t.Errorf("first hardlink = type %c body %q, want materialized regular file with GIT",
			core.hdr.Typeflag, core.body)
	}

	if core.hdr.Mode&0o777 != 0o755 {
		t.Errorf("materialized file mode = %o, want the inode's 755", core.hdr.Mode)
	}

	extra, found := find(out, "usr/lib/git-extra")
	if !found {
		t.Fatal("second hardlink lost entirely")
	}

	if extra.hdr.Typeflag != tar.TypeLink || extra.hdr.Linkname != "usr/lib/git-core" {
		t.Errorf("second hardlink = type %c target %q, want link to the materialized name",
			extra.hdr.Typeflag, extra.hdr.Linkname)
	}
}

// TestHardlinkOrderedAfterCrossLayerTarget: a link created in a higher layer
// than its target must still be emitted AFTER the target — the guest
// extractor creates entries in stream order and link(2) needs its source to
// exist.
func TestHardlinkOrderedAfterCrossLayerTarget(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "bin", typeflag: tar.TypeDir},
			{name: "bin/busybox", typeflag: tar.TypeReg, body: "BB"},
		},
		[]entry{
			{name: "bin/sh", typeflag: tar.TypeLink, linkname: "bin/busybox"},
		},
	)

	out := extractAll(t, img)

	target := indexOf(out, "bin/busybox")
	link := indexOf(out, "bin/sh")

	if target == -1 || link == -1 {
		t.Fatalf("missing entries: busybox@%d sh@%d in %v", target, link, names(out))
	}

	if link < target {
		t.Errorf("hardlink emitted before its target (link@%d, target@%d)", link, target)
	}

	if out[link].hdr.Typeflag != tar.TypeLink || out[link].hdr.Linkname != "bin/busybox" {
		t.Errorf("bin/sh = type %c target %q, want hardlink to bin/busybox",
			out[link].hdr.Typeflag, out[link].hdr.Linkname)
	}
}

// TestHardlinkToOverwrittenTargetKeepsOldContent: overwriting a file breaks
// its link family — the link keeps the OLD inode. Linking to the new name
// would silently swap the content.
func TestHardlinkToOverwrittenTargetKeepsOldContent(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "data", typeflag: tar.TypeReg, body: "old"},
			{name: "alias", typeflag: tar.TypeLink, linkname: "data"},
		},
		[]entry{
			{name: "data", typeflag: tar.TypeReg, body: "new"},
		},
	)

	out := extractAll(t, img)

	data, found := find(out, "data")
	if !found || data.body != "new" {
		t.Errorf("data = %+v, want top-layer content", data)
	}

	alias, found := find(out, "alias")
	if !found {
		t.Fatal("hardlink to overwritten target lost entirely")
	}

	if alias.hdr.Typeflag != tar.TypeReg || alias.body != "old" {
		t.Errorf("alias = type %c body %q, want materialized regular file with the OLD content",
			alias.hdr.Typeflag, alias.body)
	}
}

// TestSameLayerHardlinkStaysALink: the ordinary case — file and link in one
// layer, nothing above touches them — must remain a plain link, not get
// materialized into a copy.
func TestSameLayerHardlinkStaysALink(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "f", typeflag: tar.TypeReg, body: "x"},
			{name: "l", typeflag: tar.TypeLink, linkname: "f"},
		},
	)

	out := extractAll(t, img)

	link, found := find(out, "l")
	if !found || link.hdr.Typeflag != tar.TypeLink || link.hdr.Linkname != "f" {
		t.Errorf("l = %+v, want plain hardlink to f", link)
	}
}

// TestEscapingRelativeLinksDropped preserves upstream's guard: a relative
// symlink or hardlink target that climbs out of the rootfs is dropped, while
// in-root relative and absolute targets are kept verbatim.
func TestEscapingRelativeLinksDropped(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "dir", typeflag: tar.TypeDir},
			{name: "dir/escape", typeflag: tar.TypeSymlink, linkname: "../../outside"},
			{name: "dir/legit", typeflag: tar.TypeSymlink, linkname: "../etc/hosts"},
			{name: "abs", typeflag: tar.TypeSymlink, linkname: "/etc/hosts"},
			{name: "etc", typeflag: tar.TypeDir},
			{name: "etc/hosts", typeflag: tar.TypeReg, body: "h"},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "dir/escape"); found {
		t.Error("escaping relative symlink survived")
	}

	legit, found := find(out, "dir/legit")
	if !found || legit.hdr.Linkname != "../etc/hosts" {
		t.Errorf("in-root relative symlink = %+v, want preserved verbatim", legit)
	}

	abs, found := find(out, "abs")
	if !found || abs.hdr.Linkname != "/etc/hosts" {
		t.Errorf("absolute symlink = %+v, want preserved verbatim", abs)
	}
}

// TestNameSpellingsCollide: "./x", "/x" and "x" are one path and must dedup
// to one entry under the normalized spelling.
func TestNameSpellingsCollide(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "./etc/hosts", typeflag: tar.TypeReg, body: "dotted"},
		},
		[]entry{
			{name: "/etc/hosts", typeflag: tar.TypeReg, body: "rooted"},
		},
	)

	out := extractAll(t, img)

	hosts, found := find(out, "etc/hosts")
	if !found || hosts.body != "rooted" {
		t.Errorf("etc/hosts = %+v, want single normalized entry with top content", hosts)
	}

	for _, item := range out {
		if item.hdr.Name == "/etc/hosts" || item.hdr.Name == "./etc/hosts" {
			t.Errorf("unnormalized spelling leaked: %q", item.hdr.Name)
		}
	}
}

// TestDanglingLinkToNowhereDropped: a hardlink whose target never exists in
// any layer must be dropped, not emitted dangling (the guest would fail the
// whole extraction on it).
func TestDanglingLinkToNowhereDropped(t *testing.T) {
	t.Parallel()

	img := makeImage(t,
		[]entry{
			{name: "ok", typeflag: tar.TypeReg, body: "ok"},
			{name: "ghost", typeflag: tar.TypeLink, linkname: "no/such/file"},
		},
	)

	out := extractAll(t, img)

	if _, found := find(out, "ghost"); found {
		t.Error("dangling hardlink emitted")
	}

	if _, found := find(out, "ok"); !found {
		t.Error("healthy entry lost")
	}
}
