//go:build darwin && arm64

package main

import (
	"testing"

	"github.com/mycophonic/primordium/filesystem/dirs"
)

// The precondition main satisfies before anything else: a dirs lookup without
// the app name panics, and the tests below reach them through resolveCacheDir.
func TestMain(m *testing.M) {
	dirs.SetAppName(appName)

	m.Run()
}
