//go:build darwin && arm64

package main

import (
	"context"
	"log/slog"

	"github.com/mycophonic/primordium/app/logger"
)

// newLogger installs the process slog logger via primordium: a human-friendly
// tint handler when stderr is a TTY, structured JSON otherwise. Diagnostics go
// to stderr; stdout is reserved for machine-readable contracts (BUILDKIT_HOST,
// digests, version). level is the --log-level value already validated by the
// kong enum on CLI.LogLevel — that enum is the single source of truth for
// accepted names. It returns the installed default logger so the caller can
// inject it into command Run methods.
func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level

	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		// "info" — the kong enum admits nothing else, so this default is the
		// info case, not a fallback for unknown levels.
		slogLevel = slog.LevelInfo
	}

	logger.SetDefaultsForLogger(context.Background(), slogLevel)

	return slog.Default()
}
