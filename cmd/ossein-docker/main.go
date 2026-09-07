//go:build darwin && arm64

// Command ossein-docker is a docker-shaped front for ossein: the handful of
// `docker` invocations farcloser projects actually issue — `build`, `run`,
// `pull`, `stop`, `version` — with the flags they actually pass, and nothing
// else. It is meant to sit on a hermetic PATH under the name `docker` so a
// script written against docker/podman runs unchanged on a mac without a
// daemon.
//
// `run`, `pull` and `stop` exec the sibling `ossein` binary (whose flags are
// already docker-shaped). `build` speaks to buildkitd directly through the
// buildkit client library: it starts (or reuses) ossein's per-project buildkit
// microVM via `ossein buildkit --detach`, solves the Dockerfile, and records
// every `-t` tag in ossein's image cache — so `docker run <tag>` right after
// `docker build -t <tag>` resolves locally, exactly as with a daemon.
//
// Anything outside that subset is refused loudly (an unknown flag or command
// is an error, never a silent no-op), because a shim that swallows what it
// does not implement is worse than no shim.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/filesystem/dirs"

	"github.com/farcloser/ossein/internal/climain"
)

// appName is ossein's own: the shim shares its directories (image cache,
// runtime state), it does not own any.
const appName = "ossein"

// version is stamped by the linker (`just build`: -X main.version=…).
//

var version = "0.0.1-dev"

// CLI is the kong grammar: docker's shape, ossein's semantics.
type CLI struct {
	LogLevel string `default:"info" enum:"${log_levels}" env:"OSSEIN_LOG_LEVEL" help:"log verbosity" name:"log-level" short:"l"`

	Build   buildCmd   `cmd:"" help:"build an image from a Dockerfile with ossein's buildkit"`
	Buildx  buildxCmd  `cmd:"" help:"docker buildx compatibility (only 'buildx build')"`
	Run     runCmd     `cmd:"" help:"run a one-shot container (ossein run)"`
	Pull    pullCmd    `cmd:"" help:"pull an image into ossein's cache (ossein pull)"`
	Stop    stopCmd    `cmd:"" help:"stop ossein's background buildkit instance(s) (ossein stop)"`
	Version versionCmd `cmd:"" help:"print versions"`
}

// buildxCmd exists because `docker buildx build` is what documentation copies;
// it is `build` under another name.
type buildxCmd struct {
	Build buildCmd `cmd:"" help:"same as 'build'"`
}

func main() {
	dirs.SetAppName(appName) // must precede any dirs.* lookup (image cache, state)

	var cli CLI

	kctx := kong.Parse(&cli,
		kong.Name("docker"),
		kong.Description("docker-shaped front for ossein (microVM containers on macOS)."),
		kong.UsageOnError(),
		kong.Vars{"log_levels": climain.LogLevels},
	)

	logger := climain.NewLogger(cli.LogLevel)

	if err := kctx.Run(logger, cli.LogLevel); err != nil {
		// A build that failed on its own terms (the Dockerfile, a RUN step)
		// exits 1 like docker; a failure of the tooling itself exits 125.
		if errors.Is(err, errBuildFailed) {
			logger.Error("build failed", "err", err)
			os.Exit(1)
		}

		logger.Error("command failed", "err", err)
		os.Exit(climain.ExitInternal)
	}
}

// --- version ---

type versionCmd struct{}

// Run prints the shim's version, then execs `ossein version` in its place so
// both lines come out of one command.
func (*versionCmd) Run(level string) error {
	fmt.Fprintf(os.Stdout, "ossein-docker %s\n", version)

	return execOssein(level, "version")
}
