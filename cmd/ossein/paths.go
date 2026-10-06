//go:build darwin && arm64

package main

import (
	"fmt"
	"path/filepath"

	"github.com/mycophonic/primordium/filesystem/pathcheck"
)

// hostPath makes a host path from the command line, one ossein writes,
// absolute, and refuses one no file can be created at by this platform's rules
// (primordium's pathcheck). Absolute first: pathcheck refuses "." and "..",
// which a relative path legitimately carries.
func hostPath(value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", value, err)
	}

	if err := pathcheck.Validate(abs); err != nil {
		return "", fmt.Errorf("%q: %w", value, err)
	}

	return abs, nil
}

// consoleLog is --console-log, declared once for the three commands that boot
// a VM and embedded in each (a named field, so Validate is not promoted onto
// the command and run twice).
type consoleLog struct {
	Path string `help:"guest console log file" name:"console-log"`
}

// Validate runs after parsing: the console log is a path ossein writes.
func (c *consoleLog) Validate() error {
	if c.Path == "" {
		return nil
	}

	abs, err := hostPath(c.Path)
	if err != nil {
		return fmt.Errorf("%w: --console-log: %w", errUsage, err)
	}

	c.Path = abs

	return nil
}
