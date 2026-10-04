package cli_test

import (
	"errors"
	"testing"

	"github.com/mycophonic/primordium/bytesize"

	"github.com/farcloser/ossein/internal/cli"
)

func TestParseSize(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]uint64{
		"4GiB": 4 * bytesize.GiB, "4gib": 4 * bytesize.GiB, "512MiB": 512 * bytesize.MiB,
		"1.5GB": 1500 * bytesize.MB, "512 MiB": 512 * bytesize.MiB, "512B": 512,
		"0": 0, "0MiB": 0, "0B": 0,
	} {
		got, err := cli.ParseSize(value)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = (%d, %v), want %d", value, got, err, want)
		}
	}

	// A bare number meant MiB before sizes took units: never read as bytes.
	for _, bare := range []string{"512", "4096", "1.5"} {
		if _, err := cli.ParseSize(bare); !errors.Is(err, cli.ErrSizeUnit) {
			t.Errorf("ParseSize(%q) = %v, want ErrSizeUnit", bare, err)
		}
	}

	for _, bad := range []string{"", "g", "4g", "-1GiB", "4 GiB B", "GiB"} {
		if _, err := cli.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) was accepted", bad)
		}
	}
}

func TestFormatSizeRoundTrips(t *testing.T) {
	t.Parallel()

	for _, size := range []uint64{0, 1, 512 * bytesize.MiB, 8*bytesize.GiB + 1} {
		if got, err := cli.ParseSize(cli.FormatSize(size)); err != nil || got != size {
			t.Errorf("ParseSize(FormatSize(%d)) = (%d, %v)", size, got, err)
		}
	}
}
