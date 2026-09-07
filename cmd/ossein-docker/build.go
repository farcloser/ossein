//go:build darwin && arm64

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"

	"github.com/farcloser/ossein/pkg/image"
)

// The dockerfile frontend's vocabulary: its local mount names and the
// attribute keys `docker build` flags become (buildctl spells them the same).
const (
	frontendDockerfile = "dockerfile.v0"

	localContext    = "context"
	localDockerfile = "dockerfile"

	attrFilename    = "filename"
	attrTarget      = "target"
	attrPlatform    = "platform"
	attrNoCache     = "no-cache"
	attrResolveMode = "image-resolve-mode"
	attrBuildArg    = "build-arg:"

	resolveModePull = "pull"

	// Exporter attributes: an OCI layout DIRECTORY (not a tar) named after the
	// tags, which the client materializes as index.json + blobs/.
	exportTar   = "tar"
	exportName  = "name"
	exportFalse = "false"

	defaultDockerfile = "Dockerfile"
)

// buildCmd is `docker build` narrowed to what farcloser projects pass. The
// result is not an archive or a push: with -t, it is an entry in ossein's
// image cache, resolvable by `docker run <tag>` (and `ossein run <tag>`)
// without a registry.
type buildCmd struct {
	// sep:"none" on the repeatable value flags: kong would otherwise split
	// values on commas, and a build arg is free to contain one.
	Tags      []string `help:"name[:tag] to record the result under in ossein's image cache (repeatable)"    name:"tag"                     sep:"none"                                         short:"t"`
	File      string   `help:"Dockerfile path (default: <context>/Dockerfile)"                               name:"file"                    short:"f"`
	BuildArgs []string `help:"KEY=VALUE, or bare KEY to pass through from the host (repeatable)"             name:"build-arg"               sep:"none"`
	Platform  string   `help:"linux/amd64 | linux/arm64 (default: host)"`
	Target    string   `help:"stage to build"`
	NoCache   bool     `help:"ignore buildkit's layer cache"                                                 name:"no-cache"`
	Pull      bool     `help:"always re-resolve base image tags"`
	Progress  string   `default:"auto"                                                                       enum:"auto,plain,tty,quiet"    help:"progress output: auto | plain | tty | quiet"`
	Quiet     bool     `help:"no progress; print only the image digest on success"                           short:"q"`
	Load      bool     `help:"no-op: the result always lands in ossein's image cache (docker compatibility)"`
	Context   string   `arg:""                                                                               help:"build context directory"`
}

func (c *buildCmd) Run(logger *slog.Logger, level string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	contextDir, dockerfile, err := c.paths()
	if err != nil {
		return err
	}

	platform := ""

	if c.Platform != "" {
		if platform, _, err = image.CanonicalPlatform(c.Platform); err != nil {
			return err
		}
	}

	attrs, err := frontendAttrs(
		filepath.Base(dockerfile),
		platform,
		c.Target,
		c.NoCache,
		c.Pull,
		c.BuildArgs,
		os.LookupEnv,
	)
	if err != nil {
		return err
	}

	tags, err := normalizeTags(c.Tags)
	if err != nil {
		return err
	}

	host, err := ensureBuildkit(ctx, level)
	if err != nil {
		return err
	}

	builder, err := client.New(ctx, host)
	if err != nil {
		return fmt.Errorf("connecting to buildkit at %s: %w", host, err)
	}
	defer func() { _ = builder.Close() }()

	contextFS, err := fsutil.NewFS(contextDir)
	if err != nil {
		return fmt.Errorf("build context %s: %w", contextDir, err)
	}

	dockerfileFS, err := fsutil.NewFS(filepath.Dir(dockerfile))
	if err != nil {
		return fmt.Errorf("dockerfile dir %s: %w", filepath.Dir(dockerfile), err)
	}

	opt := client.SolveOpt{
		Frontend:      frontendDockerfile,
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{localContext: contextFS, localDockerfile: dockerfileFS},
		// Registry credentials for FROM lines come from docker's credential
		// store (`docker login`), the same one ossein's pulls consult.
		Session: []session.Attachable{keychainAuth{}},
	}

	var exportDir string

	if len(tags) > 0 {
		// The OCI layout is staged next to ossein's other scratch, imported
		// into the image cache, then dropped: the cache is the destination.
		exportDir, err = os.MkdirTemp("", "ossein-docker-build-*")
		if err != nil {
			return fmt.Errorf("staging dir: %w", err)
		}

		defer func() { _ = os.RemoveAll(exportDir) }()

		opt.Exports = []client.ExportEntry{{
			Type:      client.ExporterOCI,
			Attrs:     map[string]string{exportTar: exportFalse, exportName: strings.Join(tags, ",")},
			OutputDir: exportDir,
		}}
	}

	resp, err := solve(ctx, builder, opt, c.progressMode())
	if err != nil {
		return err
	}

	digest := resp.ExporterResponse["containerimage.digest"]

	if len(tags) == 0 {
		logger.Info("build succeeded; nothing tagged (no -t), result stays in buildkit's cache only",
			"digest", digest)

		return nil
	}

	if err := importTags(logger, exportDir, tags, platform); err != nil {
		return err
	}

	if c.Quiet {
		fmt.Fprintln(os.Stdout, digest)
	}

	return nil
}

// paths resolves the context directory and Dockerfile to absolute paths and
// checks both exist: a wrong path is our error to explain, not buildkit's.
func (c *buildCmd) paths() (contextDir, dockerfile string, err error) {
	contextDir, err = filepath.Abs(c.Context)
	if err != nil {
		return "", "", fmt.Errorf("build context %q: %w", c.Context, err)
	}

	info, err := os.Stat(contextDir)
	if err != nil {
		return "", "", fmt.Errorf("build context: %w", err)
	}

	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: build context %q is not a directory (only directory contexts)",
			errUnsupported, c.Context)
	}

	dockerfile = filepath.Join(contextDir, defaultDockerfile)

	if c.File != "" {
		if c.File == "-" {
			return "", "", fmt.Errorf("%w: a Dockerfile on stdin (-f -)", errUnsupported)
		}

		if dockerfile, err = filepath.Abs(c.File); err != nil {
			return "", "", fmt.Errorf("dockerfile %q: %w", c.File, err)
		}
	}

	if _, err := os.Stat(dockerfile); err != nil {
		return "", "", fmt.Errorf("dockerfile: %w", err)
	}

	return contextDir, dockerfile, nil
}

// progressMode maps --progress/--quiet onto buildkit's display modes.
func (c *buildCmd) progressMode() progressui.DisplayMode {
	if c.Quiet {
		return progressui.QuietMode
	}

	return progressui.DisplayMode(c.Progress)
}

// frontendAttrs assembles the dockerfile frontend's attributes. buildArgs are
// docker's `--build-arg` values: KEY=VALUE, or a bare KEY whose value comes
// from the host environment via lookup (unset → the arg is not passed, as
// docker does). noCache/pull legitimately ARE forwarded CLI flags.
//
//revive:disable-next-line:flag-parameter
func frontendAttrs(
	filename, platform, target string,
	noCache, pull bool,
	buildArgs []string,
	lookup func(string) (string, bool),
) (map[string]string, error) {
	attrs := map[string]string{attrFilename: filename}

	if platform != "" {
		attrs[attrPlatform] = platform
	}

	if target != "" {
		attrs[attrTarget] = target
	}

	if noCache {
		attrs[attrNoCache] = ""
	}

	if pull {
		attrs[attrResolveMode] = resolveModePull
	}

	for _, arg := range buildArgs {
		key, value, explicit := strings.Cut(arg, "=")
		if key == "" {
			return nil, fmt.Errorf("%w: --build-arg %q has no name", errUnsupported, arg)
		}

		if !explicit {
			fromEnv, ok := lookup(key)
			if !ok {
				continue
			}

			value = fromEnv
		}

		attrs[attrBuildArg+key] = value
	}

	return attrs, nil
}

// normalizeTags validates -t values and spells each one out in full
// (`foo` → `index.docker.io/library/foo:latest`) — the form under which
// ossein keys its resolutions — so the exporter, the cache record and a later
// `run foo` all agree on one name.
func normalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))

	for _, tag := range tags {
		ref, err := name.ParseReference(tag)
		if err != nil {
			return nil, fmt.Errorf("-t %q: %w", tag, err)
		}

		if _, isDigest := ref.(name.Digest); isDigest {
			return nil, fmt.Errorf("%w: -t %q names a digest, a build result is tagged by name", errUnsupported, tag)
		}

		out = append(out, ref.Name())
	}

	return out, nil
}

// solve runs the build with progress rendered to stderr. A solve error is a
// build failure (exit 1): the Dockerfile, a step, a missing base image.
func solve(
	ctx context.Context,
	builder *client.Client,
	opt client.SolveOpt,
	mode progressui.DisplayMode,
) (*client.SolveResponse, error) {
	display, err := progressui.NewDisplay(os.Stderr, mode)
	if err != nil {
		return nil, fmt.Errorf("progress display: %w", err)
	}

	status := make(chan *client.SolveStatus)
	rendered := make(chan error, 1)

	go func() {
		// The display drains status until Solve closes the channel. It is not
		// tied to ctx: on Ctrl-C, Solve is what gets cancelled, and closing
		// the channel is what ends the display — with the last lines shown.
		_, err := display.UpdateFrom(context.WithoutCancel(ctx), status)
		rendered <- err
	}()

	resp, solveErr := builder.Solve(ctx, nil, opt, status)

	// Solve has closed status; the display finishes on its own. Its error (a
	// broken terminal) must not mask the build's.
	if renderErr := <-rendered; renderErr != nil && solveErr == nil {
		return nil, fmt.Errorf("rendering progress: %w", renderErr)
	}

	if solveErr != nil {
		return nil, fmt.Errorf("%w: %w", errBuildFailed, solveErr)
	}

	return resp, nil
}
