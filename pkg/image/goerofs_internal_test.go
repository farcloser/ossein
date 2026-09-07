package image

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	goerofs "github.com/forkcloser/erofs"
)

// corpusTar builds an in-memory flattened-style tar exercising every entry
// shape the adapter must translate: nested dirs with setgid/sticky, files
// with setuid + multiple xattrs (sorted-application coverage), an empty
// file, a multi-block file, symlinks, hardlinks (to a file AND to a
// symlink), device nodes, a fifo, and mixed name forms ("./x", "x").
func corpusTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	tarWriter := tar.NewWriter(&buf)
	mtime := time.Unix(1700000000, 0)

	write := func(hdr *tar.Header, content []byte) {
		t.Helper()

		hdr.ModTime = mtime
		hdr.Format = tar.FormatPAX

		if err := tarWriter.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if content != nil {
			if _, err := tarWriter.Write(content); err != nil {
				t.Fatal(err)
			}
		}
	}

	write(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755}, nil)
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "./bin", Mode: 0o755}, nil)
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "tmp", Mode: 0o1777}, nil)
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "./srv/deep/nest", Mode: 0o2750, Uid: 5, Gid: 6}, nil)

	write(&tar.Header{
		Typeflag: tar.TypeReg, Name: "./bin/su", Mode: 0o4755, Uid: 0, Gid: 0,
		Size: 4,
		PAXRecords: map[string]string{
			"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\x00\x00\x00\x00",
			"SCHILY.xattr.user.b":              "2",
			"SCHILY.xattr.user.a":              "1",
		},
	}, []byte("SUID"))

	write(&tar.Header{Typeflag: tar.TypeReg, Name: "bin/empty", Mode: 0o644, Size: 0}, nil)

	big := bytes.Repeat([]byte("0123456789abcdef"), 65536) // 1 MiB
	write(&tar.Header{
		Typeflag: tar.TypeReg, Name: "./big.bin", Mode: 0o600, Uid: 7, Gid: 8,
		Size: int64(len(big)),
	}, big)

	write(&tar.Header{Typeflag: tar.TypeSymlink, Name: "./bin/sh", Linkname: "su", Mode: 0o777}, nil)

	// Hardlink to a regular file, then to a symlink (both occur in OCI layers).
	write(&tar.Header{Typeflag: tar.TypeLink, Name: "./bin/su2", Linkname: "bin/su"}, nil)
	write(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/sh2", Linkname: "./bin/sh"}, nil)

	write(&tar.Header{Typeflag: tar.TypeChar, Name: "./dev-null", Mode: 0o666, Devmajor: 1, Devminor: 3}, nil)
	write(&tar.Header{Typeflag: tar.TypeBlock, Name: "./dev-sda", Mode: 0o660, Devmajor: 8, Devminor: 0}, nil)
	write(&tar.Header{Typeflag: tar.TypeFifo, Name: "./fifo", Mode: 0o600}, nil)

	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func buildCorpusImage(t *testing.T) []byte {
	t.Helper()

	out, err := buildGoEROFS(io.NopCloser(bytes.NewReader(corpusTar(t))))
	if err != nil {
		t.Fatal(err)
	}

	img, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}

	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	return img
}

func corpusStat(t *testing.T, img fs.FS, name string) *goerofs.Stat {
	t.Helper()

	linkFS, castOK := img.(interface {
		Lstat(name string) (fs.FileInfo, error)
	})
	if !castOK {
		t.Fatal("erofs FS lacks Lstat")
	}

	info, err := linkFS.Lstat(name)
	if err != nil {
		t.Fatalf("lstat %s: %v", name, err)
	}

	stat, ok := info.Sys().(*goerofs.Stat)
	if !ok {
		t.Fatalf("lstat %s: Sys() is %T", name, info.Sys())
	}

	return stat
}

// TestGoEROFSFidelity converts the corpus tar and reads the image back,
// checking every metadata dimension the container runtime depends on.
func TestGoEROFSFidelity(t *testing.T) {
	t.Parallel()

	img, err := goerofs.Open(bytes.NewReader(buildCorpusImage(t)))
	if err != nil {
		t.Fatal(err)
	}

	// Content.
	for name, want := range map[string]string{"bin/su": "SUID", "bin/su2": "SUID", "bin/empty": ""} {
		got, err := fs.ReadFile(img, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}

	if got, err := fs.ReadFile(img, "big.bin"); err != nil || len(got) != 1<<20 || got[17] != '1' {
		t.Errorf("big.bin: err %v, len %d", err, len(got))
	}

	// Modes incl. setuid/setgid/sticky, ownership, times.
	checks := []struct {
		name     string
		mode     uint32 // raw permission+special bits
		uid, gid uint32
	}{
		{"bin/su", 0o4755, 0, 0},
		{"tmp", 0o1777, 0, 0},
		{"srv/deep/nest", 0o2750, 5, 6},
		{"big.bin", 0o600, 7, 8},
	}
	for _, check := range checks {
		stat := corpusStat(t, img, check.name)
		if perm := goModeToRaw(stat.Mode); perm != check.mode {
			t.Errorf("%s: mode %o, want %o", check.name, perm, check.mode)
		}

		if stat.UID != check.uid || stat.GID != check.gid {
			t.Errorf("%s: owner %d:%d, want %d:%d", check.name, stat.UID, stat.GID, check.uid, check.gid)
		}

		if stat.Mtime != 1700000000 {
			t.Errorf("%s: mtime %d, want 1700000000", check.name, stat.Mtime)
		}
	}

	// Xattrs (multiple on one file, incl. security.capability).
	suStat := corpusStat(t, img, "bin/su")

	wantXattrs := map[string]string{
		"security.capability": "\x01\x00\x00\x02\x00\x00\x00\x00",
		"user.a":              "1",
		"user.b":              "2",
	}
	for k, v := range wantXattrs {
		if suStat.Xattrs[k] != v {
			t.Errorf("bin/su xattr %s = %q, want %q", k, suStat.Xattrs[k], v)
		}
	}

	// Hardlinks: shared ino, nlink 2 — file pair and symlink pair.
	for _, pair := range [][2]string{{"bin/su", "bin/su2"}, {"bin/sh", "bin/sh2"}} {
		statA, statB := corpusStat(t, img, pair[0]), corpusStat(t, img, pair[1])
		if statA.Ino != statB.Ino {
			t.Errorf("%s/%s: inos %d/%d, want shared", pair[0], pair[1], statA.Ino, statB.Ino)
		}

		if statA.Nlink != 2 || statB.Nlink != 2 {
			t.Errorf("%s/%s: nlink %d/%d, want 2/2", pair[0], pair[1], statA.Nlink, statB.Nlink)
		}
	}

	// Devices and fifo.
	if stat := corpusStat(t, img, "dev-null"); stat.Rdev != 1<<8|3 {
		t.Errorf("dev-null rdev = %#x, want %#x", stat.Rdev, 1<<8|3)
	}

	if stat := corpusStat(t, img, "dev-sda"); stat.Rdev != 8<<8 {
		t.Errorf("dev-sda rdev = %#x, want %#x", stat.Rdev, 8<<8)
	}

	if stat := corpusStat(t, img, "fifo"); stat.Mode&fs.ModeNamedPipe == 0 {
		t.Errorf("fifo mode = %v, want named pipe", stat.Mode)
	}
}

// TestGoEROFSDeterministic requires byte-identical images across builds —
// the healing/verify path of the content store depends on it.
func TestGoEROFSDeterministic(t *testing.T) {
	t.Parallel()

	if !bytes.Equal(buildCorpusImage(t), buildCorpusImage(t)) {
		t.Fatal("two builds of the same tar produced different bytes")
	}
}

// TestGoEROFSBrokenHardlink: a link whose target never appeared must fail
// loudly, not produce a silent hole (flatten guarantees ordering; this
// guards the invariant).
func TestGoEROFSBrokenHardlink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	tarWriter := tar.NewWriter(&buf)
	if err := tarWriter.WriteHeader(&tar.Header{
		Typeflag: tar.TypeLink, Name: "./orphan", Linkname: "never-written",
	}); err != nil {
		t.Fatal(err)
	}

	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := buildGoEROFS(io.NopCloser(&buf))
	if err == nil || !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("orphan hardlink: err = %v, want failure naming the entry", err)
	}
}

// goModeToRaw folds an fs.FileMode's permission + setuid/setgid/sticky bits
// back into raw form for comparison against tar modes.
func goModeToRaw(mode fs.FileMode) uint32 {
	raw := uint32(mode.Perm())

	if mode&fs.ModeSetuid != 0 {
		raw |= 0o4000
	}

	if mode&fs.ModeSetgid != 0 {
		raw |= 0o2000
	}

	if mode&fs.ModeSticky != 0 {
		raw |= 0o1000
	}

	return raw
}
