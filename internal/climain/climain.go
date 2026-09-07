// Package climain holds what every ossein command-line binary shares: the
// process logger setup and the exit-status convention. It exists so that
// ossein and its docker-shaped front (cmd/ossein-docker) behave as one
// product — same log format, same level names, same exit codes — without
// either importing the other's main package.
package climain

import (
	"context"
	"log/slog"

	"github.com/mycophonic/primordium/app/logger"
)

// ExitInternal is docker's runtime-error exit convention (125): a failure of
// the tool itself must be distinguishable from a workload that exited 1.
const ExitInternal = 125

// LogLevels is the accepted --log-level vocabulary, for kong's enum tag. Keep
// it in sync with NewLogger's switch: the enum is what admits a name, the
// switch is what it means.
const LogLevels = "debug,info,warn,error"

// NewLogger installs the process slog logger via primordium: a human-friendly
// tint handler when stderr is a TTY, structured JSON otherwise. Diagnostics go
// to stderr; stdout is reserved for machine-readable contracts (BUILDKIT_HOST,
// digests, version). level is a --log-level value already validated by the
// kong enum (LogLevels). It returns the installed default logger so the caller
// can inject it into command Run methods.
func NewLogger(level string) *slog.Logger {
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
