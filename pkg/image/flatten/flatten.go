// Package flatten squashes an OCI image's layer stack into one plain tar
// stream. It is ossein's fork of go-containerregistry's mutate.Extract
// (adapted from pkg/v1/mutate, Copyright 2018 Google LLC, Apache License
// 2.0), kept in-tree because upstream's streaming flatten mishandles three
// things ossein depends on:
//
//   - opaque whiteouts (.wh..wh..opq) were ignored entirely, so files an
//     image deleted by replacing a directory reappeared in the output;
//   - a hardlink whose target a higher layer deleted was emitted dangling
//     (go-containerregistry#977) — the inode survives under the link's name
//     on a real layered filesystem, so it is materialized as a regular file
//     here, while the guest would have failed the boot on the dangling link;
//   - a hardlink was emitted wherever its layer put it, so a link could
//     precede its target in the stream, and a link to a file a higher layer
//     overwrote silently acquired the new content instead of the inode it
//     actually shares.
//
// Layers are walked TOP FIRST. That is not an optimization: the stream
// cannot retract an entry once written, and only in top-first order is the
// first occurrence of a name its winning version, making every decision
// final at the moment an entry is scanned. The price is that entries arrive
// in no parent-first order and a link's target may flow later — the pending
// and spool machinery below exists to pay exactly that price.
//
// The output holds invariants the guest extractor relies on: entry names are
// normalized (no leading "/", "./" or ".." segments); every name appears at
// most once; a TypeLink entry always appears after the entry it links to; no
// whiteout markers of any kind survive.
package flatten

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1"
)

const (
	// whiteoutPrefix marks a deleted sibling: "dir/.wh.foo" deletes "dir/foo"
	// from every lower layer.
	whiteoutPrefix = ".wh."

	// opaqueMarker marks its directory opaque: everything under it in LOWER
	// layers is hidden, while the marker's own layer contributes normally.
	opaqueMarker = ".wh..wh..opq"

	// sep is the tar path separator (tar names always use forward slashes).
	sep = "/"

	// maxLinkChain bounds hardlink-to-hardlink target chains during
	// materialization; real archives chain once, so this only stops a
	// hostile cycle.
	maxLinkChain = 16

	// logName and logTarget are the slog attribute keys of the dropped-link
	// warnings.
	logName   = "name"
	logTarget = "target"
)

// Extract returns the image's flattened filesystem as an uncompressed tar
// stream. Errors during extraction surface on the reader; the caller must
// Close it.
func Extract(img v1.Image) io.ReadCloser {
	pipeReader, pipeWriter := io.Pipe()

	go func() {
		_ = pipeWriter.CloseWithError(run(img, pipeWriter))
	}()

	return pipeReader
}

// decision is the settled fate of one name: which layer decided it, whether
// bytes were written for it, and whether it shadows the same name and its
// children in lower layers (any non-directory, tombstone, or parked link).
type decision struct {
	layer   int
	emitted bool
	hides   bool
}

// pendingLink is a hardlink entry whose target had not flowed when the link
// was scanned. layer is the link's own layer: the version of the target the
// link shares an inode with is the topmost one at or below that layer.
type pendingLink struct {
	hdr   tar.Header
	layer int
}

// flattener carries the cross-layer bookkeeping for one Extract run.
type flattener struct {
	out *tar.Writer

	// decided maps every settled name to its fate. A name present here is
	// never emitted again — the output owes each name at most one entry.
	decided map[string]decision

	// tombstones and opaques record deletion events with the layer that
	// declared them. Scan-time hiding only needs "is there any event above
	// me", but hardlink resolution needs the layer, to test whether a
	// candidate target version was still visible when the link was created.
	tombstones map[string][]int
	opaques    map[string][]int

	// pending parks hardlinks by target name until a usable version of the
	// target flows by; leftovers are dropped (with a warning) at the end.
	pending map[string][]pendingLink

	// spool retains the current layer's shadowed entries — headers, plus
	// content for regular files — so a link scanned later can still reach
	// the inode whose name lost to a higher layer (the #977 shape: a layer
	// ships file+links, a later layer deletes the file's name).
	spool spool
}

func run(img v1.Image, out io.Writer) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("%w: listing layers: %w", ErrFlatten, err)
	}

	flat := &flattener{
		out:        tar.NewWriter(out),
		decided:    map[string]decision{},
		tombstones: map[string][]int{},
		opaques:    map[string][]int{},
		pending:    map[string][]pendingLink{},
	}
	defer flat.spool.close()

	for idx, layer := range slices.Backward(layers) {
		if err := flat.layer(idx, layer); err != nil {
			return err
		}
	}

	flat.dropDangling()

	if err := flat.out.Close(); err != nil {
		return fmt.Errorf("%w: closing output: %w", ErrFlatten, err)
	}

	return nil
}

func (f *flattener) layer(layerIdx int, layer v1.Layer) error {
	reader, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("%w: opening layer %d: %w", ErrFlatten, layerIdx, err)
	}
	defer func() { _ = reader.Close() }()

	if err := f.spool.reset(); err != nil {
		return err
	}

	tarReader := tar.NewReader(reader)

	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("%w: reading layer %d: %w", ErrFlatten, layerIdx, err)
		}

		if err := f.entry(layerIdx, hdr, tarReader); err != nil {
			return err
		}
	}

	// Drain trailing bytes the tar reader did not consume, so the verifying
	// reader underneath reaches EOF and the layer digest is actually checked.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("%w: verifying layer %d: %w", ErrFlatten, layerIdx, err)
	}

	return nil
}

func (f *flattener) entry(layerIdx int, hdr *tar.Header, content *tar.Reader) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}

	hdr.Name = normalize(hdr.Name)
	if hdr.Name == "" {
		return nil // the root itself; nothing to emit
	}

	// PAX lifts USTAR's 100-char name limit and never depends on the
	// writer's format guess.
	hdr.Format = tar.FormatPAX

	dirname := filepath.Dir(hdr.Name)
	basename := filepath.Base(hdr.Name)

	if basename == opaqueMarker {
		f.opaques[dirname] = append(f.opaques[dirname], layerIdx)

		return nil
	}

	if deleted, isWhiteout := strings.CutPrefix(basename, whiteoutPrefix); isWhiteout {
		target := filepath.Join(dirname, deleted)
		f.tombstones[target] = append(f.tombstones[target], layerIdx)

		if _, ok := f.decided[target]; !ok {
			f.decided[target] = decision{layer: layerIdx, hides: true}
		}

		return nil
	}

	if escapingLink(hdr) {
		return nil
	}

	if _, seen := f.decided[hdr.Name]; seen || f.hiddenAt(hdr.Name, layerIdx) {
		return f.skipShadowed(layerIdx, hdr, content)
	}

	if hdr.Typeflag == tar.TypeLink {
		return f.link(layerIdx, hdr, 0)
	}

	return f.emit(layerIdx, hdr, content)
}

// hiddenAt reports whether name is shadowed through an ancestor: one a
// higher layer turned into a non-directory, deleted, or marked opaque. The
// exact-name case is the caller's `decided` check. Any recorded tombstone
// hides (events are recorded at layers at or above the current scan, and a
// same-layer whiteout-plus-child is malformed input that upstream also
// hides); an opaque marker spares its own layer, per the OCI spec.
func (f *flattener) hiddenAt(name string, layerIdx int) bool {
	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		if d, ok := f.decided[dir]; ok && d.hides {
			return true
		}

		if len(f.tombstones[dir]) > 0 {
			return true
		}

		for _, opaqueLayer := range f.opaques[dir] {
			if opaqueLayer > layerIdx {
				return true
			}
		}

		if dir == "." {
			return false
		}
	}
}

// emit writes one winning entry and links any parked hardlinks that were
// waiting for it.
func (f *flattener) emit(layerIdx int, hdr *tar.Header, content io.Reader) error {
	// archive/tar's writer cannot re-encode GNU sparse entries, and the
	// reader already expands them; emit the expanded bytes as a plain file.
	if hdr.Typeflag == tar.TypeGNUSparse {
		hdr.Typeflag = tar.TypeReg
	}

	if hdr.Typeflag == tar.TypeLink {
		hdr.Linkname = normalize(hdr.Linkname)
	}

	if err := f.out.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%w: writing %q: %w", ErrFlatten, hdr.Name, err)
	}

	if hdr.Size > 0 {
		if _, err := io.Copy(f.out, content); err != nil {
			return fmt.Errorf("%w: writing %q: %w", ErrFlatten, hdr.Name, err)
		}
	}

	f.decided[hdr.Name] = decision{
		layer: layerIdx, emitted: true, hides: hdr.Typeflag != tar.TypeDir,
	}

	return f.resolveAgainstEmitted(layerIdx, hdr.Name)
}

// link handles a fresh hardlink entry: link to its target if the emitted
// target is the version this link shares an inode with, materialize from the
// spool when the target's name lost to a higher layer, or park it for a
// lower layer to satisfy. depth guards target chains (a link to a link).
func (f *flattener) link(layerIdx int, hdr *tar.Header, depth int) error {
	if depth > maxLinkChain {
		slog.Default().Warn("dropping hardlink with a cyclic target chain",
			logName, hdr.Name, logTarget, hdr.Linkname)

		return nil
	}

	target := normalize(hdr.Linkname)
	hdr.Linkname = target

	// A self-target is malformed, and letting one park on its own name
	// would re-enter that name's pending list while it is being settled.
	if target == hdr.Name {
		slog.Default().Warn("dropping self-targeting hardlink", logName, hdr.Name)

		return nil
	}

	// Emitted target from a layer at or below the link's own layer is the
	// exact version the link was created against (an emitted name is the
	// topmost occurrence, so nothing above could have replaced it).
	if d, ok := f.decided[target]; ok && d.emitted && d.layer <= layerIdx {
		return f.emit(layerIdx, hdr, nil)
	}

	if spooled, ok := f.spool.entries[target]; ok {
		return f.materialize(layerIdx, hdr, spooled, depth)
	}

	f.pending[target] = append(f.pending[target], pendingLink{hdr: *hdr, layer: layerIdx})
	f.decided[hdr.Name] = decision{layer: layerIdx, hides: true}

	return nil
}

// materialize writes a hardlink whose target name did not survive: the inode
// does, under the link's name. The spooled header carries the inode's real
// metadata (mode, owner, xattrs); only the name changes.
func (f *flattener) materialize(layerIdx int, linkHdr *tar.Header, spooled spoolEntry, depth int) error {
	if spooled.materializedAs != "" {
		clone := *linkHdr
		clone.Typeflag = tar.TypeLink
		clone.Linkname = spooled.materializedAs
		clone.Size = 0

		return f.emit(layerIdx, &clone, nil)
	}

	clone := spooled.hdr
	clone.Name = linkHdr.Name

	switch spooled.hdr.Typeflag {
	case tar.TypeReg, tar.TypeGNUSparse:
		spooled.materializedAs = linkHdr.Name
		f.spool.entries[normalize(spooled.hdr.Name)] = spooled

		clone.Typeflag = tar.TypeReg
		clone.Size = spooled.size

		return f.emit(layerIdx, &clone, f.spool.content(spooled))
	case tar.TypeLink:
		// The shadowed target was itself a hardlink: chase its target.
		clone = *linkHdr
		clone.Linkname = spooled.hdr.Linkname

		return f.link(layerIdx, &clone, depth+1)
	default:
		// Symlinks, devices and fifos clone cleanly (same target, same
		// device numbers); directories as link targets are malformed.
		if spooled.hdr.Typeflag == tar.TypeDir {
			slog.Default().Warn("dropping hardlink to a directory",
				logName, linkHdr.Name, logTarget, linkHdr.Linkname)

			return nil
		}

		return f.emit(layerIdx, &clone, nil)
	}
}

// skipShadowed handles an entry whose name lost to a higher layer. Its bytes
// may still be owed to a hardlink: one parked from a higher layer, or one
// later in this same layer (tar order puts a link after its target, so a
// same-layer link to this name has not been scanned yet). Spool it for the
// rest of this layer, then settle any parked links it can serve.
func (f *flattener) skipShadowed(layerIdx int, hdr *tar.Header, content io.Reader) error {
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeGNUSparse, tar.TypeSymlink, tar.TypeLink:
	default:
		return nil
	}

	name := hdr.Name
	if err := f.spool.add(hdr, content); err != nil {
		return err
	}

	return f.resolveAgainstShadowed(layerIdx, name)
}

// resolveAgainstEmitted settles parked links after name was emitted at
// emittedLayer: every parked link from that layer or above shares its inode
// (the emitted version is the topmost, so it is what those links saw).
// Parked links from LOWER layers predate this version and keep waiting.
func (f *flattener) resolveAgainstEmitted(emittedLayer int, name string) error {
	var kept []pendingLink

	for _, parked := range f.pending[name] {
		if parked.layer < emittedLayer {
			kept = append(kept, parked)

			continue
		}

		clone := parked.hdr
		clone.Linkname = name

		if err := f.emit(parked.layer, &clone, nil); err != nil {
			return err
		}
	}

	f.settlePending(name, kept)

	return nil
}

// resolveAgainstShadowed settles parked links against a shadowed version of
// name that just flowed by (and was spooled): a parked link shares this
// version's inode when no deletion event separates them — nothing whited
// out or opaqued the name between this layer (exclusive) and the link's
// layer (inclusive). An overwrite cannot separate them: the overwriting
// version flowed earlier and already settled every link it satisfied.
func (f *flattener) resolveAgainstShadowed(layerIdx int, name string) error {
	var kept []pendingLink

	for _, parked := range f.pending[name] {
		if !f.visibleTo(name, layerIdx, parked.layer) {
			kept = append(kept, parked)

			continue
		}

		spooled := f.spool.entries[name]
		if err := f.materialize(parked.layer, &parked.hdr, spooled, 0); err != nil {
			return err
		}
	}

	f.settlePending(name, kept)

	return nil
}

// visibleTo reports whether the version of name declared at srcLayer still
// existed when linkLayer was built: no whiteout of the name or an ancestor,
// and no opaque marker over an ancestor, strictly above srcLayer and at or
// below linkLayer.
func (f *flattener) visibleTo(name string, srcLayer, linkLayer int) bool {
	if anyInWindow(f.tombstones[name], srcLayer, linkLayer) {
		return false
	}

	for dir := filepath.Dir(name); ; dir = filepath.Dir(dir) {
		if anyInWindow(f.tombstones[dir], srcLayer, linkLayer) {
			return false
		}

		if anyInWindow(f.opaques[dir], srcLayer, linkLayer) {
			return false
		}

		if dir == "." {
			return true
		}
	}
}

func (f *flattener) settlePending(name string, kept []pendingLink) {
	if len(kept) == 0 {
		delete(f.pending, name)

		return
	}

	f.pending[name] = kept
}

// dropDangling warns about parked links no layer could satisfy — the image
// references an inode that does not exist in its own final filesystem.
// Emitting them dangling (upstream's behavior) would fail extraction in the
// guest; dropping loses a name the image itself broke.
func (f *flattener) dropDangling() {
	for target, parked := range f.pending {
		for _, link := range parked {
			slog.Default().Warn("dropping dangling hardlink",
				logName, link.hdr.Name, logTarget, target)
		}
	}
}

// normalize maps a tar name to the single spelling used for every map key
// and emitted entry: no leading "/" or "./", no ".." segments (they clamp to
// the root, which the guest rejects anyway), "" for the root itself. Rooting
// the path before cleaning is what makes "etc", "/etc" and "./etc" collide
// instead of coexisting.
func normalize(name string) string {
	return strings.TrimPrefix(filepath.Clean(sep+name), sep)
}

// escapingLink reports whether a symlink or hardlink entry has a RELATIVE
// target that climbs out of the rootfs; such entries are dropped, matching
// upstream. Absolute targets are kept verbatim: symlinks are resolved (and
// clamped) at extraction time, and hardlink targets are normalized against
// the root when the link is handled.
func escapingLink(hdr *tar.Header) bool {
	if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeLink {
		return false
	}

	if filepath.IsAbs(hdr.Linkname) {
		return false
	}

	// #nosec G305 -- the joined path is only inspected, never opened
	resolved := filepath.Clean(filepath.Join(filepath.Dir(normalize(hdr.Name)), hdr.Linkname))

	return strings.HasPrefix(resolved, "..")
}

func anyInWindow(layers []int, above, atOrBelow int) bool {
	for _, eventLayer := range layers {
		if eventLayer > above && eventLayer <= atOrBelow {
			return true
		}
	}

	return false
}
