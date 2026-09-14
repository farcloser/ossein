//nolint:testpackage // white-box: exercises the unexported cache/Image wiring
package image

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	goerofs "github.com/forkcloser/erofs"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
	blobcache "github.com/mycophonic/primordium/store/cache"

	"github.com/farcloser/ossein/internal/rootfsblob"
)

// newTestImage wires an Image around a synthetic (offline) source image so the
// cache path can be exercised without a registry.
func newTestImage(t *testing.T, cache Cache, source v1.Image, identifier string) *Image {
	t.Helper()

	digest, err := source.Digest()
	if err != nil {
		t.Fatal(err)
	}

	return &Image{
		Ref:        "test/img:latest",
		Digest:     digest.Hex,
		cache:      cache,
		source:     source,
		identifier: identifier,
	}
}

// readPin reads the pinned blob file fully and releases the pin.
func readPin(t *testing.T, pin *blobcache.PinnedFile) []byte {
	t.Helper()

	defer func() { _ = pin.Release() }()

	data, err := os.ReadFile(pin.Path)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestCacheRootfsFlattensAndCaches(t *testing.T) {
	t.Parallel()

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()

	source, err := random.Image(1024, 2) // 1KiB across 2 layers
	if err != nil {
		t.Fatal(err)
	}

	img := newTestImage(t, cache, source, "test/img:latest@sha256:deadbeef|linux/arm64")

	// First RootfsFile: cache miss → flatten. Second: cache hit. Both must
	// yield the identical blob.
	first := readPin(t, mustRootfsFile(t, img))
	second := readPin(t, mustRootfsFile(t, img))

	if !bytes.Equal(first, second) {
		t.Fatalf("cache hit differs from miss: %d vs %d bytes", len(first), len(second))
	}

	if len(first) == 0 {
		t.Fatal("empty rootfs")
	}

	// The blob doubles as a raw virtio-blk image: Virtualization.framework
	// refuses anything off sector alignment.
	if len(first)%512 != 0 {
		t.Fatalf("blob size %d is not 512-aligned", len(first))
	}

	// The default codec is goerofs. Classify the bytes with the SAME function
	// the guest dispatches on (internal/rootfsblob, imported by both worlds):
	// this is the host↔guest round-trip, not a restatement of the magic. A
	// writer change that produced something the guest would extract instead of
	// mount fails here rather than at boot.
	if got := rootfsblob.Sniff(first); got != rootfsblob.FormatEROFS {
		t.Fatalf("the guest would classify the default-codec blob as %v, want erofs", got)
	}

	// And it must open as a walkable filesystem via the same fork that wrote it.
	img2, err := goerofs.Open(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("cached rootfs does not open as EROFS: %v", err)
	}

	entries := 0

	if err := fs.WalkDir(img2, ".", func(string, fs.DirEntry, error) error {
		entries++

		return nil
	}); err != nil {
		t.Fatalf("walking cached EROFS rootfs: %v", err)
	}

	if entries == 0 {
		t.Fatal("cached EROFS rootfs is empty")
	}
}

func TestCacheGarbageCollect(t *testing.T) {
	t.Parallel()

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()

	source, err := random.Image(2048, 1)
	if err != nil {
		t.Fatal(err)
	}

	img := newTestImage(t, cache, source, "gc/img@sha256:cafe|linux/arm64")
	_ = readPin(t, mustRootfsFile(t, img))

	// Under the default 50GB quota nothing is over-quota, so GC frees zero and
	// must not error — the point is the call path works end to end.
	if _, err := cache.GarbageCollect(); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
}

func mustRootfsFile(t *testing.T, img *Image) *blobcache.PinnedFile {
	t.Helper()

	pin, err := img.RootfsFile()
	if err != nil {
		t.Fatal(err)
	}

	return pin
}

func TestResolutionRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "res.json")
	cfg := v1.Config{Entrypoint: []string{"/bin/sh"}, Env: []string{"FOO=bar"}}

	storeResolution(path, resolution{Digest: "sha256:deadbeef", Config: cfg})

	res, err := loadResolution(path)
	if err != nil {
		t.Fatal(err)
	}

	if res == nil {
		t.Fatal("stored resolution reads as a miss")
	}

	if res.Digest != "sha256:deadbeef" {
		t.Fatalf("digest = %q, want sha256:deadbeef", res.Digest)
	}

	if !reflect.DeepEqual(res.Config.Entrypoint, cfg.Entrypoint) || !reflect.DeepEqual(res.Config.Env, cfg.Env) {
		t.Fatalf("config round-trip mismatch: %+v", res.Config)
	}

	// Corrupt entry → clean miss (nil, nil), never an error.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = loadResolution(path)
	if err != nil {
		t.Fatalf("corrupt resolution must not error: %v", err)
	}

	if res != nil {
		t.Fatalf("corrupt resolution must read as a miss, got %+v", res)
	}

	// Absent file → clean miss too.
	res, err = loadResolution(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || res != nil {
		t.Fatalf("absent resolution = (%+v, %v), want (nil, nil)", res, err)
	}
}

func TestResolvePullNeverUncachedFails(t *testing.T) {
	// Redirect $HOME so the resolve cache (under dirs.CacheDir) is empty and the
	// test never touches the user's real cache. t.Setenv forbids t.Parallel.
	t.Setenv("HOME", t.TempDir())

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	_, err = Resolve(context.Background(), cache, "example.com/never/cached:latest", "", PullNever)
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("Resolve(PullNever, uncached) = %v, want ErrResolve", err)
	}
}

// pinnedFixture is a random image, an index that lists it for the host
// platform, and the chained record online() would have written for it.
type pinnedFixture struct {
	img       v1.Image
	imgDigest string
	idxDigest string
	record    resolution
}

func newPinnedFixture(t *testing.T) pinnedFixture {
	t.Helper()

	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Listed for the platform the tests resolve — the host's, whichever
	// runner this is — since that is what the index must vouch for.
	host := hostPlatform()

	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add: img,
		Descriptor: v1.Descriptor{
			MediaType: types.OCIManifestSchema1,
			Platform:  &host,
		},
	})

	rawIndex, err := idx.RawManifest()
	if err != nil {
		t.Fatal(err)
	}

	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatal(err)
	}

	rawConfig, err := img.RawConfigFile()
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}

	imgDigest, _ := img.Digest()
	idxDigest, _ := idx.Digest()

	return pinnedFixture{
		img: img, imgDigest: imgDigest.String(), idxDigest: idxDigest.String(),
		record: resolution{
			Digest: imgDigest.String(), Config: cfg.Config,
			Index: rawIndex, Manifest: rawManifest, ConfigBlob: rawConfig,
		},
	}
}

// writeRecord stores rec as the resolution for ref on the host platform.
func writeRecord(t *testing.T, ref string, rec resolution) {
	t.Helper()

	path, err := resolveCacheFile(ref, hostPlatform().String())
	if err != nil {
		t.Fatal(err)
	}

	storeResolution(path, rec)
}

func TestResolvePinnedRefVerifiesTheChain(t *testing.T) {
	// Records live under dirs.CacheDir: redirect $HOME. t.Setenv forbids t.Parallel.
	t.Setenv("HOME", t.TempDir())

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	fixture := newPinnedFixture(t)

	// Pinned to the index: verified through Index → Manifest → ConfigBlob.
	viaIndex := "example.com/pinned@" + fixture.idxDigest
	writeRecord(t, viaIndex, fixture.record)

	for _, pull := range []string{PullNever, PullMissing} {
		got, err := Resolve(context.Background(), cache, viaIndex, "", pull)
		if err != nil {
			t.Fatalf("Resolve(%s, pinned to index) = %v", pull, err)
		}

		if "sha256:"+got.Digest != fixture.imgDigest {
			t.Fatalf("Resolve(%s) digest = %s, want %s", pull, got.Digest, fixture.imgDigest)
		}
	}

	// Pinned to the manifest itself: no Index in the chain.
	direct := "example.com/pinned@" + fixture.imgDigest
	rec := fixture.record
	rec.Index = nil
	writeRecord(t, direct, rec)

	if _, err := Resolve(context.Background(), cache, direct, "", PullNever); err != nil {
		t.Fatalf("Resolve(pinned to manifest) = %v", err)
	}
}

func TestResolvePinnedRefRefusesAnAlteredRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	fixture := newPinnedFixture(t)
	other := newPinnedFixture(t)
	ref := "example.com/pinned@" + fixture.idxDigest

	for name, alter := range map[string]func(*resolution){
		// The digest field points elsewhere while the bytes still hash: caught
		// by Digest ≠ sha256(Manifest).
		"digest swapped": func(r *resolution) { r.Digest = other.imgDigest },
		// Whole chain swapped for another image's: sha256(Index) ≠ the ref.
		"index swapped": func(r *resolution) {
			r.Index, r.Manifest, r.ConfigBlob, r.Digest = other.record.Index, other.record.Manifest, other.record.ConfigBlob, other.imgDigest
		},
		// Manifest from another image under the right index: the index does not list it.
		"manifest swapped": func(r *resolution) { r.Manifest, r.Digest = other.record.Manifest, other.imgDigest },
		// The config (entrypoint, env, user — what the container runs with)
		// replaced: the manifest's config digest no longer matches.
		"config swapped": func(r *resolution) { r.ConfigBlob = other.record.ConfigBlob },
		"config edited": func(r *resolution) {
			r.ConfigBlob = append([]byte(`{"config":{"Entrypoint":["/evil"]},"x":`), r.ConfigBlob[1:]...)
		},
	} {
		rec := fixture.record
		alter(&rec)
		writeRecord(t, ref, rec)

		for _, pull := range []string{PullNever, PullMissing} {
			if _, err := Resolve(context.Background(), cache, ref, "", pull); !errors.Is(err, ErrResolve) {
				t.Errorf("%s, Resolve(%s) = %v, want ErrResolve", name, pull, err)
			}
		}
	}
}

func TestResolvePinnedRefWithoutChainNeedsARePull(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	fixture := newPinnedFixture(t)

	// A record from before the chain was kept: digest and config only.
	legacy := resolution{Digest: fixture.imgDigest, Config: fixture.record.Config}

	// Pinned ref: never refuses and says how to fix it (missing would go to
	// the registry, which a unit test cannot).
	pinned := "example.com/pinned@" + fixture.idxDigest
	writeRecord(t, pinned, legacy)

	_, err = Resolve(context.Background(), cache, pinned, "", PullNever)
	if !errors.Is(err, ErrResolve) || !strings.Contains(err.Error(), "--pull=always") {
		t.Fatalf("Resolve(never, unchained pinned record) = %v, want ErrResolve naming --pull=always", err)
	}

	// A tag is not pinned to anything: the same legacy record still serves it.
	tag := "example.com/pinned:dev"
	writeRecord(t, tag, legacy)

	got, err := Resolve(context.Background(), cache, tag, "", PullNever)
	if err != nil || "sha256:"+got.Digest != fixture.imgDigest {
		t.Fatalf("Resolve(never, tag with legacy record) = (%v, %v)", got, err)
	}
}

func TestResolveUnknownPullPolicyErrors(t *testing.T) {
	t.Parallel()

	// The policy is validated before anything else, so no cache (nil) and no
	// network are ever touched.
	_, err := Resolve(context.Background(), nil, "debian", "", "sometimes")
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("Resolve(pull=sometimes) = %v, want ErrResolve", err)
	}

	if !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("error must name the bad policy: %v", err)
	}
}

func TestCanonicalPlatform(t *testing.T) {
	t.Parallel()

	cases := []struct {
		requested string
		platform  string
		arch      string
		wantErr   bool
	}{
		{"", "linux/arm64", "arm64", false},
		{"arm64", "linux/arm64", "arm64", false},
		{"aarch64", "linux/arm64", "arm64", false},
		{"linux/arm64", "linux/arm64", "arm64", false},
		{"linux/arm64/v8", "linux/arm64", "arm64", false},
		{"amd64", "linux/amd64", "amd64", false},
		{"x86_64", "linux/amd64", "amd64", false},
		{"linux/amd64", "linux/amd64", "amd64", false},
		{"linux/x86_64", "linux/amd64", "amd64", false},
		{"windows/amd64", "", "", true},
		{"linux/riscv64", "", "", true},
		{"garbage", "", "", true},
	}

	for _, testCase := range cases {
		platform, arch, err := CanonicalPlatform(testCase.requested)
		if (err != nil) != testCase.wantErr {
			t.Fatalf("CanonicalPlatform(%q) err = %v, wantErr = %v", testCase.requested, err, testCase.wantErr)
		}

		if err != nil {
			if !errors.Is(err, ErrUnsupportedPlatform) {
				t.Fatalf("CanonicalPlatform(%q) err = %v, want ErrUnsupportedPlatform", testCase.requested, err)
			}

			continue
		}

		if platform != testCase.platform || arch != testCase.arch {
			t.Fatalf("CanonicalPlatform(%q) = (%q, %q), want (%q, %q)",
				testCase.requested, platform, arch, testCase.platform, testCase.arch)
		}
	}
}

func TestResolveCacheKeyNormalization(t *testing.T) {
	t.Parallel()

	short, err := resolveCacheFileName("debian", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}

	full, err := resolveCacheFileName("docker.io/library/debian:latest", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}

	if short != full {
		t.Fatalf("equivalent refs map to different cache files: %q vs %q", short, full)
	}

	// A different platform must be a different entry.
	other, err := resolveCacheFileName("debian", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}

	if other == short {
		t.Fatal("different platforms map to the same cache file")
	}

	// A different tag must be a different entry.
	tagged, err := resolveCacheFileName("debian:bookworm", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}

	if tagged == short {
		t.Fatal("different tags map to the same cache file")
	}

	if _, err := resolveCacheFileName("UPPER not a ref!!", "linux/arm64"); !errors.Is(err, ErrResolve) {
		t.Fatalf("invalid ref = %v, want ErrResolve", err)
	}
}

func TestImportResolvesOfflineAndFlattens(t *testing.T) {
	// Redirect $HOME: Import writes the resolve cache under dirs.CacheDir.
	// t.Setenv forbids t.Parallel.
	t.Setenv("HOME", t.TempDir())

	cache, err := openCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	source, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}

	wantDigest, err := source.Digest()
	if err != nil {
		t.Fatal(err)
	}

	const ref = "example.com/built/locally:dev"

	imported, err := Import(cache, ref, "", source)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if imported.Digest != wantDigest.Hex {
		t.Fatalf("Import digest = %s, want %s", imported.Digest, wantDigest.Hex)
	}

	// The imported image flattens straight from its in-memory source.
	blob := readPin(t, mustRootfsFile(t, imported))
	if len(blob) == 0 || len(blob)%512 != 0 {
		t.Fatalf("imported rootfs is %d bytes", len(blob))
	}

	// After Import the tag resolves OFFLINE (never touches a registry) under
	// both pull policies that consult the local record, and serves the same
	// cached blob.
	for _, pull := range []string{PullNever, PullMissing} {
		resolved, err := Resolve(context.Background(), cache, ref, "", pull)
		if err != nil {
			t.Fatalf("Resolve(%s) after Import: %v", pull, err)
		}

		if resolved.Digest != wantDigest.Hex {
			t.Fatalf("Resolve(%s) digest = %s, want %s", pull, resolved.Digest, wantDigest.Hex)
		}

		if again := readPin(t, mustRootfsFile(t, resolved)); !bytes.Equal(again, blob) {
			t.Fatalf("Resolve(%s) serves a different rootfs than Import produced", pull)
		}
	}

	// A resolved-from-record local image whose blob is gone cannot be re-fetched:
	// the error must say "rebuild", not try a registry.
	resolved, err := Resolve(context.Background(), cache, ref, "", PullMissing)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := resolved.resolveSource(); !errors.Is(err, ErrCache) || !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("local image re-fetch = %v, want ErrCache asking for a rebuild", err)
	}

	// Import refuses an unparsable ref before touching anything.
	if _, err := Import(cache, "UPPER not a ref!!", "", source); !errors.Is(err, ErrResolve) {
		t.Fatalf("Import(bad ref) = %v, want ErrResolve", err)
	}
}
