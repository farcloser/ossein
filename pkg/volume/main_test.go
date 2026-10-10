//go:build darwin && arm64

package volume_test

import (
	"testing"

	"github.com/mycophonic/primordium/filesystem/dirs"
)

// CentralDir resolves DataDir, which panics without an app name: the
// precondition the ossein binaries satisfy in main, satisfied here for the
// test process.
func TestMain(m *testing.M) {
	dirs.SetAppName("ossein")

	m.Run()
}
