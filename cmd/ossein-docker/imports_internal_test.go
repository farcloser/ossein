//go:build darwin && arm64

package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/farcloser/ossein/pkg/image"
)

// writeLayout writes an OCI layout shaped like buildkit's exports: plain
// image manifests for a single-platform build, or an index carrying platform
// manifests next to an attestation manifest.
func writeLayout(t *testing.T, idx v1.ImageIndex) string {
	t.Helper()

	dir := t.TempDir()

	if _, err := layout.Write(dir, idx); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestSelectImageSkipsAttestationsAndMatchesPlatform(t *testing.T) {
	t.Parallel()

	arm, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}

	amd, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}

	attestation, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: attestation, Descriptor: v1.Descriptor{
			MediaType:   types.OCIManifestSchema1,
			Annotations: map[string]string{attestationAnnotation: "attestation-manifest"},
			Platform:    &v1.Platform{OS: "unknown", Architecture: "unknown"},
		}},
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{
			MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
		}},
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{
			MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "arm64"},
		}},
	)

	dir := writeLayout(t, idx)

	for platform, want := range map[string]v1.Image{"": arm, "linux/arm64": arm, "linux/amd64": amd} {
		got, err := imageFromLayout(dir, platform)
		if err != nil {
			t.Fatalf("imageFromLayout(%q): %v", platform, err)
		}

		gotDigest, _ := got.Digest()
		wantDigest, _ := want.Digest()

		if gotDigest != wantDigest {
			t.Fatalf("imageFromLayout(%q) = %s, want %s", platform, gotDigest, wantDigest)
		}
	}

	if _, err := imageFromLayout(dir, "linux/riscv64"); !errors.Is(err, errNoImage) {
		t.Fatalf("absent platform = %v, want errNoImage", err)
	}
}

func TestImportTagsMakesTheTagResolvable(t *testing.T) {
	// Import writes ossein's resolve cache and image cache under $HOME.
	t.Setenv("HOME", t.TempDir())

	built, err := random.Image(512, 2)
	if err != nil {
		t.Fatal(err)
	}

	// A single-platform export: one platform-less manifest in the index.
	dir := writeLayout(t, mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: built, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1}}))

	tags := []string{"index.docker.io/library/app:dev", "ghcr.io/org/app:v1"}

	if err := importTags(slog.Default(), dir, tags, ""); err != nil {
		t.Fatalf("importTags: %v", err)
	}

	wantDigest, _ := built.Digest()

	cache, err := image.NewCache()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = cache.Close() }()

	// The short spelling a script uses must resolve OFFLINE to the build.
	for _, ref := range []string{"app:dev", "ghcr.io/org/app:v1"} {
		resolved, err := image.Resolve(context.Background(), cache, ref, "", image.PullNever)
		if err != nil {
			t.Fatalf("Resolve(%s) after importTags: %v", ref, err)
		}

		if resolved.Digest != wantDigest.Hex {
			t.Fatalf("Resolve(%s) = %s, want %s", ref, resolved.Digest, wantDigest.Hex)
		}

		// Warm: the rootfs is already flattened, so this must not need the
		// (now deleted) layout dir.
		pin, err := resolved.RootfsFile()
		if err != nil {
			t.Fatalf("RootfsFile(%s): %v", ref, err)
		}

		_ = pin.Release()
	}
}
