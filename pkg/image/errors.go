package image

import "errors"

var (
	// ErrUnsupportedPlatform indicates a platform ossein cannot run
	// (anything other than linux/amd64 or linux/arm64).
	ErrUnsupportedPlatform = errors.New("unsupported platform")

	// ErrCache indicates a failure in the local content-addressed image cache
	// (locating, creating, or writing under the cache directory).
	ErrCache = errors.New("image cache failure")

	// ErrResolve indicates the image could not be pulled or resolved from its
	// registry.
	ErrResolve = errors.New("image resolution failure")
)
