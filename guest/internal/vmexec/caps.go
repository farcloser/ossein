//go:build linux

package vmexec

import (
	"fmt"
	"strings"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// capByName maps OCI capability names ("CAP_CHOWN") to library Cap values.
//
//nolint:gochecknoglobals // immutable lookup table, built once at init and only read afterwards
var capByName = func() map[string]capability.Cap {
	m := make(map[string]capability.Cap)
	for _, c := range capability.ListKnown() {
		m["CAP_"+strings.ToUpper(c.String())] = c
	}

	return m
}()

// toCaps resolves OCI capability names; an unknown name is an error, never a
// silent drop.
func toCaps(names []string) ([]capability.Cap, error) {
	out := make([]capability.Cap, 0, len(names))

	for _, n := range names {
		c, ok := capByName[strings.ToUpper(n)]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnknownCapability, n)
		}

		out = append(out, c)
	}

	return out, nil
}

// capState carries the configured capability sets between the pre- and post-uid
// phases (vmexec's prepareCapabilities / finishCapabilities). A nil caps means
// the spec requested no capability changes and both phases are no-ops.
type capState struct {
	caps capability.Capabilities
}

// prepareCaps sets the OCI capability sets, applies the bounding set (dropping
// caps early, before the uid change), and enables keepcaps so the remaining
// sets survive setuid. Call before setCredentials; call (*capState).finish
// after. Per OCI, a nil capabilities object means "no change": the whole phase
// is skipped, not applied with empty sets.
func prepareCaps(oci *specs.LinuxCapabilities) (*capState, error) {
	if oci == nil {
		return &capState{}, nil
	}

	caps, err := capability.NewPid2(0)
	if err != nil {
		return nil, fmt.Errorf("load caps: %w", err)
	}

	caps.Clear(capability.CAPS | capability.BOUNDING | capability.AMBIENT)

	for _, capSet := range []struct {
		which capability.CapType
		names []string
	}{
		{capability.BOUNDING, oci.Bounding},
		{capability.PERMITTED, oci.Permitted},
		{capability.EFFECTIVE, oci.Effective},
		{capability.INHERITABLE, oci.Inheritable},
		{capability.AMBIENT, oci.Ambient},
	} {
		list, err := toCaps(capSet.names)
		if err != nil {
			return nil, err
		}

		caps.Set(capSet.which, list...)
	}

	if err := caps.Apply(capability.BOUNDING); err != nil {
		return nil, fmt.Errorf("apply bounding set: %w", err)
	}

	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("set keepcaps: %w", err)
	}

	return &capState{caps: caps}, nil
}

// finish clears keepcaps and applies the effective/permitted/inheritable and
// ambient sets after the uid change. A no-op when prepareCaps was one.
func (s *capState) finish() error {
	if s.caps == nil {
		return nil
	}

	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("clear keepcaps: %w", err)
	}

	if err := s.caps.Apply(capability.CAPS); err != nil {
		return fmt.Errorf("apply capability sets: %w", err)
	}

	_ = s.caps.Apply(capability.AMBIENT) // ambient is best-effort (kernel/support-dependent)

	return nil
}
