// Package cli holds the conventions every ossein command-line binary
// follows — the process logger, the --log-level vocabulary, the exit-status
// convention, the spelling of "the whole host" on --cpus and --memory — so
// that ossein and its docker-shaped front (cmd/ossein-docker) behave as one
// product without either importing the other's main package.
package cli

import (
	"context"
	"log/slog"

	"github.com/mycophonic/primordium/app/logger"
)

// ExitInternal is docker's runtime-error exit convention (125): a failure of
// the tool itself must be distinguishable from a workload that exited 1.
const ExitInternal = 125

// WholeHost is the --cpus / --memory value that means "everything the host
// has": docker's own spelling of no limit, and what the docker-shaped front
// passes when a script sizes nothing. ossein resolves it against what
// Virtualization.framework allows (pkg/vm HostMaxCPUs, HostMaxMemoryMiB);
// the front never computes it, because only the side that links the
// framework knows the ceiling.
const WholeHost = 0

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
