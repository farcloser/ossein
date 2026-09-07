//go:build darwin && arm64

package volume_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/farcloser/ossein/pkg/volume"
)

const (
	// testImageSize keeps the (sparse) test images small enough to format fast.
	// It must exceed 128 MiB: go-diskfs force-enables the ext4 resize inode and
	// refuses any single-block-group image (≤128 MiB at 4 KiB blocks) with "no
	// backup groups available" (see tools/build-initfs for the long story).
	testImageSize = 192 * 1024 * 1024 // 192 MiB

	// ext4MagicOffset / ext4Magic mirror the superblock constants under test:
	// s_magic sits at byte 1024+56, little-endian 0xEF53.
	ext4MagicOffset = 1024 + 56
	ext4Magic       = 0xEF53

	imageName = "cache.img"
)

func TestEnsureCreatesValidExt4Image(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	vol, err := volume.Ensure(dir, testImageSize, false)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = vol.Close() }()

	if vol.Path != filepath.Join(dir, imageName) {
		t.Fatalf("volume path = %q, want %q", vol.Path, filepath.Join(dir, imageName))
	}

	info, err := os.Stat(vol.Path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Size() != testImageSize {
		t.Fatalf("image size = %d, want %d", info.Size(), testImageSize)
	}

	handle, err := os.Open(vol.Path)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = handle.Close() }()

	var magic [2]byte
	if _, err := handle.ReadAt(magic[:], ext4MagicOffset); err != nil {
		t.Fatal(err)
	}

	if got := binary.LittleEndian.Uint16(magic[:]); got != ext4Magic {
		t.Fatalf("ext4 superblock magic = %#x, want %#x", got, ext4Magic)
	}
}

func TestEnsureRefusesConcurrentUse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := volume.Ensure(dir, testImageSize, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := volume.Ensure(dir, testImageSize, false); !errors.Is(err, volume.ErrInUse) {
		t.Fatalf("second Ensure while held = %v, want ErrInUse", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := volume.Ensure(dir, testImageSize, false)
	if err != nil {
		t.Fatalf("Ensure after Close: %v", err)
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPruneUnusedSkipsHeldRemovesIdle(t *testing.T) {
	// Redirect DataDir (derived from $HOME on darwin) into a temp dir so the
	// test never touches the user's real cache volumes. t.Setenv forbids
	// t.Parallel, which is exactly what we want here.
	t.Setenv("HOME", t.TempDir())

	heldDir, err := volume.CentralDir("held-project")
	if err != nil {
		t.Fatal(err)
	}

	idleDir, err := volume.CentralDir("idle-project")
	if err != nil {
		t.Fatal(err)
	}

	held, err := volume.Ensure(heldDir, testImageSize, false)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = held.Close() }()

	idle, err := volume.Ensure(idleDir, testImageSize, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}

	freed, removed, err := volume.PruneUnused()
	if err != nil {
		t.Fatal(err)
	}

	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	if freed <= 0 {
		t.Fatalf("freedBytes = %d, want > 0", freed)
	}

	if _, err := os.Stat(heldDir); err != nil {
		t.Fatalf("held (locked) volume dir was pruned: %v", err)
	}

	if _, err := os.Stat(idleDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle volume dir survived prune: stat err = %v", err)
	}
}

func TestEnsureRejectsCorruptImage(t *testing.T) {
	t.Parallel()

	corruptions := []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{
			name: "truncated",
			corrupt: func(t *testing.T, path string) {
				t.Helper()

				if err := os.Truncate(path, testImageSize/2); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "magic clobbered",
			corrupt: func(t *testing.T, path string) {
				t.Helper()

				handle, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}

				defer func() { _ = handle.Close() }()

				if _, err := handle.WriteAt([]byte{0, 0}, ext4MagicOffset); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, testCase := range corruptions {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			vol, err := volume.Ensure(dir, testImageSize, false)
			if err != nil {
				t.Fatal(err)
			}

			if err := vol.Close(); err != nil {
				t.Fatal(err)
			}

			imagePath := filepath.Join(dir, imageName)
			testCase.corrupt(t, imagePath)

			_, err = volume.Ensure(dir, testImageSize, false)
			if !errors.Is(err, volume.ErrCorruptImage) {
				t.Fatalf("Ensure on corrupt image = %v, want ErrCorruptImage", err)
			}

			// The corrupt file must be left in place for inspection, not deleted.
			if _, statErr := os.Stat(imagePath); statErr != nil {
				t.Fatalf("corrupt image was removed: %v", statErr)
			}
		})
	}
}
