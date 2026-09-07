//go:build darwin && arm64

package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// runCmd is `docker run` narrowed to what farcloser scripts pass. Every flag
// maps onto `ossein run` (whose flags are already docker-shaped) and the
// process execs into it; the only translation is docker's resource syntax
// (`--cpus 1.5`, `--memory 8g`) and `--network none`.
type runCmd struct {
	CPUs        string `help:"vCPUs (docker syntax: a decimal; rounded up)"                                     name:"cpus"`
	Memory      string `help:"memory (docker syntax: bytes with an optional b/k/m/g suffix; rounded up to MiB)" name:"memory"  short:"m"`
	Interactive bool   `help:"keep stdin open"                                                                  short:"i"`
	TTY         bool   `help:"allocate a pseudo-TTY"                                                            name:"tty"     short:"t"`
	Privileged  bool   `help:"grant all capabilities"`
	Network     string `help:"none | host | bridge | default (docker syntax; only 'none' changes anything)"     name:"network"`
	Rm          bool   `help:"no-op; the container is always removed on exit"                                   name:"rm"`
	Workdir     string `help:"working directory inside the container"                                           name:"workdir" short:"w"`
	User        string `help:"numeric uid[:gid]"                                                                short:"u"`
	// sep:"none" on every repeatable value flag: kong would otherwise split
	// values on commas.
	Env      []string `help:"KEY=VALUE, or bare KEY to pass through from the host (repeatable)" sep:"none"                  short:"e"`
	EnvFile  []string `help:"read environment variables from a file (repeatable)"               name:"env-file"             sep:"none"`
	Volume   []string `help:"bind mount host-dir:dest[:options] (repeatable)"                   name:"volume"               sep:"none"                                   short:"v"`
	Platform string   `help:"linux/amd64 | linux/arm64 (default: host)"`
	Pull     string   `default:"missing"                                                        enum:"always,missing,never" help:"pull policy: always | missing | never"`
	Image    string   `arg:""                                                                   help:"image reference"`
	Command  []string `arg:""                                                                   help:"command + args"       optional:""                                  passthrough:""`
}

func (c *runCmd) Run(level string) error {
	args, err := c.osseinArgs()
	if err != nil {
		return err
	}

	return execOssein(level, args...)
}

// osseinArgs is the `ossein run` argument vector for this invocation.
func (c *runCmd) osseinArgs() ([]string, error) {
	args := []string{"run"}

	if c.CPUs != "" {
		cpus, err := parseCPUs(c.CPUs)
		if err != nil {
			return nil, err
		}

		args = append(args, "--cpus", strconv.FormatUint(cpus, decimal))
	}

	if c.Memory != "" {
		mib, err := parseMemoryMiB(c.Memory)
		if err != nil {
			return nil, err
		}

		args = append(args, "--memory", strconv.FormatUint(mib, decimal))
	}

	switch c.Network {
	case "", "host", "bridge", "default":
		// ossein's default: outbound networking. docker's named networks have
		// no equivalent; host/bridge/default all mean "networked".
	case "none":
		args = append(args, "--no-network")
	default:
		return nil, fmt.Errorf("%w: --network %q (only none, host, bridge, default)", errUnsupported, c.Network)
	}

	for _, toggle := range []struct {
		flag string
		set  bool
	}{
		{"--interactive", c.Interactive}, {"--tty", c.TTY}, {"--privileged", c.Privileged}, {"--rm", c.Rm},
	} {
		if toggle.set {
			args = append(args, toggle.flag)
		}
	}

	for _, opt := range []struct{ flag, value string }{
		{"--workdir", c.Workdir}, {"--user", c.User}, {"--platform", c.Platform},
	} {
		if opt.value != "" {
			args = append(args, opt.flag, opt.value)
		}
	}

	args = append(args, "--pull", c.Pull)

	for _, env := range c.Env {
		args = append(args, "--env", env)
	}

	for _, file := range c.EnvFile {
		args = append(args, "--env-file", file)
	}

	for _, vol := range c.Volume {
		args = append(args, "--volume", vol)
	}

	// "--" so an image or command that starts with a dash is never read as
	// one of ossein's flags.
	args = append(args, "--", c.Image)

	return append(args, c.Command...), nil
}

const (
	decimal = 10
	bits64  = 64

	// mebi is docker's "m" and ossein's --memory unit.
	mebi = 1 << 20
	kibi = 1 << 10
	gibi = 1 << 30
)

// parseCPUs reads docker's decimal --cpus (a share, e.g. 1.5) and rounds it
// up to whole vCPUs, the only granularity a microVM has.
func parseCPUs(value string) (uint64, error) {
	cpus, err := strconv.ParseFloat(value, bits64)
	if err != nil || cpus <= 0 || math.IsInf(cpus, 0) {
		return 0, fmt.Errorf("%w: --cpus %q is not a positive number", errUnsupported, value)
	}

	return uint64(math.Ceil(cpus)), nil
}

// parseMemoryMiB reads docker's --memory (bytes, or a number with a b, k, m
// or g suffix, case-insensitive) and returns whole MiB, rounded up, at least 1.
func parseMemoryMiB(value string) (uint64, error) {
	spec := strings.ToLower(strings.TrimSpace(value))
	multiplier := uint64(1)

	if len(spec) > 0 {
		switch spec[len(spec)-1] {
		case 'b':
			spec = spec[:len(spec)-1]
		case 'k':
			multiplier, spec = kibi, spec[:len(spec)-1]
		case 'm':
			multiplier, spec = mebi, spec[:len(spec)-1]
		case 'g':
			multiplier, spec = gibi, spec[:len(spec)-1]
		default:
			// no suffix: bytes
		}
	}

	amount, err := strconv.ParseUint(spec, decimal, bits64)
	if err != nil || amount == 0 || amount > math.MaxUint64/multiplier {
		return 0, fmt.Errorf("%w: --memory %q is not a positive size", errUnsupported, value)
	}

	bytes := amount * multiplier

	return max((bytes+mebi-1)/mebi, 1), nil
}

// --- pull ---

// pullCmd is `docker pull`, passed straight to `ossein pull`.
type pullCmd struct {
	Platform string `help:"linux/amd64 | linux/arm64 (default: host)"`
	Image    string `arg:""                                           help:"image reference"`
}

func (c *pullCmd) Run(level string) error {
	args := []string{"pull"}
	if c.Platform != "" {
		args = append(args, "--platform", c.Platform)
	}

	return execOssein(level, append(args, "--", c.Image)...)
}

// --- stop ---

// stopCmd is `docker stop` with ossein's meaning: it stops background buildkit
// instances (all of them, or the given ids) — the only long-lived thing
// ossein ever leaves running.
type stopCmd struct {
	IDs []string `arg:"" help:"ossein instance ids to stop (default: all)" name:"id" optional:""`
}

func (c *stopCmd) Run(level string) error {
	return execOssein(level, append([]string{"stop"}, c.IDs...)...)
}
