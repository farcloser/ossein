//go:build darwin && arm64

package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/farcloser/ossein/internal/cli"
)

// runCmd is `docker run` narrowed to what farcloser scripts pass. Every flag
// maps onto `ossein run` (whose flags are already docker-shaped) and the
// process execs into it; the only translation is docker's resource syntax
// (`--cpus 1.5`, `--memory 8g`) and `--network none`. Where docker and ossein
// disagree, docker wins: a run that sizes nothing gets the whole host, as a
// docker container does — not `ossein run`'s own 2 vCPU / 4 GiB.
type runCmd struct {
	CPUs        string `help:"vCPUs (docker syntax: a decimal, rounded up; default: every host CPU)"                                       name:"cpus"`
	Memory      string `help:"memory (docker syntax: a number with an optional k/m/g/t unit, rounded up to MiB; default: all host memory)" name:"memory"  short:"m"`
	Interactive bool   `help:"keep stdin open"                                                                                             short:"i"`
	TTY         bool   `help:"allocate a pseudo-TTY"                                                                                       name:"tty"     short:"t"`
	Privileged  bool   `help:"grant all capabilities"`
	Network     string `help:"none | host | bridge | default (docker syntax; only 'none' changes anything)"                                name:"network"`
	Rm          bool   `help:"no-op; the container is always removed on exit"                                                              name:"rm"`
	Workdir     string `help:"working directory inside the container"                                                                      name:"workdir" short:"w"`
	User        string `help:"numeric uid[:gid]"                                                                                           short:"u"`
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

	// Always sized explicitly: cli.WholeHost is what docker means by no
	// --cpus / --memory at all.
	var cpus, mib uint64 = cli.WholeHost, cli.WholeHost

	if c.CPUs != "" {
		parsed, err := parseCPUs(c.CPUs)
		if err != nil {
			return nil, err
		}

		cpus = parsed
	}

	if c.Memory != "" {
		parsed, err := parseMemoryMiB(c.Memory)
		if err != nil {
			return nil, err
		}

		mib = parsed
	}

	args = append(args, "--cpus", strconv.FormatUint(cpus, decimal), "--memory", strconv.FormatUint(mib, decimal))

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

	// Binary units, as docker reads every --memory suffix; mebi is also
	// ossein's --memory unit.
	kibi = 1 << 10
	mebi = 1 << 20
	gibi = 1 << 30
	tebi = 1 << 40
	pebi = 1 << 50
)

// memorySpec is docker's --memory grammar (go-units' RAMInBytes): a number,
// optionally decimal, an optional unit letter, then an optional "i" and/or
// "b" — so 8g, 8G, 8gb, 8GiB and 1.5g all parse, and every unit is binary.
var memorySpec = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?) ?([kKmMgGtTpP])?[iI]?[bB]?$`)

// memoryUnit is the byte multiplier for a memorySpec unit letter.
func memoryUnit(letter string) float64 {
	switch strings.ToLower(letter) {
	case "k":
		return kibi
	case "m":
		return mebi
	case "g":
		return gibi
	case "t":
		return tebi
	case "p":
		return pebi
	default:
		return 1
	}
}

// parseCPUs reads docker's decimal --cpus (a share, e.g. 1.5) and rounds it
// up to whole vCPUs, the only granularity a microVM has.
func parseCPUs(value string) (uint64, error) {
	cpus, err := strconv.ParseFloat(value, bits64)
	if err != nil || cpus <= 0 || math.IsInf(cpus, 0) {
		return 0, fmt.Errorf("%w: --cpus %q is not a positive number", errUnsupported, value)
	}

	return uint64(math.Ceil(cpus)), nil
}

// parseMemoryMiB reads docker's --memory (see memorySpec) and returns whole
// MiB, rounded up, at least 1.
func parseMemoryMiB(value string) (uint64, error) {
	match := memorySpec.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf(
			"%w: --memory %q is not a docker size (a number, optionally k/m/g/t)",
			errUnsupported,
			value,
		)
	}

	amount, err := strconv.ParseFloat(match[1], bits64)
	if err != nil {
		return 0, fmt.Errorf("%w: --memory %q: %w", errUnsupported, value, err)
	}

	bytes := amount * memoryUnit(match[2])
	if bytes <= 0 || math.IsInf(bytes, 0) || bytes >= math.MaxUint64 {
		return 0, fmt.Errorf("%w: --memory %q is not a positive size", errUnsupported, value)
	}

	return max(uint64(math.Ceil(bytes/mebi)), 1), nil
}
