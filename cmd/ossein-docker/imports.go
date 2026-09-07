//go:build darwin && arm64

package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"

	"github.com/farcloser/ossein/pkg/image"
)

// attestationAnnotation marks the attestation manifests buildkit adds next to
// an image in an index (`vnd.docker.reference.type=attestation-manifest`);
// they are not the image.
const attestationAnnotation = "vnd.docker.reference.type"

var errNoImage = errors.New("no image for the requested platform in the build output")

// importTags records the build result under every tag in ossein's image cache
// and flattens its rootfs now, so the first `run <tag>` boots warm. The
// layout dir is the OCI directory buildkit's exporter produced.
func importTags(logger *slog.Logger, layoutDir string, tags []string, platform string) error {
	built, err := imageFromLayout(layoutDir, platform)
	if err != nil {
		return err
	}

	cache, err := image.NewCache()
	if err != nil {
		return err
	}

	defer func() { _ = cache.Close() }()

	for _, tag := range tags {
		imported, err := image.Import(cache, tag, platform, built)
		if err != nil {
			return err
		}

		pin, err := imported.RootfsFile()
		if err != nil {
			return err
		}

		_ = pin.Release()

		logger.Info("tagged", "ref", tag, "digest", "sha256:"+imported.Digest, "platform", imported.Platform.String())
	}

	return nil
}

// imageFromLayout picks the built image out of the exported OCI layout: the
// single manifest of a one-platform build, or the platform's manifest inside
// an index (skipping attestation manifests). platform "" means the host.
func imageFromLayout(dir, platform string) (v1.Image, error) {
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("reading build output %s: %w", dir, err)
	}

	want, err := wantedPlatform(platform)
	if err != nil {
		return nil, err
	}

	return selectImage(idx, want)
}

// wantedPlatform parses platform ("" → the host, as ossein resolves it).
func wantedPlatform(platform string) (v1.Platform, error) {
	if platform == "" {
		return v1.Platform{OS: "linux", Architecture: hostArch}, nil
	}

	parsed, err := v1.ParsePlatform(platform)
	if err != nil {
		return v1.Platform{}, fmt.Errorf("platform %q: %w", platform, err)
	}

	return *parsed, nil
}

// selectImage walks an index (recursively: buildkit nests a per-platform
// index when it attaches attestations) for the first image manifest matching
// want on OS and architecture, ignoring attestation manifests. A manifest
// without a platform (a single-platform export) is taken as the build's.
func selectImage(idx v1.ImageIndex, want v1.Platform) (v1.Image, error) {
	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	for _, desc := range manifest.Manifests {
		if _, attestation := desc.Annotations[attestationAnnotation]; attestation {
			continue
		}

		if desc.MediaType.IsIndex() {
			child, err := idx.ImageIndex(desc.Digest)
			if err != nil {
				return nil, fmt.Errorf("reading nested index %s: %w", desc.Digest, err)
			}

			if img, err := selectImage(child, want); err == nil {
				return img, nil
			}

			continue
		}

		if !desc.MediaType.IsImage() {
			continue
		}

		if desc.Platform != nil &&
			(desc.Platform.OS != want.OS || desc.Platform.Architecture != want.Architecture) {
			continue
		}

		img, err := idx.Image(desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("reading manifest %s: %w", desc.Digest, err)
		}

		return img, nil
	}

	return nil, fmt.Errorf("%w (%s/%s)", errNoImage, want.OS, want.Architecture)
}

// hostArch is the architecture a platform-less build targets: ossein runs on
// Apple silicon only (see the build constraint), so this is arm64 — and it is
// spelled as a constant rather than runtime.GOARCH so the resolution key the
// import writes cannot drift from the one `ossein run` looks up.
const hostArch = "arm64"
