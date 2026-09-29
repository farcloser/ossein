package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mycophonic/primordium/human"
)

// ErrSizeUnit is a size flag given a number with no unit.
var ErrSizeUnit = errors.New("a size needs a unit")

// decimalBase is the base FormatSize writes a byte count in.
const decimalBase = 10

// ParseSize reads a size flag in bytes: a number and a unit, "4GiB", "512MiB",
// "1.5GB", as human.ParseSize reads it (kB to EB count powers of 1000, KiB to
// EiB powers of 1024, in any case). A bare number is ErrSizeUnit unless it is
// zero, which is the same in every unit: --memory counted MiB before it took
// units, and "512" read as bytes would shrink a guest 2^20-fold in silence.
func ParseSize(value string) (uint64, error) {
	size, err := human.ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", value, err)
	}

	if size != 0 && strings.Trim(value, "0123456789.") == "" {
		return 0, fmt.Errorf("%w: %q (say %sMiB, %sGiB, or %sB)", ErrSizeUnit, value, value, value, value)
	}

	return uint64(size), nil // #nosec G115 -- human.ParseSize's grammar has no sign.
}

// FormatSize writes size as a count of bytes with its unit, which ParseSize
// reads back exactly.
func FormatSize(size uint64) string {
	return strconv.FormatUint(size, decimalBase) + "B"
}
