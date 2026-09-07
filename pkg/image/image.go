// Package image resolves OCI images and serves their flattened rootfs, backed
// by primordium's content-addressable store (concurrency-safe, GC'd).
//
// go-containerregistry pulls the manifest/config; the in-tree flatten package
// (a fixed fork of mutate.Extract) squashes the layers, resolving plain AND
// opaque whiteouts and repairing hardlinks whose target a higher layer
// deleted or overwrote; the flattened tar is converted into an uncompressed
// EROFS filesystem image, sector-padded, and cached by content hash under a
// per-(digest,codec,generation) identifier, so amd64/arm64 variants coexist.
// The cached blob doubles as a raw disk image: warm boots attach it to the
// microVM as a read-only virtio-blk device (RootfsFile pins it against GC for
// the VM's lifetime) and the guest MOUNTS it under a tmpfs-upper overlay
// rather than unpacking it; a cold pull stages the conversion to disk,
// commits it, then attaches.
//
// The store lives under dirs.CacheDir()/images/<version> (i.e.
// ~/Library/Caches/ossein/images/<version> on darwin) — the version segment
// lets a future layout change ship beside the old cache.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/mycophonic/primordium/digest"
	"github.com/mycophonic/primordium/fault"
	"github.com/mycophonic/primordium/filesystem/dirs"
	"github.com/mycophonic/primordium/store/cache"
	"github.com/mycophonic/primordium/store/content"

	"github.com/farcloser/ossein/pkg/image/flatten"
)

// Cache is the content-addressable blob cache ossein needs: identifier →
// cached blob with fetch-on-miss, plus GC and Close. primordium's
// *content.Store satisfies it, so NewCache returns one directly — no wrapper.
//
// The signatures deliberately use primordium types: this is a swap seam (the
// one place a different storage backend would plug in), not a full storage
// abstraction. Everything else in this package depends on Cache, not on
// *content.Store.
type Cache interface {
	// AcquireFile returns the blob for identifier as a complete, immutable,
	// GC-pinned file on disk, calling fetch to completion on a miss (staged
	// to disk, not RAM). ossein always passes a NIL digest — the rootfs hash
	// is only known after flattening. The pin must be Released once the VM
	// no longer uses the file.
	AcquireFile(identifier string, dgst digest.Digest, fetch content.FetchFunc) (*cache.PinnedFile, error)
	// Invalidate drops identifier's index entry, so the next AcquireFile
	// re-fetches and re-hashes instead of verifying against the recorded
	// digest. RootfsFile needs it to recover from a stale entry (a surviving
	// index entry whose blob was reclaimed).
	Invalidate(identifier string) error
	// GarbageCollect reclaims cached blobs over the quota.
	GarbageCollect() (cache.GCStats, error)
	// Close flushes and releases the cache.
	Close() error
}

// cacheDirPerm/cacheFilePerm are the modes for the resolve-cache dir tree and
// entries: private to the user, group-readable dirs like the content store.
const (
	cacheDirPerm  = 0o750
	cacheFilePerm = 0o644
)

// cacheLayoutVersion is the on-disk cache schema version. It is a path segment
// so a future layout or storage change ships as a new version (v2, …) beside
// the old one instead of colliding with it — old caches simply go cold.
const cacheLayoutVersion = "v1"

// rootfsCodec names the FORMAT the flattened rootfs is stored under, and is a
// segment of the cache identifier (see rootfsIdentifier). It is a constant
// rather than a choice: goerofs — an uncompressed EROFS filesystem image
// written by the pure-Go writer — is the only format ossein produces.
//
// It stays a named segment precisely BECAUSE it no longer varies. The cache
// holds blobs by identifier, so the segment is what keeps entries written by a
// different format from ever being served to a guest that would misread them:
// yesterday's "+lz4+" and "+erofs+" entries simply go cold and fall to GC
// instead of colliding. A future format change must edit this string in the
// same commit that changes the bytes.
//
// Keep it in lockstep with what the guest's materializer can dispatch on —
// one shared classifier, internal/rootfsblob, which both this package's tests
// and the guest run against the same bytes.
//
// What it bought (measured 2026-08-02; rootfs-materialization notes, a local engineering journal): the
// guest MOUNTS the blob read-only under a tmpfs-upper overlay instead of
// extracting it — rust boot 857ms → ~200ms, materialization ~100µs flat in
// image size, kernel-compile parity, image pages evictable, and the
// image-must-fit-in-RAM cap gone.
//
// The tar+lz4 codec that preceded it is gone rather than kept as an A/B knob.
// It was the guest's only reason to carry a tar extractor, and the escape
// hatch it nominally provided — a guest kernel without CONFIG_EROFS_FS —
// cannot occur: the kernel is embedded, pinned by tag and sha256,
// cosign-verified, and config-asserted against a golden. The benchmark numbers
// it served as a baseline for are recorded in the benchmarks journal (local), and the code
// is in git history.
const rootfsCodec = "goerofs"

// sectorSize is the virtio-blk sector granularity the stored blob is padded
// to; Virtualization.framework rejects raw disk images off this alignment.
const sectorSize = 512

// flattenGeneration versions the FLATTEN ALGORITHM in the cache identifier,
// for the same reason the codec rides there: the cache holds flatten output
// by content hash, so a change in what flatten produces must be a clean miss.
// Without it, warm caches keep serving the previous algorithm's bytes (gen 1
// predates opaque-whiteout and hardlink repair — exactly the bugs the flatten
// fork fixes), and the GC'd-blob verify path re-flattens to bytes that can
// never match the recorded digest (see healingReader). Bump on any change to
// pkg/image/flatten's output; stale entries just go cold and fall to GC.
//
//	gen 2 — in-tree flatten fork: opaque whiteouts honored, hardlinks
//	        repaired/ordered, names normalized (2026-07-31).
const flattenGeneration = "g2"

// NewCache opens the image cache under <cache-dir>/images/<cacheLayoutVersion>
// (the app name segment comes from dirs.SetAppName, called once in main).
func NewCache() (Cache, error) {
	root, err := dirs.CacheDir("images", cacheLayoutVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: locating cache dir: %w", ErrCache, err)
	}

	return openCache(root)
}

// openCache opens the content store at root (tests inject a temp dir).
// content.New creates root (and its subdirs) itself, so no MkdirAll here.
func openCache(root string) (Cache, error) {
	store, err := content.New(root, nil) // nil opts → default 50GB quota
	if err != nil {
		return nil, fmt.Errorf("%w: opening content store: %w", ErrCache, err)
	}

	return store, nil
}

// Image is a resolved image: manifest/config metadata plus a handle to fetch
// the flattened rootfs on demand.
type Image struct {
	Ref      string
	Digest   string // manifest digest hex
	Config   v1.Config
	Platform v1.Platform

	cache  Cache
	source v1.Image // registry image, set when resolved online; nil when resolved
	// from the local resolve cache — then resolveSource fetches it lazily, BY THE
	// PINNED DIGEST (never by tag), only if the rootfs blob turns out to be
	// absent (GC'd) and flatten must re-run. The closure captures the ctx
	// Resolve was given, so cancelling that ctx also cancels a lazy re-fetch
	// triggered later via Rootfs.
	resolveSource func() (v1.Image, error)
	identifier    string // cache key: manifest digest + codec
}

// Pull policies (Docker semantics). Default is PullMissing.
const (
	PullAlways  = "always"  // always re-resolve from the registry
	PullMissing = "missing" // resolve offline if seen before; else hit the registry
	PullNever   = "never"   // offline only; error if never resolved locally
)

// rootfsIdentifier is the content-cache key for a manifest digest. The codec
// and flatten-generation segments ride along so that any change in what the
// cache stores for a digest is a clean miss instead of a stale serve.
func rootfsIdentifier(dgst string) string {
	return dgst + "+" + rootfsCodec + "+" + flattenGeneration
}

func hostPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH} // arm64 on Apple Silicon
}

// CanonicalPlatform validates and canonicalizes a Docker-style platform string
// to "linux/amd64" or "linux/arm64" — the only two ossein supports — and
// returns the bare arch ("amd64"/"arm64"). Empty means the host (arm64).
// amd64 on our arm64 host is what triggers Rosetta downstream.
func CanonicalPlatform(requested string) (platform, arch string, err error) {
	switch requested {
	case "", "linux/arm64", "linux/arm64/v8", "arm64", "aarch64":
		return "linux/arm64", "arm64", nil
	case "linux/amd64", "amd64", "linux/x86_64", "x86_64":
		return "linux/amd64", "amd64", nil
	default:
		return "", "", fmt.Errorf("%w: %q (only linux/amd64, linux/arm64)", ErrUnsupportedPlatform, requested)
	}
}

// Resolve returns a bound *Image for ref at platform (empty = host), honoring the
// pull policy. It does NOT flatten the rootfs — that happens lazily via Rootfs.
// ctx bounds all registry traffic, including a lazy rootfs re-fetch triggered
// later by Rootfs (resolveSource captures this ctx).
//
//	PullAlways  — always hit the registry (remote.Get), then cache the resolution.
//	PullMissing — if ref@platform was resolved before, use the cached digest+config
//	              OFFLINE (no network); the rootfs blob is fetched lazily by Rootfs
//	              only if it turns out to be absent — by the pinned digest, so the
//	              cached resolution holds even if the tag has moved upstream (tags
//	              are only re-resolved on PullAlways). First-ever ref hits the
//	              registry. This is the default and skips the ~800 ms resolve on a
//	              warm run.
//	PullNever   — offline only: use the cached resolution; error if there is none.
//
// Any other pull value is an error. The content-cache key is the manifest digest
// + codec: equivalent refs (debian == docker.io/library/debian:latest) and both
// arches coexist; a moved tag yields a new digest and the stale blob falls to
// GC. The codec segment makes a codec change (gzip→zstd) a clean miss rather
// than serving undecodable bytes.
func Resolve(ctx context.Context, store Cache, ref, platformStr, pull string) (*Image, error) {
	switch pull {
	case PullAlways, PullMissing, PullNever:
	default:
		return nil, fmt.Errorf("%w: unknown pull policy %q (valid: %s, %s, %s)",
			ErrResolve, pull, PullAlways, PullMissing, PullNever)
	}

	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: parse ref %q: %w", ErrResolve, ref, err)
	}

	platform := hostPlatform()

	if platformStr != "" {
		p, err := v1.ParsePlatform(platformStr)
		if err != nil {
			return nil, fmt.Errorf("%w: parse platform %q: %w", ErrResolve, platformStr, err)
		}

		platform = *p
	}

	platKey := platform.String()

	// remoteImage does the actual network resolve, bounded by ctx (captured, so a
	// lazy re-fetch via resolveSource stays cancellable too). Auth from Docker's
	// credential store (a `docker login` lifts the anonymous rate limit; falls
	// back to anonymous, so always safe to pass).
	remoteImage := func() (v1.Image, error) {
		desc, err := remote.Get(parsed,
			remote.WithContext(ctx),
			remote.WithPlatform(platform),
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve %s: %w", ErrResolve, ref, err)
		}

		img, err := desc.Image()
		if err != nil {
			return nil, fmt.Errorf("%w: image for %s (platform %s): %w", ErrResolve, ref, platKey, err)
		}

		return img, nil
	}

	// online resolves via the registry now, records the resolution, and returns a
	// fully-bound Image (source set → flatten needs no further network).
	online := func() (*Image, error) {
		img, err := remoteImage()
		if err != nil {
			return nil, err
		}

		dgst, err := img.Digest()
		if err != nil {
			return nil, fmt.Errorf("%w: digest %s: %w", ErrResolve, ref, err)
		}

		cfg, err := img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("%w: config %s: %w", ErrResolve, ref, err)
		}

		// Record ref@platform → digest+config so a later missing/never run is
		// offline. Best-effort: a cache-write failure must not fail the run.
		if cachePath, cacheErr := resolveCacheFile(ref, platKey); cacheErr == nil {
			storeResolution(cachePath, dgst.String(), cfg.Config)
		}

		return &Image{
			Ref: ref, Digest: dgst.Hex, Config: cfg.Config, Platform: platform,
			cache: store, source: img, identifier: rootfsIdentifier(dgst.String()),
		}, nil
	}

	if pull == PullAlways {
		return online()
	}

	// missing / never: try the local resolve cache first (skips the registry).
	cachePath, err := resolveCacheFile(ref, platKey)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve cache for %s: %w", ErrResolve, ref, err)
	}

	res, err := loadResolution(cachePath)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve cache for %s: %w", ErrResolve, ref, err)
	}

	if res == nil {
		if pull == PullNever {
			return nil, fmt.Errorf("%w: %q not resolved locally (--pull=never)", ErrResolve, ref)
		}

		return online() // missing: never seen — resolve now
	}

	hash, err := v1.NewHash(res.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: cached digest %q for %s: %w", ErrResolve, res.Digest, ref, err)
	}

	img := &Image{
		Ref: ref, Digest: hash.Hex, Config: res.Config, Platform: platform,
		cache: store, source: nil, identifier: rootfsIdentifier(res.Digest),
	}

	// If the rootfs blob was GC'd, flatten needs the registry image again:
	// missing re-fetches BY THE PINNED DIGEST (never by tag — this run committed
	// to the cached resolution, and a tag re-resolve here would either poison
	// the digest-keyed cache or fail when the tag moved upstream; the tag is
	// only ever re-resolved when the user asks, via --pull=always); never
	// refuses (offline contract). The closure carries Resolve's ctx.
	if pull == PullNever {
		img.resolveSource = func() (v1.Image, error) {
			return nil, fmt.Errorf("%w: rootfs for %q not cached (--pull=never)", ErrCache, ref)
		}
	} else {
		img.resolveSource = pinnedFetcher(ctx, ref, parsed.Context().Digest(res.Digest))
	}

	return img, nil
}

// pinnedFetcher is the lazy source for an Image resolved from the local cache:
// it fetches the manifest by its recorded digest, so the bytes flatten sees are
// the ones this run committed to, whatever the tag points at by now. ctx is
// Resolve's, so cancelling Resolve's caller cancels a re-fetch triggered later
// via Rootfs.
func pinnedFetcher(ctx context.Context, ref string, pinned name.Digest) func() (v1.Image, error) {
	return func() (v1.Image, error) {
		desc, err := remote.Get(pinned,
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch %s (pinned manifest for %s; if the registry "+
				"no longer serves it, re-pull with --pull=always): %w",
				ErrResolve, pinned.String(), ref, err)
		}

		fetched, err := desc.Image()
		if err != nil {
			return nil, fmt.Errorf("%w: image for %s: %w", ErrResolve, pinned.String(), err)
		}

		return fetched, nil
	}
}

// resolution is the cached ref@platform → manifest identity that lets missing/never
// skip the registry round-trip. Config is stored too so the OCI process spec
// (entrypoint/env/cwd) is available offline.
type resolution struct {
	Digest string    `json:"digest"` // full digest string, e.g. "sha256:…"
	Config v1.Config `json:"config"`
}

// resolveCacheFile is the on-disk path for a ref@platform resolution — a sibling
// of the content store (not inside it) so the store's layout/GC never touch it.
func resolveCacheFile(ref, platform string) (string, error) {
	root, err := dirs.CacheDir("resolve", cacheLayoutVersion)
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}

	fileName, err := resolveCacheFileName(ref, platform)
	if err != nil {
		return "", err
	}

	return filepath.Join(root, fileName), nil
}

// resolveCacheFileName derives the cache file name for ref@platform. The key is
// the NORMALIZED ref (name.ParseReference(...).Name()), not the raw spelling,
// so equivalent refs — "debian" vs "docker.io/library/debian:latest" — share
// one entry instead of the second spelling missing (and failing under
// PullNever) despite being locally resolved.
func resolveCacheFileName(ref, platform string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("%w: parse ref %q: %w", ErrResolve, ref, err)
	}

	sum := sha256.Sum256([]byte(parsed.Name() + "\x00" + platform))

	return hex.EncodeToString(sum[:]) + ".json", nil
}

// loadResolution reads the resolution cached at path; a missing or corrupt
// entry is a clean miss (nil, nil), never an error.
func loadResolution(path string) (*resolution, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- ossein-owned cache path
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // "not cached" is a clean miss, not an error
	}

	if err != nil {
		return nil, fmt.Errorf("reading resolution cache: %w", err)
	}

	var r resolution
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil //nolint:nilnil,nilerr // corrupt entry → treat as a miss, re-resolve
	}

	return &r, nil
}

// storeResolution records a resolution at path. Best-effort: failures are
// swallowed — a warm-run optimization must never fail a run.
func storeResolution(path, dgst string, cfg v1.Config) {
	if err := os.MkdirAll(filepath.Dir(path), cacheDirPerm); err != nil {
		return
	}

	encoded, err := json.Marshal(resolution{Digest: dgst, Config: cfg})
	if err != nil {
		return
	}

	tmp := path + ".tmp"
	if os.WriteFile(tmp, encoded, cacheFilePerm) == nil {
		_ = os.Rename(tmp, path)
	}
}

// RootfsFile returns the flattened rootfs blob as a complete, immutable,
// GC-pinned file (flattening and caching on miss), sized to a 512-byte
// multiple so it can be attached to the microVM as a raw read-only
// virtio-blk disk. The caller must Release the pin once the VM no longer
// needs the file.
//
// Healing (previously healingReader): blob GC deletes cached blobs but never
// the index, so an identifier can survive with a digest whose blob is gone.
// The next acquire then re-flattens and verifies against the RECORDED
// digest, and any drift in how those bytes are produced (a codec bump, a
// flatten change) makes that verification fail forever — retries reproduce
// it, and --pull=always re-resolves the tag to the same identifier. Keyed on
// fault.ErrWriteFailure rather than a specific mismatch: every reason the
// store could not persist the blob leaves the same useless entry behind.
// Dropping it here turns a permanent failure into a one-boot one.
func (i *Image) RootfsFile() (*cache.PinnedFile, error) {
	pin, err := i.cache.AcquireFile(i.identifier, nil, i.flatten)
	if err == nil {
		return pin, nil
	}

	if !errors.Is(err, fault.ErrWriteFailure) {
		return nil, fmt.Errorf("%w: acquiring rootfs %s: %w", ErrCache, i.Ref, err)
	}

	if invErr := i.cache.Invalidate(i.identifier); invErr != nil {
		slog.Default().Warn("could not drop stale rootfs cache entry",
			"identifier", i.identifier, "err", invErr)

		return nil, fmt.Errorf("%w: cached rootfs is stale and could not be dropped "+
			"(clear the image cache to recover): %w", ErrCache, err)
	}

	slog.Default().Warn("dropped stale rootfs cache entry; the next run will refetch",
		"identifier", i.identifier)

	return nil, fmt.Errorf("%w: cached rootfs entry was stale (its blob was reclaimed and the "+
		"rebuilt bytes no longer match the recorded digest); it has been dropped, re-run to "+
		"refetch: %w", ErrCache, err)
}

// flatten is the content.FetchFunc: called only on cache miss. It squashes the
// layers and converts the result into an EROFS image; the store hashes and
// caches the bytes, and the cached blob IS the disk the microVM boots from
// (attached read-only via virtio-blk, mounted rather than unpacked).
//
// The blob is UNCOMPRESSED, which is deliberate and was measured. Compression
// only ever paid for itself while the guest had to stream the whole thing
// through a decoder on every boot; now it mounts, faults pages in on demand,
// and a compressed image would put decompression back on the read path of
// every file the workload touches — in exchange for host disk, the cheapest
// resource in this pipeline. The predecessor's codec reasoning (zstd → lz4,
// chosen on phase-isolated boot measurements once virtio-blk replaced vsock)
// is in git history and no longer applies to anything ossein does.
func (i *Image) flatten() (io.ReadCloser, error) {
	// Cache-resolved image (source nil) whose blob was absent (or GC'd): fetch the
	// registry image now. Acquire dedups, so this runs at most once.
	src := i.source
	if src == nil {
		if i.resolveSource == nil {
			return nil, fmt.Errorf("%w: no source to flatten %s", ErrCache, i.Ref)
		}

		fetched, err := i.resolveSource()
		if err != nil {
			return nil, err
		}

		// Identity backstop: resolveSource fetches by the pinned manifest digest
		// (and go-containerregistry verifies digest-addressed content), so a
		// mismatch here should be impossible. It stays because the cost of the
		// impossible happening is a poisoned cache — new rootfs stored under the
		// old identifier, served with the old config, persisting for every later
		// run. Never store bytes under an identifier they don't match.
		fetchedDigest, err := fetched.Digest()
		if err != nil {
			return nil, fmt.Errorf("%w: digest %s: %w", ErrResolve, i.Ref, err)
		}

		if fetchedDigest.Hex != i.Digest {
			return nil, fmt.Errorf(
				"%w: re-fetched manifest for %q has digest %s, expected sha256:%s; "+
					"refusing to cache mismatched bytes (re-pull with --pull=always to re-resolve)",
				ErrResolve, i.Ref, fetchedDigest, i.Digest,
			)
		}

		src = fetched
		i.source = fetched
	}

	slog.Default().Info("flattening image",
		"ref", i.Ref, "digest", i.Digest, "platform", i.Platform.String())

	tar := flatten.Extract(src)

	return buildGoEROFS(tar)
}

// removeOnClose deletes the staging dir once the store has consumed the file.
type removeOnClose struct {
	*os.File

	dir string
}

func (r *removeOnClose) Close() error {
	err := r.File.Close()
	_ = os.RemoveAll(r.dir)

	if err != nil {
		return fmt.Errorf("closing staged erofs image: %w", err)
	}

	return nil
}

// paddedReader zero-pads its source so the total stream length is a multiple
// of 512, which Virtualization.framework requires of a raw disk image.
//
// Bare zeros are safe for what ossein stores: an EROFS image declares its own
// size in its superblock, is built in 4KiB blocks, and so is already sector
// aligned in practice — the padding is usually empty, and anything past the
// filesystem's end lies outside it and is never read. It was NOT safe for the
// tar+lz4 codec this replaced, whose tail had to be a well-formed lz4
// skippable frame because pierrec/lz4 rejects trailing zeros as a bad magic
// number; that machinery went with the codec. Round-tripped in
// TestCacheRootfsFlattensAndCaches.
type paddedReader struct {
	src     io.ReadCloser
	tail    []byte
	count   int64
	srcDone bool
}

func newPaddedReader(src io.ReadCloser) *paddedReader {
	return &paddedReader{src: src}
}

func (p *paddedReader) Read(buf []byte) (int, error) {
	if !p.srcDone {
		read, err := p.src.Read(buf)
		p.count += int64(read)

		if err == nil || !errors.Is(err, io.EOF) {
			//nolint:wrapcheck // transparent reader: pass source errors through
			return read, err
		}

		p.srcDone = true
		p.tail = p.buildTail()

		return read, nil
	}

	if len(p.tail) == 0 {
		return 0, io.EOF
	}

	served := copy(buf, p.tail)
	p.tail = p.tail[served:]

	return served, nil
}

func (p *paddedReader) Close() error {
	//nolint:wrapcheck // transparent closer
	return p.src.Close()
}

// buildTail computes the sector-alignment tail once the payload length is
// known. An already-aligned stream gets no tail at all.
func (p *paddedReader) buildTail() []byte {
	padding := int((sectorSize - p.count%sectorSize) % sectorSize)
	if padding == 0 {
		return nil
	}

	return make([]byte, padding)
}
