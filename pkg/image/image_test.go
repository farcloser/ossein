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
	"github.com/google/go-containerregistry/pkg/v1/random"
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

	storeResolution(path, "sha256:deadbeef", cfg)

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
