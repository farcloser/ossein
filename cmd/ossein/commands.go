//go:build darwin && arm64

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mycophonic/primordium/filesystem/dirs"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/farcloser/ossein/pkg/container"
	"github.com/farcloser/ossein/pkg/image"
	"github.com/farcloser/ossein/pkg/volume"
)

const (
	// guestBkSock is where buildkitd listens inside the container; hostBkSock
	// is the per-instance socket filename on the host. The buildkit image pin
	// itself lives in the Justfile (buildkit_tag + buildkit_digest) and is linked
	// in as main.buildkitImage.
	guestBkSock = "/run/buildkit/buildkitd.sock"
	hostBkSock  = "buildkitd.sock"

	// stopPollInterval is how often `stop` checks whether a SIGTERM'd instance
	// has exited (the grace budget itself is the stop --grace flag).
	stopPollInterval = 250 * time.Millisecond

	// pidFileName is the per-instance pid file (inside InstanceDir); pidFileMode
	// keeps it owner-only. The record is "pid:starttime" — the start time pins
	// the process incarnation so a recycled pid is never signaled.
	pidFileName = "pid"
	pidFileMode = 0o600

	// buildkitDataDir is where buildkitd keeps its content store, cache metadata,
	// and snapshots; backing it with a per-project ext4 volume is what makes the
	// cache survive the ephemeral VM. buildkitCacheSize is the sparse image size.
	buildkitDataDir   = "/var/lib/buildkit"
	buildkitCacheSize = 20 << 30 // 20 GiB, sparse (grows as used)

	// bkReadyTimeout bounds how long detach waits for the backgrounded
	// buildkitd to answer on its socket before giving up and killing the child.
	bkReadyTimeout = 5 * time.Minute

	// exitInterrupted is the shell convention for death by SIGINT (128+2);
	// used when signal escalation abandons the guest.
	exitInterrupted = 130

	// decimal is strconv's base-10 argument; int64Bits sizes ParseInt for the
	// pid file's start-time field.
	decimal   = 10
	int64Bits = 64

	// logKeyID tags instance ids in log lines.
	logKeyID = "id"

	// hostDirMode is the mode for host directories ossein creates (volume
	// bind sources): owner-writable, group-readable.
	hostDirMode = 0o750
)

// --- run ---

type runCmd struct {
	CPUs        uint   `default:"2"                   help:"vCPUs"               name:"cpus"`
	Memory      uint64 `default:"4096"                help:"memory MiB"`
	Interactive bool   `help:"keep stdin open"        short:"i"`
	TTY         bool   `help:"allocate a pseudo-TTY"  name:"tty"                 short:"t"`
	Privileged  bool   `help:"grant all capabilities"`
	Network     bool   `default:"true"                help:"outbound networking" negatable:""`
	// Rm is a no-op: every run owns a throwaway microVM that is always torn down
	// on exit (see runCmd.Run's deferred Close). Accepted only so docker-shaped
	// scripts that pass --rm don't fail on an unknown flag.
	Rm         bool     `help:"no-op; the container is always removed on exit (docker compatibility)"                        name:"rm"`
	Cwd        string   `help:"working directory inside the container"                                                       name:"workdir"                              short:"w"`
	User       string   `help:"numeric uid[:gid]"`
	Env        []string `help:"set environment variables: KEY=VALUE, or bare KEY to pass through from the host (repeatable)" short:"e"`
	EnvFile    []string `help:"read environment variables from a file (docker --env-file, repeatable)"                       name:"env-file"`
	Platform   string   `help:"linux/amd64 | linux/arm64 (default: host; amd64 runs via Rosetta)"`
	Pull       string   `default:"missing"                                                                                   enum:"always,missing,never"                 help:"pull policy: always | missing | never (missing skips the registry when the image is already local)" name:"pull"`
	ConsoleLog string   `help:"guest console log file"                                                                       name:"console-log"`
	Volume     []string `help:"docker -v bind mount: host-dir:dest[:options] (repeatable)"                                   name:"volume"                               short:"v"`
	Image      string   `arg:""                                                                                              help:"image reference"`
	Command    []string `arg:""                                                                                              help:"command + args (overrides image CMD)" optional:""                                                                                               passthrough:""`
}

// parseVolume parses a docker-style `-v`/`--volume` spec but supports ONLY the
// directory bind-mount form — an existing (or auto-created) host DIRECTORY
// mapped to an absolute container path, optionally read-only. Every other
// docker volume form is rejected with a clear, actionable message:
//
//   - anonymous volume  `-v /data`        (no host source)
//   - named volume      `-v cache:/data`  (source has no path separator)
//   - single-file bind  `-v /host/f:/f`   (virtio-fs shares directories only)
//
// Matching docker `-v`, a missing host source is created as a directory.
func parseVolume(value string) (container.Mount, error) {
	spec, err := parseVolumeSpec(value)
	if err != nil {
		return container.Mount{}, err
	}

	abs, err := filepath.Abs(spec.source)
	if err != nil {
		return container.Mount{}, fmt.Errorf("resolving volume host %q: %w", spec.source, err)
	}

	info, err := os.Stat(abs)

	switch {
	case errors.Is(err, os.ErrNotExist):
		// docker -v creates a missing bind source as a directory.
		if mkErr := os.MkdirAll(abs, hostDirMode); mkErr != nil {
			return container.Mount{}, fmt.Errorf("creating volume host dir %q: %w", abs, mkErr)
		}
	case err != nil:
		return container.Mount{}, fmt.Errorf("volume host %q: %w", abs, err)
	case !info.IsDir():
		return container.Mount{}, fmt.Errorf(
			"%w: volume %q: %s is not a directory; only directory bind mounts are supported "+
				"(mount its parent directory instead)", errUsage, value, abs,
		)
	default:
		// exists and is a directory — ready to bind.
	}

	return container.Mount{Host: abs, Dest: spec.dest, ReadOnly: spec.readOnly}, nil
}

// volumeSpec is a parsed -v argument before the filesystem is consulted.
type volumeSpec struct {
	source, dest string
	readOnly     bool
}

// parseVolumeSpec is the pure half of parseVolume: the docker -v grammar
// (host-dir:dest[:options]) validated without touching the filesystem, so it
// can be fuzzed (FuzzParseVolumeSpec) — parseVolume itself creates a missing
// source directory, which no fuzz harness may do with arbitrary paths.
func parseVolumeSpec(value string) (volumeSpec, error) {
	fields := strings.Split(value, ":")

	// One field is an anonymous volume (`-v /data`): nothing to bind.
	if len(fields) < 2 {
		return volumeSpec{}, fmt.Errorf(
			"%w: volume %q: anonymous volumes are not supported; bind a host directory (-v /host/dir:%s)",
			errUsage, value, value,
		)
	}

	if len(fields) > 3 {
		return volumeSpec{}, fmt.Errorf(
			"%w: volume %q: too many ':'-separated fields; want host-dir:dest[:options]", errUsage, value,
		)
	}

	source, dest := fields[0], fields[1]

	if !strings.HasPrefix(dest, "/") {
		return volumeSpec{}, fmt.Errorf(
			"%w: volume %q: destination %q must be an absolute path", errUsage, value, dest,
		)
	}

	// A source with no path separator is a named volume (`-v cache:/x`), not a
	// host path.
	if !strings.ContainsRune(source, '/') && !strings.HasPrefix(source, ".") {
		return volumeSpec{}, fmt.Errorf(
			"%w: volume %q: named volumes are not supported; bind a host directory (-v /host/dir:%s)",
			errUsage, value, dest,
		)
	}

	readOnly, err := parseVolumeOptions(value, fields)
	if err != nil {
		return volumeSpec{}, err
	}

	return volumeSpec{source: source, dest: dest, readOnly: readOnly}, nil
}

// parseVolumeOptions reads the optional third field (comma-separated), honoring
// ro/readonly/rw and tolerating docker-compat no-ops; unknown options error.
func parseVolumeOptions(value string, fields []string) (bool, error) {
	readOnly := false

	if len(fields) != 3 {
		return readOnly, nil
	}

	for opt := range strings.SplitSeq(fields[2], ",") {
		switch opt {
		case "ro", "readonly":
			readOnly = true
		case "rw", "":
			readOnly = false
		case "z", "Z", "consistent", "cached", "delegated",
			"rprivate", "private", "rshared", "shared", "rslave", "slave":
			// docker-compat no-ops: SELinux relabeling (z/Z) is inert without
			// SELinux, the osxfs consistency hints are obsolete, and bind
			// propagation doesn't cross the VM boundary.
		default:
			return false, fmt.Errorf("%w: volume %q: unsupported option %q", errUsage, value, opt)
		}
	}

	return readOnly, nil
}

// resolveEnv expands docker-style -e/--env entries. "KEY=VALUE" (including an
// empty "KEY=") passes through verbatim; a bare "KEY" takes its value from the
// host environment and — matching docker — is dropped entirely when KEY is
// unset on the host. An empty name ("=VALUE" or "") is rejected, matching
// parseEnvFile. Ordering is preserved; ocispec dedups image vs user vars.
func resolveEnv(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(entries))

	for _, entry := range entries {
		key, _, hasValue := strings.Cut(entry, "=")
		if key == "" {
			return nil, fmt.Errorf("%w: env %q: no variable name", errUsage, entry)
		}

		if hasValue {
			out = append(out, entry)

			continue
		}

		if value, ok := os.LookupEnv(entry); ok {
			out = append(out, entry+"="+value)
		}
	}

	return out, nil
}

// parseEnvFile reads a docker --env-file: one entry per line. Blank lines and
// lines beginning with '#' are ignored; "KEY=VALUE" is taken literally (value
// may be empty or contain '='); a bare "KEY" inherits the host value and is
// dropped when unset (like -e). Empty or whitespace-bearing names are rejected,
// matching docker/cli.
func parseEnvFile(path string) ([]string, error) {
	file, err := os.Open(path) // #nosec G304 -- user-supplied --env-file path — reading it is the feature
	if err != nil {
		return nil, fmt.Errorf("env-file: %w", err)
	}
	defer func() { _ = file.Close() }()

	var out []string

	scanner := bufio.NewScanner(file)

	for line := 0; scanner.Scan(); {
		text := scanner.Text()
		line++

		if line == 1 {
			text = strings.TrimPrefix(text, string([]byte{0xEF, 0xBB, 0xBF})) // strip a leading UTF-8 BOM
		}

		if len(text) == 0 || text[0] == '#' {
			continue
		}

		key, _, hasValue := strings.Cut(text, "=")

		switch {
		case key == "":
			return nil, fmt.Errorf("%w: env-file %q line %d: no variable name", errUsage, path, line)
		case strings.ContainsAny(key, " \t"):
			return nil, fmt.Errorf(
				"%w: env-file %q line %d: variable %q contains whitespace",
				errUsage,
				path,
				line,
				key,
			)
		case hasValue:
			out = append(out, text)
		default:
			if value, ok := os.LookupEnv(key); ok {
				out = append(out, key+"="+value)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("env-file %q: %w", path, err)
	}

	return out, nil
}

func (c *runCmd) Run(art *container.Artifacts) (err error) {
	if err := requireArtifacts(art); err != nil {
		return err
	}

	mounts := make([]container.Mount, 0, len(c.Volume))

	for _, raw := range c.Volume {
		mount, err := parseVolume(raw)
		if err != nil {
			return err
		}

		mounts = append(mounts, mount)
	}

	env, err := c.envVars()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// sigs sees every SIGINT/SIGTERM the NotifyContext above sees; the
	// escalation handler installed after Boot counts them (second → SIGKILL,
	// third → give up). Registered here — before the first signal could fire —
	// so that signal's copy is guaranteed buffered when escalation consumes it.
	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(sigs)

	// Mint the id and drop the pid file BEFORE Boot: a concurrent `ossein gc`
	// must never see a booting instance (dir without a live pid) as dead and
	// reap it mid-pull.
	instanceID := container.NewID()

	dir, err := container.InstanceDir(instanceID)
	if err != nil {
		return err
	}

	if err := writePid(dir, os.Getpid()); err != nil {
		return err
	}

	// Registered before inst.Close's defer below so LIFO runs Close first and
	// the console log survives until teardown has finished.
	defer func() {
		if removeInstanceDirOnReturn(err) {
			_ = os.RemoveAll(dir)
		}
	}()

	spec := container.RunSpec{
		InstanceID: instanceID,
		Image:      c.Image,
		Platform:   c.Platform,
		Pull:       c.Pull,
		Command:    c.Command,
		Cwd:        c.Cwd,
		User:       c.User,
		Env:        env,
		TTY:        c.TTY,
		Privileged: c.Privileged,
		Network:    c.Network,
		CPUs:       c.CPUs,
		MemoryMiB:  c.Memory,
		Mounts:     mounts,
		ConsoleLog: c.ConsoleLog,
		Stdout:     os.Stdout,
	}

	switch {
	case c.TTY:
		spec.Stdin = os.Stdin
	default:
		spec.Stderr = os.Stderr

		// Deliberate deviation from docker (which requires -i even for pipes):
		// a piped stdin auto-attaches, so build-tool one-liners like
		// `git archive … | ossein run img tar -x` work without remembering -i.
		// A terminal stdin still requires -i, so interactive use keeps docker's
		// behavior.
		if c.Interactive || stdinIsPipe() {
			spec.Stdin = os.Stdin
		}
	}

	cache, err := image.NewCache()
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	inst, _, err := container.Boot(ctx, *art, cache, spec)
	if err != nil {
		return err
	}
	defer inst.Close(context.Background())

	release := escalateSignals(ctx, sigs, inst, dir)
	defer release()

	if err := inst.StartProcess(ctx); err != nil {
		return err
	}

	if c.TTY && term.IsTerminal(int(os.Stdin.Fd())) {
		restore := makeRaw(ctx, inst)
		defer restore()
	}

	code, err := inst.Wait(context.Background())
	if err != nil {
		return err
	}

	if code != 0 {
		// Return (not os.Exit) so the deferred terminal restore, instance
		// teardown, and state-dir removal all run; main translates this into
		// the process exit code once the stack has unwound.
		return exitError{code: int(code)}
	}

	return nil
}

// envVars assembles the container environment from --env-file(s) then -e flags,
// docker order: files first, -e last. ocispec then dedups against the image env
// keeping the last occurrence, so -e overrides --env-file overrides the image.
func (c *runCmd) envVars() ([]string, error) {
	var out []string

	for _, path := range c.EnvFile {
		vars, err := parseEnvFile(path)
		if err != nil {
			return nil, err
		}

		out = append(out, vars...)
	}

	flags, err := resolveEnv(c.Env)
	if err != nil {
		return nil, err
	}

	return append(out, flags...), nil
}

func stdinIsPipe() bool {
	stat, err := os.Stdin.Stat()

	return err == nil && stat.Mode()&os.ModeCharDevice == 0
}

// escalateSignals forwards shutdown signals to the guest, docker-style. The
// first SIGINT/SIGTERM cancels ctx (signal.NotifyContext) and is forwarded to
// the guest as SIGTERM; a second signal escalates to SIGKILL; a third gives up
// on the guest entirely: os.Exit(130) (128+SIGINT) — after two ignored kills
// the deferred teardown cannot be trusted to finish, so the state dir is
// dropped best-effort and gc reaps whatever survives. sigs must have been
// signal.Notify-registered alongside the NotifyContext so the first signal's
// copy is buffered before escalation consumes it. The returned release
// detaches the handler; call it once the workload has exited.
func escalateSignals(ctx context.Context, sigs <-chan os.Signal, inst *container.Instance, stateDir string) func() {
	done := make(chan struct{})

	go func() { // #nosec G118 -- escalation must outlive the cancelled ctx; each kill goes out on a fresh context
		select {
		case <-done:
			return
		case <-ctx.Done():
		}

		// ctx is already cancelled; the SIGTERM must go out on a fresh context.
		_ = inst.Kill(context.Background(), int32(syscall.SIGTERM)) //nolint:contextcheck // ctx already done

		// The signal that cancelled ctx was also delivered to sigs — consume
		// that copy so the next receive is a genuine second signal.
		select {
		case <-done:
			return
		case <-sigs:
		}

		select {
		case <-done:
			return
		case <-sigs:
		}

		// Second signal: the guest ignored SIGTERM — SIGKILL its init process.
		_ = inst.Kill(context.Background(), int32(syscall.SIGKILL)) //nolint:contextcheck // ctx already done

		select {
		case <-done:
			return
		case <-sigs:
		}

		// Third signal: even the SIGKILL RPC didn't end it (wedged control
		// channel). Deferred cleanup would trap the user, so die here.
		_ = os.RemoveAll(stateDir)
		//revive:disable-next-line:deep-exit
		os.Exit(exitInterrupted)
	}()

	return func() { close(done) }
}

// makeRaw puts the terminal in raw mode, wires SIGWINCH resizes, and returns a
// restore func.
func makeRaw(ctx context.Context, inst *container.Instance) func() {
	stdinFD := int(os.Stdin.Fd())

	old, err := term.MakeRaw(stdinFD)
	if err != nil {
		return func() {}
	}

	resize := func() {
		width, height, err := term.GetSize(stdinFD)
		if err != nil {
			return
		}

		rows, cols := uint32(height), uint32(width) // #nosec G115 -- terminal rows/cols are small non-negative ints
		_ = inst.Resize(ctx, rows, cols)
	}
	resize()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	go func() {
		for range winch {
			resize()
		}
	}()

	return func() {
		// Detach WINCH before closing the channel (Stop guarantees no further
		// sends) so the resize goroutine exits with the raw-mode session.
		signal.Stop(winch)
		close(winch)

		_ = term.Restore(stdinFD, old)
	}
}

// --- buildkit ---

type buildkitCmd struct {
	CPUs   uint   `default:"4"    help:"vCPUs"      name:"cpus"`
	Memory uint64 `default:"8192" help:"memory MiB"`
	// The default is the digest-pinned image linked in at build time (main.buildkitImage,
	// fed from the Justfile); kong interpolates it via kong.Vars.
	Image      string `default:"${buildkit_image}"                                                                                             help:"buildkit image (digest-pinned by default)"`
	Sock       string `help:"host unix socket path (default: state dir)"`
	ConsoleLog string `help:"guest console log file"                                                                                           name:"console-log"`
	Cache      string `help:"persistent buildkit cache: a name (central, default: current dir) or a path like ./.ossein/cache (project-local)"`
	PrintCache bool   `help:"resolve and print the cache directory, then exit"                                                                 name:"print-cache"`
	Detach     bool   `help:"background the VM, print BUILDKIT_HOST, exit"`
	// InstanceID/CacheDir/CacheLocal are internal: the detach launcher sets them
	// on the re-exec'd child so it shares the parent's instance dir and the
	// already-resolved cache location (no re-resolution against a changed CWD).
	InstanceID string `help:"internal: forced instance id"           hidden:"" name:"instance-id"`
	CacheDir   string `help:"internal: pre-resolved cache directory" hidden:"" name:"cache-dir"`
	CacheLocal bool   `help:"internal: cache dir is project-local"   hidden:"" name:"cache-local"`
}

func (c *buildkitCmd) Run(logger *slog.Logger, art *container.Artifacts, level string) (err error) {
	if err := requireArtifacts(art); err != nil {
		return err
	}

	cacheDir, gitignore, err := c.resolveCacheDir()
	if err != nil {
		return err
	}

	// --print-cache: reveal the resolved location and exit (no VM).
	if c.PrintCache {
		fmt.Fprintln(os.Stdout, cacheDir)

		return nil
	}

	// Fail fast with an actionable message if this project's cache is already
	// held by a running instance — instead of dying deep inside a detached child
	// with only a terse log line. (The child still enforces the lock; this is a
	// courtesy pre-check, so a lost race just falls back to the real error.)
	if busy, err := volume.InUse(cacheDir); err != nil {
		return err
	} else if busy {
		return fmt.Errorf(
			"%w %q — run `ossein stop` first, or use --cache <name> for a separate cache",
			errCacheBusy,
			cacheDir,
		)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A user-supplied socket that something still answers on belongs to a
	// previous instance (or an unrelated daemon): fail before booting a VM —
	// ExposeUnix would refuse it anyway, and detach would otherwise print a
	// BUILDKIT_HOST that points at the wrong listener.
	if c.Sock != "" {
		if err := ensureSocketFree(ctx, c.Sock); err != nil {
			return err
		}
	}

	// Detach: re-exec ourselves in the background with a pre-chosen socket
	// path, wait until buildkitd answers, print the export line, exit. (A
	// foreground `ossein buildkit` inside `eval "$(…)"` deadlocks: command
	// substitution waits for process exit, but the process IS the VM.)
	if c.Detach {
		return c.detach(ctx, logger, art, level, cacheDir, gitignore)
	}

	// See runCmd.Run: registered alongside the NotifyContext so the escalation
	// handler can count signals past the first.
	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(sigs)

	// Persistent per-project cache: acquire (create+format on first use) the
	// ext4 volume that backs /var/lib/buildkit so this build's cache survives
	// the VM and warms the next build. The lock is held for this process's life.
	vol, err := volume.Ensure(cacheDir, buildkitCacheSize, gitignore)
	if err != nil {
		return fmt.Errorf("buildkit cache volume: %w", err)
	}
	defer func() { _ = vol.Close() }()

	logger.Info("buildkit cache", "dir", cacheDir, "image", vol.Path)

	// buildkitd logs logrus-JSON; forward each line back through our slog logger
	// so its output shares our format, level, and destination. The Flush is
	// registered before inst.Close's defer below, so LIFO flushes only after
	// Close has quiesced the VM (and waitBuildkit has already drained the
	// relays via Wait) — no relay can Write after Flush.
	bkLogs := newLogForwarder(logger, "buildkitd")
	defer bkLogs.Flush()

	// Mint the id (unless the detach parent forced one, sharing its dir) and
	// drop the pid file BEFORE Boot: a concurrent `ossein gc` must never see a
	// booting instance (dir without a live pid) as dead and reap it mid-pull.
	instanceID := c.InstanceID
	if instanceID == "" {
		instanceID = container.NewID()
	} else if err := validateInstanceID(instanceID); err != nil {
		// The forced id is joined into the state root and the resulting dir is
		// deferred-RemoveAll'd — an escaping id must never get that far.
		return err
	}

	dir, err := container.InstanceDir(instanceID)
	if err != nil {
		return err
	}

	if err := writePid(dir, os.Getpid()); err != nil {
		return err
	}

	// Registered before inst.Close's defer below so LIFO runs Close first and
	// the console log survives until teardown has finished.
	defer func() {
		if removeInstanceDirOnReturn(err) {
			_ = os.RemoveAll(dir)
		}
	}()

	spec := container.RunSpec{
		InstanceID: instanceID,
		Image:      c.Image,
		Command:    []string{"buildkitd", "--addr", "unix://" + guestBkSock, "--log-format", "json"},
		Privileged: true,
		Network:    true,
		CPUs:       c.CPUs,
		MemoryMiB:  c.Memory,
		ConsoleLog: c.ConsoleLog,
		Disks:      []container.DiskMount{{ImagePath: vol.Path, GuestPath: buildkitDataDir}},
		Stdout:     bkLogs,
		Stderr:     bkLogs,
	}

	cache, err := image.NewCache()
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	inst, _, err := container.Boot(ctx, *art, cache, spec)
	if err != nil {
		return err
	}
	defer inst.Close(context.Background())

	release := escalateSignals(ctx, sigs, inst, dir)
	defer release()

	if err := inst.StartProcess(ctx); err != nil {
		return err
	}

	hostSock, err := resolveSock(c.Sock, inst.Dir)
	if err != nil {
		return err
	}

	cleanup, err := inst.ExposeUnix(ctx, guestBkSock, hostSock)
	if err != nil {
		// ExposeUnix refuses a host socket something still answers on (its
		// error names the path); name the fix, not just the failure.
		return fmt.Errorf(
			"exposing buildkitd: %w — `ossein stop` the instance using it, or pass a different --sock", err,
		)
	}
	defer cleanup()

	fmt.Fprintf(os.Stdout, "export BUILDKIT_HOST=unix://%s\n", hostSock)
	logger.Info("buildkit up — Ctrl-C to stop", "sock", hostSock)

	return waitBuildkit(ctx, inst)
}

// resolveSock turns the --sock flag into the absolute host socket path
// (default: the instance dir). Absolute matters: BUILDKIT_HOST=unix://<relative>
// is resolved by buildctl against ITS cwd, so a relative path printed verbatim
// silently stops working after any `cd`.
func resolveSock(flag, instanceDir string) (string, error) {
	if flag == "" {
		return filepath.Join(instanceDir, hostBkSock), nil
	}

	abs, err := filepath.Abs(flag)
	if err != nil {
		return "", fmt.Errorf("resolving --sock %q: %w", flag, err)
	}

	return abs, nil
}

// waitBuildkit blocks until buildkitd exits. On the signal path the escalation
// handler (escalateSignals) owns forwarding, which is what makes the final
// <-waitErr interruptible: a second signal SIGKILLs the guest — ending the
// Wait — and a third exits the process outright.
func waitBuildkit(ctx context.Context, inst *container.Instance) error {
	waitErr := make(chan error, 1)

	// Background, not ctx: on Ctrl-C the escalation handler SIGTERMs the guest;
	// this Wait must keep blocking until buildkitd *actually* exits so the
	// <-waitErr teardown is synchronized. Cancelling it would abandon the wait
	// (and race a bogus "context canceled" error onto a clean stop).
	// #nosec G118 -- + waitBuildkit$1: Background is the deliberate teardown context (see comment)
	go func() { //nolint:contextcheck // + waitBuildkit$1: Background is the deliberate teardown context (see comment)
		_, err := inst.Wait(context.Background())
		waitErr <- err
	}()

	select {
	case <-ctx.Done():
		<-waitErr

		return nil
	case err := <-waitErr:
		if err != nil {
			return fmt.Errorf("buildkitd exited: %w", err)
		}

		return fmt.Errorf("%w: exited unexpectedly", errBuildkit)
	}
}

// resolveCacheDir turns the cache selection into a concrete directory and
// whether it is project-local (which gets a .gitignore). Precedence:
//   - CacheDir (internal): the detach parent already resolved it — use verbatim,
//     so the child never re-resolves against a possibly-different CWD.
//   - --cache is path-like (contains a separator or starts with "."): a
//     project-local directory at that path.
//   - --cache is a bare name: a central cache keyed by that name.
//   - --cache is empty (default): a central cache keyed by the current dir.
func (c *buildkitCmd) resolveCacheDir() (dir string, gitignore bool, err error) {
	if c.CacheDir != "" {
		return c.CacheDir, c.CacheLocal, nil
	}

	switch {
	case c.Cache == "":
		cwd, err := os.Getwd()
		if err != nil {
			return "", false, fmt.Errorf("resolving current dir for cache key: %w", err)
		}

		central, err := volume.CentralDir(cwd)

		return central, false, err
	case looksLikePath(c.Cache):
		abs, err := filepath.Abs(c.Cache)
		if err != nil {
			return "", false, fmt.Errorf("resolving cache path %q: %w", c.Cache, err)
		}

		return abs, true, nil
	default:
		central, err := volume.CentralDir(c.Cache)

		return central, false, err
	}
}

// looksLikePath reports whether a --cache value denotes a filesystem path
// (project-local storage) rather than a bare central-cache name.
func looksLikePath(value string) bool {
	return strings.ContainsRune(value, filepath.Separator) || strings.HasPrefix(value, ".")
}

// detach backgrounds a foreground `ossein buildkit` child (own session, logs
// to the state dir), waits for buildkitd to answer, prints the export line,
// and returns. The child is a user-scoped per-instance process, not a daemon.
func (c *buildkitCmd) detach(
	ctx context.Context,
	logger *slog.Logger,
	art *container.Artifacts,
	level, cacheDir string,
	cacheLocal bool,
) error {
	// Pre-pull in the FOREGROUND: visible progress, and any network-consent
	// prompt targets a process the user can see. The child then starts warm.
	cache, err := image.NewCache()
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	img, err := image.Resolve(ctx, cache, c.Image, "", image.PullMissing)
	if err != nil {
		return fmt.Errorf("pre-pull %s: %w", c.Image, err)
	}

	if err := warm(img); err != nil {
		return fmt.Errorf("pre-pull %s: %w", c.Image, err)
	}

	// Choose the instance id up front so the backgrounded child and this parent
	// share ONE instance dir: its console log, buildkit log, pid, and socket all
	// land together under RuntimeDir()/<id>.
	instanceID := container.NewID()

	dir, err := container.InstanceDir(instanceID)
	if err != nil {
		return err
	}

	sock, err := resolveSock(c.Sock, dir)
	if err != nil {
		return err
	}

	logPath := filepath.Join(dir, "buildkit.log")

	logFile, err := os.Create(logPath) // #nosec G304 -- logPath is a ossein-owned state-dir path
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	child, err := c.childCmd(art, level, instanceID, sock, cacheDir, cacheLocal)
	if err != nil {
		return err
	}

	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return fmt.Errorf("starting background buildkit: %w", err)
	}

	if err := writePid(dir, child.Process.Pid); err != nil {
		// Without a pid file the background VM would be unmanageable (stop/gc
		// could never find it): kill and reap the child before reporting.
		_ = child.Process.Kill()
		_ = child.Wait()

		return err
	}

	go func() { _ = child.Wait() }() // reap if it dies while we poll

	logger.Info("buildkit starting in background", pidFileName, child.Process.Pid, "log", logPath)

	if err := awaitSocket(ctx, sock, child.Process.Pid, logPath, bkReadyTimeout); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "export BUILDKIT_HOST=unix://%s\n", sock)
	logger.Info("buildkit ready", logKeyID, instanceID, "stop", "ossein stop "+instanceID)

	return nil
}

// childCmd builds the foreground re-exec: global flags precede the subcommand
// in kong, and --detach is omitted so the child runs in the foreground.
// cacheLocal legitimately IS a forwarded CLI flag, not control coupling.
//
//revive:disable-next-line:flag-parameter
func (c *buildkitCmd) childCmd(
	art *container.Artifacts,
	level, instanceID, sock, cacheDir string,
	cacheLocal bool,
) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating own binary: %w", err)
	}

	args := []string{
		"--log-level", level,
		"--kernel", art.Kernel,
		"--initfs", art.Initfs,
		"buildkit",
		"--instance-id", instanceID, // share the parent's instance dir
		"--cache-dir", cacheDir, // pre-resolved cache location; child holds the lock
		"--image", c.Image,
		"--sock", sock,
		"--cpus", strconv.FormatUint(uint64(c.CPUs), decimal),
		"--memory", strconv.FormatUint(c.Memory, decimal),
	}
	if cacheLocal {
		args = append(args, "--cache-local")
	}

	if c.ConsoleLog != "" {
		args = append(args, "--console-log", c.ConsoleLog)
	}

	// #nosec G204 -- re-exec of our own binary (os.Executable) with our own flags; the child outlives any context
	//nolint:noctx // re-exec of our own binary (os.Executable) with our own flags; the child outlives any context
	return exec.Command(self, args...), nil
}

// ensureSocketFree fails fast when a user-supplied --sock path is still being
// served — by a previous instance or an unrelated daemon. A stale socket file
// nothing answers on is fine: ExposeUnix clears it.
func ensureSocketFree(ctx context.Context, sock string) error {
	conn, err := netDial(ctx, sock)
	if err != nil {
		return nil //nolint:nilerr // no listener = the path is free, which is the success case
	}

	_ = conn.Close()

	return fmt.Errorf(
		"%w: %s — `ossein stop` the instance using it, or pass a different --sock", errSockBusy, sock,
	)
}

// awaitSocket polls until the whole buildkitd proxy chain answers on sock,
// the child dies, or timeout elapses (then the child is killed: a VM that
// never became reachable is not worth keeping).
func awaitSocket(ctx context.Context, sock string, childPID int, logPath string, timeout time.Duration) error {
	// Capture the child's identity up front: it is reaped concurrently, and a
	// bare recycled pid would probe as "alive" forever.
	childStart, running := processStartTime(childPID)
	if !running {
		return fmt.Errorf("%w: child died — see %s", errBuildkit, logPath)
	}

	deadline := time.Now().Add(timeout)

	for !probeSocketChain(ctx, sock) {
		// Ctrl-C aborts the wait but leaves the recorded instance starting up:
		// it has a pid file, so `ossein stop` (or gc, once dead) handles it.
		if ctx.Err() != nil {
			return fmt.Errorf("%w: interrupted while waiting — `ossein stop` removes the background instance",
				errBuildkit)
		}

		if !processAlive(childPID, childStart) {
			return fmt.Errorf("%w: child died — see %s", errBuildkit, logPath)
		}

		if time.Now().After(deadline) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)

			return fmt.Errorf("%w: not answering after %s — see %s", errBuildkit, timeout, logPath)
		}

		time.Sleep(250 * time.Millisecond)
	}

	return nil
}

// removeInstanceDirOnReturn decides whether a command's deferred cleanup may
// delete the instance dir. On failure the dir is KEPT: boot errors point at
// console.log inside it (consoleErrFmt), and removing it would hand the user
// a path to a file that no longer exists. A workload's own nonzero status
// (exitError) is a completed run and cleans up normally; `ossein gc` reaps
// kept dirs once the pid is dead.
func removeInstanceDirOnReturn(err error) bool {
	var exit exitError

	return err == nil || errors.As(err, &exit)
}

// --- stop / gc ---

// validateInstanceID rejects ids that could resolve outside the state root
// once joined into a path — shared by stop's argv ids and buildkit's hidden
// --instance-id (whose dir is later os.RemoveAll'd, the sharp edge).
func validateInstanceID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%w: invalid instance id %q", errUsage, id)
	}

	return nil
}

type stopCmd struct {
	IDs   []string      `arg:""        help:"instance ids to stop (default: all)"                                name:"id"    optional:""`
	Grace time.Duration `default:"60s" help:"how long to wait for a clean shutdown (cache flush) before SIGKILL" name:"grace"`
}

func (c *stopCmd) Run(logger *slog.Logger) error {
	root, err := stateRoot()
	if err != nil {
		return err
	}

	// Explicit ids come straight from argv and are joined into state paths:
	// refuse anything that could resolve outside the state root.
	for _, target := range c.IDs {
		if err := validateInstanceID(target); err != nil {
			return err
		}
	}

	explicit := len(c.IDs) > 0
	targets := c.IDs

	if !explicit {
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			logger.Info("stop: nothing running")

			return nil
		}

		if err != nil {
			return fmt.Errorf("reading state dir: %w", err)
		}

		for _, entry := range entries {
			targets = append(targets, entry.Name())
		}
	}

	stopped := 0

	for _, target := range targets {
		if explicit {
			if _, err := os.Stat(filepath.Join(root, target)); errors.Is(err, os.ErrNotExist) {
				// Still exit 0: stop is idempotent — "not running" is the state
				// the user asked for; the message flags a likely typo'd id.
				logger.Info("stop: no such instance", logKeyID, target,
					"hint", "`ossein stop` with no id stops every instance")

				continue
			}
		}

		pid, start, ok := readPid(filepath.Join(root, target, pidFileName))
		if !ok || !processAlive(pid, start) {
			continue
		}

		if terminate(logger, pid, start, c.Grace) {
			logger.Info("stop: stopped", logKeyID, target, pidFileName, pid)

			stopped++
		}
	}

	if stopped == 0 {
		logger.Info("stop: nothing running")
	}

	return nil
}

type gcCmd struct {
	PruneCache bool `help:"also delete per-project buildkit cache volumes not currently in use" name:"prune-cache"`
}

// Run reclaims stale per-instance state (socket dirs of dead buildkit VMs) and
// image-cache blobs over quota. Container rootfs lives in guest tmpfs and dies
// with the VM, so there's nothing else to collect there. Per-project buildkit
// cache volumes are persistent and valuable, so they are pruned only with
// --prune-cache, and only when not in use.
func (c *gcCmd) Run(logger *slog.Logger) error {
	removed, err := gcStateDirs()
	if err != nil {
		return err
	}

	cache, err := image.NewCache()
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()

	stats, err := cache.GarbageCollect()
	if err != nil {
		return fmt.Errorf("image cache gc: %w", err)
	}

	fields := []any{"instance_dirs_removed", removed, "cache_bytes_freed", stats.BytesFreed}

	if c.PruneCache {
		freed, count, err := volume.PruneUnused()
		if err != nil {
			return fmt.Errorf("pruning buildkit cache: %w", err)
		}

		fields = append(fields, "buildkit_cache_removed", count, "buildkit_cache_bytes_freed", freed)
	}

	logger.Info("gc: done", fields...)

	return nil
}

func gcStateDirs() (int, error) {
	root, err := stateRoot()
	if err != nil {
		return 0, err
	}

	return gcRoot(root)
}

// gcRoot reaps dead instance dirs under root (split from gcStateDirs so tests
// can point it at a scratch root).
func gcRoot(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("reading state dir: %w", err)
	}

	removed := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue // instance state is per-directory; ignore stray files
		}

		dir := filepath.Join(root, entry.Name())

		// An instance is live if it answers on its buildkit socket (buildkit) or
		// still has a running pid (any instance). Only reap when both say dead.
		if conn, err := netDial(context.Background(), filepath.Join(dir, hostBkSock)); err == nil {
			_ = conn.Close()

			continue
		}

		if pidAlive(filepath.Join(dir, pidFileName)) {
			continue
		}

		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("removing %s: %w", dir, err)
		}

		removed++
	}

	return removed, nil
}

// warm materializes an image's rootfs blob into the content cache (used by
// pull and the detach pre-pull). RootfsFile drives the flatten to completion
// on a miss; the pin is released immediately since nothing boots here.
func warm(img *image.Image) error {
	pin, err := img.RootfsFile()
	if err != nil {
		return fmt.Errorf("warming image cache: %w", err)
	}

	if err := pin.Release(); err != nil {
		return fmt.Errorf("warming image cache: %w", err)
	}

	return nil
}

// --- shared helpers ---

// stateRoot is the per-user directory holding a backgrounded buildkit
// instance's ephemeral files — its socket, pid file, and log. dirs.RuntimeDir
// is the right home for these: they belong to a live process and are stale
// once the machine restarts, so the OS is free to reclaim them (on macOS it
// resolves under $TMPDIR/ossein).
func stateRoot() (string, error) {
	root, err := dirs.RuntimeDir()
	if err != nil {
		return "", fmt.Errorf("locating runtime dir: %w", err)
	}

	return root, nil
}

// netDial dials a unix socket honoring ctx. A package var, not a func: the
// only seam through which a test can hand probeSocketChain a connection whose
// Read fails with a non-timeout net.Error — darwin AF_UNIX reads yield clean
// EOF on every real peer-close shape, so no actual socket can produce one.
var netDial = func(ctx context.Context, sock string) (net.Conn, error) { //nolint:gochecknoglobals // deliberate test seam
	dialer := net.Dialer{Timeout: time.Second}

	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", sock, err)
	}

	return conn, nil
}

// processStartTime returns pid's kernel start time (seconds since the epoch) —
// the stable half of the pid:starttime identity. ok is false when no such
// process exists.
func processStartTime(pid int) (start int64, ok bool) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}

	return info.Proc.P_starttime.Sec, true
}

// processAlive reports whether pid is alive AND still the incarnation recorded
// at start — a recycled pid has a different start time and reads as dead.
func processAlive(pid int, start int64) bool {
	current, ok := processStartTime(pid)

	return ok && current == start
}

// writePid records "pid:starttime" in dir/pidFileName so `stop`/`gc` can find
// the instance and verify the pid was not recycled before signaling it.
func writePid(dir string, pid int) error {
	start, ok := processStartTime(pid)
	if !ok {
		return fmt.Errorf("%w: pid %d", errPidGone, pid)
	}

	record := strconv.Itoa(pid) + ":" + strconv.FormatInt(start, decimal)

	if err := os.WriteFile(filepath.Join(dir, pidFileName), []byte(record), pidFileMode); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}

	return nil
}

// terminate stops pid gracefully: SIGTERM, then poll for exit up to grace,
// escalating to SIGKILL if the process ignores the term. grace must be generous
// enough to cover a clean teardown — buildkitd's shutdown plus the cache disk's
// sync+unmount — or SIGKILL would truncate the flush and lose the cache. It
// reports whether the process is gone (or a kill was issued for it). A SIGKILL
// bypasses the instance's own state-dir cleanup, so gc reaps the leftover dir.
// Every signal is preceded by a start-time identity check so a recycled pid is
// never signaled (it just reads as already-stopped).
func terminate(logger *slog.Logger, pid int, start int64, grace time.Duration) bool {
	if !processAlive(pid, start) {
		return true // already gone (or the pid was recycled) — count it stopped
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return !processAlive(pid, start) // ESRCH: already gone, count it stopped
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid, start) {
			return true
		}

		time.Sleep(stopPollInterval)
	}

	if !processAlive(pid, start) {
		return true
	}

	logger.Warn("stop: process ignored SIGTERM, sending SIGKILL", pidFileName, pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)

	return true
}

// readPid parses the "pid:starttime" record at pidPath. ok is false when the
// file is missing or does not parse EXACTLY — these state dirs are ephemeral
// (see stateRoot), so an old-format, truncated, or garbage file is simply
// stale, never a reason to guess.
func readPid(pidPath string) (pid int, start int64, ok bool) {
	raw, err := os.ReadFile(pidPath) // #nosec G304 -- path under ossein's state dir
	if err != nil {
		return 0, 0, false
	}

	pidField, startField, found := strings.Cut(string(raw), ":")
	if !found {
		return 0, 0, false
	}

	pid, err = strconv.Atoi(pidField)
	if err != nil || pid <= 0 {
		return 0, 0, false
	}

	start, err = strconv.ParseInt(startField, decimal, int64Bits)
	if err != nil || start <= 0 {
		return 0, 0, false
	}

	return pid, start, true
}

// pidAlive reports whether the process recorded in pidPath is that same
// process, still live. Missing/unreadable/garbage pid files read as not-alive.
func pidAlive(pidPath string) bool {
	pid, start, ok := readPid(pidPath)

	return ok && processAlive(pid, start)
}

// probeSocketChain verifies the WHOLE proxy chain (host unix → vsock →
// vminitd → guest unix → buildkitd), not just the host listener. A broken
// guest leg closes the connection immediately (fast EOF/reset). A healthy one
// either sends bytes right away (gRPC/h2 servers emit their SETTINGS frame on
// connect) or holds the connection open silently — both are ready.
func probeSocketChain(ctx context.Context, sock string) bool {
	conn, err := netDial(ctx, sock)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))

	var buf [1]byte

	_, rerr := conn.Read(buf[:])
	if rerr == nil {
		return true // backend spoke first (h2 SETTINGS) — chain healthy
	}

	var nerr net.Error

	// Only a read TIMEOUT means "held open = ready". Any other net.Error —
	// notably *net.OpError wrapping ECONNRESET — is a broken chain, exactly
	// like EOF, and must not print a BUILDKIT_HOST nobody can use.
	return errors.As(rerr, &nerr) && nerr.Timeout()
}
