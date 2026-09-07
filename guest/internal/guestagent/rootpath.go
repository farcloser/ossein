//go:build linux

package guestagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// rootPath resolves archive- and image-supplied paths against a root the way a
// chroot would, and is the ONLY containment boundary for those paths.
//
// The rule it enforces is the kernel's: RESOLVE_IN_ROOT makes openat2 treat the
// root fd as "/" for the whole walk, so an absolute symlink ("/dev") lands
// inside the root, and ".." at the root stays at the root. A lexical check
// cannot do this — it validates the string, while the kernel resolves the
// symlinks, and those two disagree exactly when an intermediate component is a
// symlink. That gap was a real escape: an image shipping "evil -> /run" plus a
// regular file "evil/x" wrote x outside the rootfs entirely.
//
// Symlinks are FOLLOWED, not refused. Refusing them would be a stricter-looking
// but wrong policy: usrmerge images ship "lib -> usr/lib" and a later entry
// "lib/libc.so" must legitimately resolve into usr/lib. Containment, not
// avoidance, is the property we need.
type rootPath struct {
	fd       int
	resolved string

	// dirCache memoizes parent-directory resolutions (relative dir →
	// resolved path). Tar entries cluster by directory, so without it every
	// entry re-pays the openat2+readlink+close of lookup for a parent that
	// was just resolved. Directories never move during an extraction; the
	// only way a cached resolution can go stale is the recovery path
	// replacing an existing node, and Forget covers exactly that.
	dirCache map[string]string
}

// openRoot pins root for the lifetime of an extraction. Callers must Close.
func openRoot(root string) (*rootPath, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", root, err)
	}

	// The resolved root is what every resolution is checked against below; the
	// caller's string may itself contain symlinks.
	resolved, err := os.Readlink(procFDPath(rootFD))
	if err != nil {
		_ = unix.Close(rootFD)

		return nil, fmt.Errorf("resolve root %s: %w", root, err)
	}

	return &rootPath{fd: rootFD, resolved: resolved, dirCache: map[string]string{}}, nil
}

func (r *rootPath) Close() error {
	//nolint:wrapcheck // a close error on an O_PATH fd carries no extra context
	return unix.Close(r.fd)
}

// Resolve maps an archive path to the resolved path to operate on, creating any
// missing parent directories inside the root. It returns "" for a name that
// denotes the root itself.
//
// Only the PARENT is resolved. The final component is deliberately left to the
// caller, which must not follow it: tar semantics replace whatever sits at that
// name (writeEntry creates exclusively and replaces on collision), and metadata
// uses the L* variants so a symlink's own attributes are set. Resolving the
// last component here would reintroduce exactly the redirection this type
// exists to stop.
func (r *rootPath) Resolve(name string) (string, error) {
	// Archive VALIDATION, not containment: the kernel would clamp a ".."
	// entry into the root and carry on, but a flattened OCI layer has no
	// legitimate reason to contain one, so treat it as a malformed or hostile
	// image and say so instead of silently writing somewhere else. Containment
	// is enforced below, by the kernel, and does not depend on this check.
	if escapesLexically(r.resolved, name) {
		return "", fmt.Errorf("%w: %q", errPathEscapes, name)
	}

	rel := strings.TrimPrefix(filepath.Clean("/"+name), "/")
	if rel == "" {
		return "", nil
	}

	dir, base := filepath.Split(rel)
	dir = strings.TrimSuffix(dir, "/")

	if cached, ok := r.dirCache[dir]; ok {
		return filepath.Join(cached, base), nil
	}

	dirReal, err := r.resolveDir(dir)
	if err != nil {
		return "", err
	}

	r.dirCache[dir] = dirReal

	return filepath.Join(dirReal, base), nil
}

// Forget drops cached directory resolutions at or under an archive name.
// The caller invokes it whenever extraction REPLACES an existing node (the
// collision-recovery path): the node may have been a directory, or a symlink
// some cached resolution walked through, so anything cached at or below that
// name is suspect. Fast-path creations need no invalidation — they only
// succeed where nothing existed, so nothing can have been resolved through
// the name. Collisions are close to nonexistent in practice, which is what
// keeps the sweep off the hot path.
func (r *rootPath) Forget(name string) {
	rel := strings.TrimPrefix(filepath.Clean(string(os.PathSeparator)+name), string(os.PathSeparator))
	if rel == "" {
		clear(r.dirCache)

		return
	}

	for key := range r.dirCache {
		if key == rel || strings.HasPrefix(key, rel+string(os.PathSeparator)) {
			delete(r.dirCache, key)
		}
	}
}

// resolveDir returns the resolved path of dir (relative to the root), creating
// missing components as it goes. Every lookup is re-rooted at r.fd rather than
// walked incrementally from the previous component: a symlink at depth N must be
// interpreted against the ROOT, not against its own parent, and only a full
// re-resolution from the root gets that right.
func (r *rootPath) resolveDir(dir string) (string, error) {
	if dir == "" {
		return r.resolved, nil
	}

	if resolved, err := r.lookup(dir); err == nil {
		return resolved, nil
	} else if !errors.Is(err, unix.ENOENT) {
		return "", err
	}

	// Missing: create the chain, shallowest first.
	parts := strings.Split(dir, string(os.PathSeparator))
	for idx := range parts {
		prefix := filepath.Join(parts[:idx+1]...)

		_, err := r.lookup(prefix)
		if err == nil {
			continue
		}

		if !errors.Is(err, unix.ENOENT) {
			return "", err
		}

		if err := r.mkdirIn(filepath.Join(parts[:idx]...), parts[idx]); err != nil {
			return "", err
		}
	}

	return r.lookup(dir)
}

// mkdirIn creates one component inside an already-resolved parent. Taking the
// parent as an fd is what keeps the create confined: mkdirat cannot traverse.
func (r *rootPath) mkdirIn(parent, name string) error {
	parentFD, err := r.open(parent)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parentFD) }()

	// 0o755 for implicitly created parents, as everywhere in the rootfs.
	if err := unix.Mkdirat(parentFD, name, stdDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("mkdir %s: %w", filepath.Join(parent, name), err)
	}

	return nil
}

// lookup returns the resolved path of an existing directory inside the root.
func (r *rootPath) lookup(rel string) (string, error) {
	dirFD, err := r.open(rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(dirFD) }()

	resolved, err := os.Readlink(procFDPath(dirFD))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", rel, err)
	}

	// Defence in depth: the kernel already guarantees containment, so a path
	// outside the root here means an assumption broke (a bind mount inside the
	// root, a root that moved). Fail loudly rather than write outside.
	if resolved != r.resolved && !strings.HasPrefix(resolved, r.resolved+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q resolved to %q", errPathEscapes, rel, resolved)
	}

	return resolved, nil
}

func (r *rootPath) open(rel string) (int, error) {
	if rel == "" || rel == "." {
		rel = "."
	}

	dirFD, err := unix.Openat2(r.fd, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT,
	})
	if err != nil {
		//nolint:wrapcheck // callers match on ENOENT; wrapping would hide it
		return -1, err
	}

	return dirFD, nil
}

func procFDPath(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}

// escapesLexically reports whether name climbs above root by ".." alone,
// ignoring symlinks entirely. It is deliberately NOT a containment check — it
// cannot be one, since it does not resolve symlinks, which is precisely the
// hole a lexical check left open here before. Its only job is to reject
// malformed archives loudly.
//
// An absolute name is not an escape: "/etc/passwd" is the ordinary way a tar
// spells a rooted path, and joining clamps it into root like any other entry.
func escapesLexically(root, name string) bool {
	joined := filepath.Join(root, name)

	return joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator))
}
