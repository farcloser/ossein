//go:build darwin && arm64

package image

// This produces the ONLY rootfs format ossein stores (rootfsCodec in
// image.go): an uncompressed EROFS filesystem image, written in pure Go via
// github.com/forkcloser/erofs — the fork of erofs/go-erofs carrying
// Writer.Link (shared-inode hardlinks).
//
// Pure Go rather than shelling out to mkfs.erofs — which is what the
// migration prototyped with, and what the git history holds — is not a
// stylistic preference: mkfs.erofs is an unpinnable host dependency
// (brew install erofs-utils) on a project whose entire supply chain is
// otherwise pinned and checksum-verified, it cannot run in CI at all, and it
// defaults its block size to the BUILD host's page size — 16KiB on Apple
// Silicon — which the 4KiB-page guest kernel refuses to mount.
//
// The adapter leans on pkg/image/flatten's output invariants — normalized
// names, each name at most once, no whiteouts, TypeLink strictly after its
// target — so entries stream straight into the Writer with no staging pass.
// A tar entry the flattener could not produce is therefore not handled
// defensively here; that contract is flatten's to keep.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	goerofs "github.com/forkcloser/erofs"
)

// erofsBlockSize pins the fs block size to the GUEST page size; the library
// would otherwise be free to choose, and anything above 4096 is unmountable
// on the 4KiB-page guest kernel (same constraint as mkfs.erofs -b4096).
const erofsBlockSize = 4096

// buildGoEROFS converts the flattened tar stream into an uncompressed EROFS
// image in a temp file and returns it for the content store to consume.
// WithBuildTime(0,0) keeps the bytes deterministic for the healing/verify
// path — entry timestamps come from the tar, only fs-level defaults use it.
func buildGoEROFS(tarStream io.ReadCloser) (io.ReadCloser, error) {
	tmpDir, err := os.MkdirTemp("", "ossein-goerofs-*")
	if err != nil {
		_ = tarStream.Close()

		return nil, fmt.Errorf("%w: goerofs staging dir: %w", ErrCache, err)
	}

	imgPath := filepath.Join(tmpDir, "rootfs.erofs")

	img, err := os.Create(imgPath) // #nosec G304 -- fresh private temp dir
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		_ = tarStream.Close()

		return nil, fmt.Errorf("%w: goerofs image file: %w", ErrCache, err)
	}

	convErr := convertTarToEROFS(tarStream, img, tmpDir)

	closeErr := errors.Join(img.Close(), tarStream.Close())
	if err := errors.Join(convErr, closeErr); err != nil {
		_ = os.RemoveAll(tmpDir)

		return nil, fmt.Errorf("%w: goerofs conversion: %w", ErrCache, err)
	}

	reopened, err := os.Open(imgPath) // #nosec G304 -- fresh private temp dir
	if err != nil {
		_ = os.RemoveAll(tmpDir)

		return nil, fmt.Errorf("%w: reopening goerofs image: %w", ErrCache, err)
	}

	return newPaddedReader(&removeOnClose{File: reopened, dir: tmpDir}), nil
}

//nolint:gocognit,cyclop,funlen // linear tar-entry dispatch, one case per type
func convertTarToEROFS(tarStream io.Reader, img *os.File, tmpDir string) error {
	writer := goerofs.Create(img,
		goerofs.WithBlockSize(erofsBlockSize),
		goerofs.WithBuildTime(0, 0),
		goerofs.WithTempDir(tmpDir),
	)

	tarReader := tar.NewReader(tarStream)

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name := erofsPath(hdr.Name)
		if name == "/" {
			// Root exists implicitly; apply its metadata only.
			applyEROFSMetadata(writer, "/", hdr)

			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := writer.Mkdir(name, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("%s: mkdir: %w", name, err)
			}

		case tar.TypeReg, tar.TypeGNUSparse:
			if err := erofsWriteFile(writer, name, tarReader); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}

		case tar.TypeSymlink:
			if err := writer.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("%s: symlink: %w", name, err)
			}

		case tar.TypeLink:
			// A real shared-inode hardlink (fork's Writer.Link). The target's
			// inode already carries its metadata, and a link entry's own
			// header fields are often zeroed, so they must NOT be re-applied.
			if err := writer.Link(erofsPath(hdr.Linkname), name); err != nil {
				return fmt.Errorf("%s: link: %w", name, err)
			}

			continue // metadata lives on the shared inode

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if err := writer.Mknod(name, erofsRawMode(hdr), erofsRdev(hdr)); err != nil {
				return fmt.Errorf("%s: mknod: %w", name, err)
			}

		default:
			continue // skip unsupported entry types
		}

		applyEROFSMetadata(writer, name, hdr)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize erofs: %w", err)
	}

	return nil
}

func erofsWriteFile(writer *goerofs.Writer, name string, content io.Reader) error {
	file, err := writer.Create(name)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()

		return fmt.Errorf("write: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

// applyEROFSMetadata sets ownership, mode, times and xattrs. Best-effort on
// the root entry (the library may refuse some root operations), strict
// elsewhere would be nicer but the Writer carries a sticky error: any
// failure resurfaces at Close.
func applyEROFSMetadata(writer *goerofs.Writer, name string, hdr *tar.Header) {
	_ = writer.Chown(name, hdr.Uid, hdr.Gid)

	// Mkdir/Create already applied a mode, but tar modes carry
	// setuid/setgid/sticky which fs.FileMode encodes separately —
	// FileInfo().Mode() converts, Chmod preserves type bits.
	if hdr.Typeflag != tar.TypeSymlink {
		_ = writer.Chmod(name, hdr.FileInfo().Mode())
	}

	if !hdr.ModTime.IsZero() {
		atime := hdr.AccessTime
		if atime.IsZero() {
			atime = hdr.ModTime
		}

		_ = writer.Chtimes(name, atime, hdr.ModTime)
	}

	// Sorted keys: map range order is random per process, and xattr
	// APPLICATION order becomes on-disk LAYOUT order — unsorted, the image
	// bytes stop being reproducible and the cache's healing verify breaks.
	for _, key := range slices.Sorted(maps.Keys(hdr.PAXRecords)) {
		if attr, ok := strings.CutPrefix(key, paxXattrPrefix); ok {
			_ = writer.Setxattr(name, attr, hdr.PAXRecords[key])
		}
	}
}

// paxXattrPrefix is the PAX record key prefix libarchive/GNU tar use to
// carry extended attributes (notably security.capability).
const paxXattrPrefix = "SCHILY.xattr."

// erofsPath normalizes a tar entry name to the Writer's absolute-path form.
func erofsPath(name string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "./"))

	return cleaned
}

// erofsRawMode builds the raw stat mode (type bits | permissions) Mknod
// expects.
func erofsRawMode(hdr *tar.Header) uint16 {
	perm := uint16(hdr.Mode & 0o7777)

	switch hdr.Typeflag {
	case tar.TypeChar:
		return statTypeChr | perm
	case tar.TypeBlock:
		return statTypeBlk | perm
	default:
		return statTypeFifo | perm
	}
}

// Raw stat type bits (sys/stat.h), shared across unixes.
const (
	statTypeFifo uint16 = 0o010000
	statTypeChr  uint16 = 0o020000
	statTypeBlk  uint16 = 0o060000
)

// rdev encoding, Linux new_encode_dev layout: low byte of minor, then 12
// bits of major, then the minor's high bits — the old major<<8|minor form is
// a subset for small numbers (all container device nodes in practice).
const (
	rdevMinorLowMask  = 0xff
	rdevMajorShift    = 8
	rdevMinorHighward = 12
)

// erofsRdev encodes a tar entry's major/minor for Mknod.
func erofsRdev(hdr *tar.Header) uint32 {
	major := uint32(hdr.Devmajor) // #nosec G115 -- tar device numbers fit dev_t
	minor := uint32(hdr.Devminor) // #nosec G115 -- tar device numbers fit dev_t

	return (minor & rdevMinorLowMask) | (major << rdevMajorShift) | ((minor &^ rdevMinorLowMask) << rdevMinorHighward)
}
