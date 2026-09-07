//go:build darwin && arm64

// Command ossein runs Linux containers on macOS in per-container microVMs —
// daemonless, pure Go over Code-Hex/vz. The guest kernel comes from the pinned
// ossein-kernel release and the initfs is built in-repo (init/ module); both
// are embedded into this binary at build time (see pkg/guestartifacts).
//
// The CLI is defined with kong; logging is slog via primordium (tint on a TTY,
// JSON otherwise). stdout carries only machine-readable contracts (BUILDKIT_HOST,
// digests, version); everything else is logged to stderr.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/mycophonic/primordium/filesystem/dirs"

	"github.com/farcloser/ossein/internal/climain"
	"github.com/farcloser/ossein/pkg/container"
	"github.com/farcloser/ossein/pkg/guestartifacts"
	"github.com/farcloser/ossein/pkg/image"
)

// version and buildkitImage are stamped by the linker (`just build` passes
// -X main.<name>=…); the literals here are the fallback for a plain `go build`.
// buildkitImage MUST stay digest-pinned: the tag is decorative, the digest is
// what makes an unchanged ossein commit always run the same buildkit. The
// Justfile is the source of truth — bump buildkit_tag + buildkit_digest there
// (`just buildkit-digest <tag>` resolves the new digest) and mirror it here.
//
//nolint:gochecknoglobals // -ldflags -X targets must be package-level vars
var (
	version = "0.0.1-dev"

	buildkitImage = "docker.io/moby/buildkit:v0.32.0@" +
		"sha256:1f8167fcb0eca5b7126353d35299386945cbb8949cc516c592a49f80cfce4fa2"
)

// appName is the top-level segment for primordium's user dirs
// (~/Library/Caches/<appName>/…). Matches the Go module for cache continuity.
const appName = "ossein"

// CLI is the kong grammar. Kernel/Initfs are global (most subcommands need them).
// Both are EMBEDDED in the binary (`just build` bundles the verified kernel + the
// initfs) and extracted to cache on first use, so they default to the embedded
// artifacts; a flag or the OSSEIN_KERNEL/OSSEIN_INITFS env var overrides with an
// on-disk path (local dev, bench A/B).
type CLI struct {
	LogLevel string `default:"info"      enum:"${log_levels}"                                      env:"OSSEIN_LOG_LEVEL" help:"log verbosity" name:"log-level"`
	Kernel   string `env:"OSSEIN_KERNEL" help:"guest kernel path (default: the embedded kernel)"   name:"kernel"`
	Initfs   string `env:"OSSEIN_INITFS" help:"vminitd initfs.cpio (default: the embedded initfs)" name:"initfs"`

	Doctor   doctorCmd   `cmd:"" help:"boot a bare microVM, handshake vminitd, tear down"`
	Pull     pullCmd     `cmd:"" help:"pull and flatten an image into the local cache"`
	Run      runCmd      `cmd:"" help:"run a one-shot container"`
	Buildkit buildkitCmd `cmd:"" help:"run buildkitd in a microVM; prints BUILDKIT_HOST"`
	Stop     stopCmd     `cmd:"" help:"stop backgrounded buildkit instance(s)"`
	GC       gcCmd       `cmd:"" help:"remove stale instance state"                       name:"gc"`
	Version  versionCmd  `cmd:"" help:"print version"`
}

func main() {
	dirs.SetAppName(appName) // must precede any dirs.* lookup (image cache)

	var cli CLI

	kctx := kong.Parse(&cli,
		kong.Name(appName),
		kong.Description("Run Linux containers on macOS in per-container microVMs."),
		kong.UsageOnError(),
		// Struct tags are compile-time literals, so a linker-stamped default has
		// to arrive as a kong variable: `default:"${buildkit_image}"` on
		// buildkitCmd.Image interpolates this.
		kong.Vars{"buildkit_image": buildkitImage, "log_levels": climain.LogLevels},
	)

	logger := climain.NewLogger(cli.LogLevel)

	art := container.Artifacts{Kernel: cli.Kernel, Initfs: cli.Initfs}

	// Bind the logger, the resolved artifacts, and the level string (the detach
	// re-exec needs the level) for injection into command Run methods. Runtime
	// errors are logged through slog (kong already handled parse/usage errors
	// during Parse), so all output shares one format and destination.
	if err := kctx.Run(logger, &art, cli.LogLevel); err != nil {
		// The workload's own nonzero status: all cleanup has unwound by now;
		// pass the code through silently, exactly like the zero-status path.
		if exit, ok := errors.AsType[exitError](err); ok {
			os.Exit(exit.code)
		}

		logger.Error("command failed", "err", err)
		os.Exit(climain.ExitInternal)
	}
}

// requireArtifacts resolves and validates the kernel/initfs preconditions for the
// commands that boot a VM. With no explicit --kernel/--initfs (or OSSEIN_KERNEL/
// OSSEIN_INITFS), it extracts the artifacts embedded in this binary at build time
// to the per-user cache — no network, purely local.
func requireArtifacts(art *container.Artifacts) error {
	cacheRoot, err := dirs.CacheDir("guest")
	if err != nil {
		return fmt.Errorf("guest cache dir: %w", err)
	}

	if art.Kernel == "" {
		if art.Kernel, err = guestartifacts.Kernel(cacheRoot); err != nil {
			return fmt.Errorf("extract embedded kernel: %w", err)
		}
	}

	if art.Initfs == "" {
		if art.Initfs, err = guestartifacts.Initfs(cacheRoot); err != nil {
			return fmt.Errorf("extract embedded initfs: %w", err)
		}
	}

	for _, path := range []string{art.Kernel, art.Initfs} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("artifact %q: %w", path, err)
		}
	}

	return nil
}

// --- version ---

type versionCmd struct{}

func (*versionCmd) Run() error {
	fmt.Fprintf(os.Stdout, "ossein %s\n", version)

	return nil
}

// --- doctor ---

type doctorCmd struct {
	ConsoleLog string `help:"guest console log file" name:"console-log"`
}

func (c *doctorCmd) Run(logger *slog.Logger, art *container.Artifacts) error {
	if err := requireArtifacts(art); err != nil {
		return err
	}

	logger.Info("doctor: booting bare microVM (no image, no network)")

	if err := container.Doctor(context.Background(), *art, c.ConsoleLog); err != nil {
		return fmt.Errorf("doctor: %w", err)
	}

	logger.Info("doctor: PASS", "checks", "boot+vminitd+tmpfs+write+teardown")

	return nil
}

// --- pull ---

type pullCmd struct {
	Platform string `help:"linux/amd64 | linux/arm64 (default: host)"`
	Image    string `arg:""                                           help:"image reference"`
}

func (c *pullCmd) Run() error {
	// A pull is a long network operation; Ctrl-C must cancel it cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	canon, _, err := image.CanonicalPlatform(c.Platform)
	if err != nil {
		return err
	}

	cache, err := image.NewCache()
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	img, err := image.Resolve(ctx, cache, c.Image, canon, image.PullAlways)
	if err != nil {
		return err
	}

	if err := warm(img); err != nil { // materialize the rootfs into the cache
		return err
	}

	fmt.Fprintf(os.Stdout, "%s\n", img.Digest)

	return nil
}
