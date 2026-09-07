package flatten

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
)

// spoolEntry is one shadowed entry retained for the rest of its layer: the
// header always, the content (at offset/size in the spool file) for regular
// files. materializedAs is set once a hardlink claims this inode, so every
// later link to the same lost name links there instead of duplicating the
// bytes.
type spoolEntry struct {
	hdr            tar.Header
	offset         int64
	size           int64
	materializedAs string
}

// spool retains the current layer's shadowed entries so hardlinks can still
// reach an inode whose name lost to a higher layer. Content goes to one
// temp file, appended sequentially and read back with ReadAt; reset
// truncates it at each layer boundary, and most layers shadow nothing, so
// the file is created lazily and usually not at all.
type spool struct {
	file    *os.File
	entries map[string]spoolEntry
	offset  int64
}

func (s *spool) reset() error {
	s.entries = map[string]spoolEntry{}
	s.offset = 0

	if s.file == nil {
		return nil
	}

	if err := s.file.Truncate(0); err != nil {
		return fmt.Errorf("%w: resetting spool: %w", ErrFlatten, err)
	}

	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: resetting spool: %w", ErrFlatten, err)
	}

	return nil
}

func (s *spool) add(hdr *tar.Header, content io.Reader) error {
	entry := spoolEntry{hdr: *hdr, offset: -1}

	if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeGNUSparse {
		if s.file == nil {
			file, err := os.CreateTemp("", "ossein-flatten-spool-*")
			if err != nil {
				return fmt.Errorf("%w: creating spool: %w", ErrFlatten, err)
			}

			s.file = file
		}

		written, err := io.Copy(s.file, content)
		if err != nil {
			return fmt.Errorf("%w: spooling %q: %w", ErrFlatten, hdr.Name, err)
		}

		entry.offset = s.offset
		entry.size = written
		s.offset += written
	}

	s.entries[hdr.Name] = entry

	return nil
}

// content returns a reader over a spooled regular file's bytes.
func (s *spool) content(entry spoolEntry) io.Reader {
	return io.NewSectionReader(s.file, entry.offset, entry.size)
}

func (s *spool) close() {
	if s.file == nil {
		return
	}

	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
	s.file = nil
}
