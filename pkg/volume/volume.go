//go:build darwin && arm64

// Package volume manages persistent per-project ext4 disk images that back a
// container's writable state (e.g. buildkit's /var/lib/buildkit) across the
// ephemeral VM's lifetime. By default images live centrally under
// DataDir()/buildkit/<hash>/ (see CentralDir); a caller may instead pass a
// project-local host directory to Ensure. Either way the image is created +
// formatted in pure Go (go-diskfs) on first use, reused thereafter.
//
// The VM's rootfs is tmpfs and dies with the VM; a Volume is how state a build
// wants to keep — the buildkit content store and cache metadata — survives to
// speed up the next build for the same project.
package volume

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/mycophonic/primordium/filesystem/dirs"
	"github.com/mycophonic/primordium/filesystem/flock"
)

const (
	buildkitSubdir = "buildkit" // DataDir()/buildkit holds all per-project volumes
	imageName      = "cache.img"
	lockName       = "lock"

	// sectorsPerBlock4k makes ext4 use 4096-byte blocks (8 * 512): the host page
	// size and the sane default for a cache holding many real files.
	sectorsPerBlock4k = 8
	ext4SectorSize    = 512
	volumeLabel       = "ossein-bk"
	dirPerm           = 0o750

	// statBlockSize is the fixed unit of stat(2)'s st_blocks field (always 512
	// bytes), used to compute a sparse image's actual on-disk usage.
	statBlockSize = 512

	lockFilePerm  = 0o600
	gitignorePerm = 0o644 // plain marker file — world-readable, unlike the lock

	// ext4MagicOffset is where the ext4 superblock magic lives: the superblock
	// starts at byte 1024 and s_magic sits 56 bytes in (offset 0x438).
	ext4MagicOffset = 1024 + 56
	// ext4Magic is s_magic's value, stored little-endian on disk (0x53 0xEF).
	ext4Magic = 0xEF53
)

// ErrInUse is returned when another process already holds the volume's lock —
// i.e. a VM is currently using this project's cache. A virtio-blk image mounted
// read-write by two VMs at once would corrupt, so we refuse rather than share.
var ErrInUse = errors.New("cache volume in use by another instance")

// ErrCorruptImage is returned when an existing cache image fails validation
// (wrong size, or no ext4 superblock magic). The file is deliberately left in
// place — it is the user's cache, and silently deleting it would destroy the
// evidence of whatever corrupted it. Remove the file (or its directory) to
// rebuild.
var ErrCorruptImage = errors.New("cache image failed validation")

// Volume is an acquired per-project cache image. Path is the host ext4 image to
// attach as a virtio-blk device; the held lock guarantees single-writer use.
// Close once the VM that mounted it has stopped.
type Volume struct {
	Path string
	lock *os.File
}

// key derives a stable, filesystem-safe hash from a project directory (or any
// bare cache name) for the central cache layout.
func key(source string) string {
	sum := sha256.Sum256([]byte(source))

	return hex.EncodeToString(sum[:8]) // 16 hex chars — collision-safe enough here
}

// buildkitRoot is the base directory holding every central cache volume:
// DataDir()/buildkit (persistent, not the GC'd cache dir).
func buildkitRoot() (string, error) {
	base, err := dirs.DataDir()
	if err != nil {
		return "", fmt.Errorf("locating data dir: %w", err)
	}

	return filepath.Join(base, buildkitSubdir), nil
}

// CentralDir returns the central cache directory for keySource (a project
// directory or a bare --cache name): DataDir()/buildkit/<hash>. The project-
// local alternative is any host path the caller passes straight to Ensure.
func CentralDir(keySource string) (string, error) {
	root, err := buildkitRoot()
	if err != nil {
		return "", err
	}

	return filepath.Join(root, key(keySource)), nil
}

// Ensure acquires the cache volume rooted at dir, creating and formatting a
// sparse ext4 image of sizeBytes on first use and reusing it thereafter (a
// pre-existing image is validated first — see ErrCorruptImage). It takes an
// exclusive lock; if another instance holds it, it returns ErrInUse.
// When gitignore is set (project-local caches), it drops a .gitignore so the
// image never lands in version control. The caller owns the returned Volume and
// must Close it once its VM has stopped.
// The gitignore bool is a deliberate mode switch, not hidden control flow:
// central caches must never write into the tree, project-local ones must.
//
//revive:disable-next-line:flag-parameter
func Ensure(dir string, sizeBytes int64, gitignore bool) (*Volume, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}

	if gitignore {
		if err := writeGitignore(dir); err != nil {
			return nil, err
		}
	}

	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	imagePath := filepath.Join(dir, imageName)
	if err := ensureImage(imagePath, sizeBytes); err != nil {
		_ = flock.Unlock(lock)

		return nil, err
	}

	return &Volume{Path: imagePath, lock: lock}, nil
}

// writeGitignore drops a .gitignore excluding the whole cache dir, so a
// project-local cache image is never committed. Idempotent.
func writeGitignore(dir string) error {
	const content = "# ossein build cache — machine-local, do not commit\n*\n"

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(content), gitignorePerm); err != nil {
		return fmt.Errorf("writing cache .gitignore: %w", err)
	}

	return nil
}

// acquireLock exclusively locks dir/lockName, creating the lock file first
// (primordium's unix flock locks an EXISTING file — it does not O_CREATE).
// Returns ErrInUse when another instance already holds the lock. If dir (or
// the lock file) vanishes mid-acquire — a concurrent PruneUnused won the race —
// it recreates the directory and retries once.
func acquireLock(dir string) (*os.File, error) {
	lock, err := tryLockOnce(dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return lock, err
	}

	if mkErr := os.MkdirAll(dir, dirPerm); mkErr != nil {
		return nil, fmt.Errorf("recreating cache dir: %w", mkErr)
	}

	return tryLockOnce(dir)
}

// tryLockOnce is one create-then-flock attempt; acquireLock wraps it with the
// vanished-directory retry.
func tryLockOnce(dir string) (*os.File, error) {
	lockPath := filepath.Join(dir, lockName)

	// #nosec G304 -- lockPath is a fixed ossein-owned path under DataDir()/buildkit
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDONLY, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("creating lock file: %w", err)
	}

	_ = lockFile.Close()

	lock, err := flock.TryLock(lockPath)
	if err != nil {
		if errors.Is(err, flock.ErrLockWouldBlock) {
			return nil, ErrInUse
		}

		return nil, fmt.Errorf("locking cache volume: %w", err)
	}

	return lock, nil
}

// InUse reports whether the cache volume at dir is currently locked by a
// running instance. It never blocks and leaves no lasting effect, so it is safe
// as a fail-fast pre-check before launching a build VM.
func InUse(dir string) (bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil // no volume yet → nothing holds it
	}

	lock, err := acquireLock(dir)
	if errors.Is(err, ErrInUse) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	_ = flock.Unlock(lock)

	return false, nil
}

// Close releases the exclusive lock. Safe on a nil Volume.
func (v *Volume) Close() error {
	if v == nil || v.lock == nil {
		return nil
	}

	if err := flock.Unlock(v.lock); err != nil {
		return fmt.Errorf("releasing cache lock: %w", err)
	}

	return nil
}

// PruneUnused deletes every cache volume not currently held by a running
// instance, returning the actual disk space reclaimed and the number removed.
// In-use volumes (their lock is held) are left untouched — they are the whole
// point of the cache, so pruning is opt-in and never touches a live one.
func PruneUnused() (freedBytes int64, removed int, err error) {
	root, err := buildkitRoot()
	if err != nil {
		return 0, 0, err
	}

	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}

	if err != nil {
		return 0, 0, fmt.Errorf("reading cache dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(root, entry.Name())

		lock, lerr := acquireLock(dir)
		if lerr != nil {
			continue // in use (ErrInUse) or unlockable — either way, leave it
		}

		size := diskUsage(filepath.Join(dir, imageName))

		// Delete while STILL holding the flock (unlinking a flocked file is fine
		// on unix): unlocking first would open a window where a concurrent Ensure
		// locks the old-inode lock file and returns a volume whose image this
		// RemoveAll then deletes out from under its VM.
		rerr := os.RemoveAll(dir)

		_ = flock.Unlock(lock)

		if rerr != nil {
			return freedBytes, removed, fmt.Errorf("removing %s: %w", dir, rerr)
		}

		freedBytes += size
		removed++
	}

	return freedBytes, removed, nil
}

// diskUsage returns the ACTUAL bytes a (sparse) file occupies on disk, not its
// logical size — cache images are sparse, so st_blocks is what was reclaimed.
func diskUsage(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}

	return st.Blocks * statBlockSize
}

// ensureImage creates and formats the ext4 image if it does not already exist;
// a pre-existing image is validated (size + ext4 magic) before reuse. It
// formats into a temp file, fsyncs it, and renames in (then fsyncs the parent
// directory), so an interrupted format — process crash or power loss — never
// leaves a half-written image that a later run would mistake for reusable.
func ensureImage(path string, sizeBytes int64) error {
	switch info, err := os.Stat(path); {
	case err == nil:
		return validateImage(path, info.Size(), sizeBytes)
	case !os.IsNotExist(err):
		return fmt.Errorf("stat cache image: %w", err)
	}

	tmp := path + ".tmp"
	_ = os.Remove(tmp)

	back, err := file.CreateFromPath(tmp, sizeBytes)
	if err != nil {
		return fmt.Errorf("creating cache image: %w", err)
	}

	if _, err := ext4.Create(back, sizeBytes, 0, ext4SectorSize, &ext4.Params{
		SectorsPerBlock: sectorsPerBlock4k,
		VolumeName:      volumeLabel,
	}); err != nil {
		_ = back.Close()
		_ = os.Remove(tmp)

		return fmt.Errorf("formatting ext4: %w", err)
	}

	if err := back.Close(); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("closing cache image: %w", err)
	}

	// go-diskfs's Close does not sync: fsync the tmp file before the rename
	// publishes it, so a power loss cannot expose a validly-named empty husk.
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("syncing cache image: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("finalizing cache image: %w", err)
	}

	// fsync the parent directory so the rename itself is durable.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing cache dir: %w", err)
	}

	return nil
}

// validateImage sanity-checks a pre-existing cache image before reuse: the
// logical size must match what the caller asked for, and the ext4 superblock
// magic must be present. A truncated or clobbered image otherwise surfaces
// only as an opaque mount error inside the guest. On mismatch it fails loudly
// with ErrCorruptImage, naming the path — it does NOT delete the user's cache.
func validateImage(path string, actualSize, expectedSize int64) error {
	if actualSize != expectedSize {
		return fmt.Errorf(
			"%w: %s is %d bytes, expected %d — remove the file (or its directory) to rebuild the cache",
			ErrCorruptImage, path, actualSize, expectedSize,
		)
	}

	handle, err := os.Open(path) // #nosec G304 -- ossein-owned cache path
	if err != nil {
		return fmt.Errorf("opening cache image for validation: %w", err)
	}

	defer func() { _ = handle.Close() }()

	var magic [2]byte
	if _, err := handle.ReadAt(magic[:], ext4MagicOffset); err != nil {
		return fmt.Errorf("%w: reading ext4 superblock of %s: %w", ErrCorruptImage, path, err)
	}

	if binary.LittleEndian.Uint16(magic[:]) != ext4Magic {
		return fmt.Errorf(
			"%w: %s has no ext4 superblock magic at offset %#x — "+
				"remove the file (or its directory) to rebuild the cache",
			ErrCorruptImage, path, int64(ext4MagicOffset),
		)
	}

	return nil
}

// syncFile fsyncs path (open-for-write, Sync, Close), making its blocks
// durable before a rename publishes them.
func syncFile(path string) error {
	handle, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- ossein-owned cache path
	if err != nil {
		return fmt.Errorf("opening %s for sync: %w", path, err)
	}

	if err := handle.Sync(); err != nil {
		_ = handle.Close()

		return fmt.Errorf("syncing %s: %w", path, err)
	}

	if err := handle.Close(); err != nil {
		return fmt.Errorf("closing %s after sync: %w", path, err)
	}

	return nil
}

// syncDir fsyncs a directory so a just-renamed entry survives power loss.
func syncDir(dir string) error {
	handle, err := os.Open(dir) // #nosec G304 -- ossein-owned cache path
	if err != nil {
		return fmt.Errorf("opening %s for sync: %w", dir, err)
	}

	if err := handle.Sync(); err != nil {
		_ = handle.Close()

		return fmt.Errorf("syncing %s: %w", dir, err)
	}

	if err := handle.Close(); err != nil {
		return fmt.Errorf("closing %s after sync: %w", dir, err)
	}

	return nil
}
