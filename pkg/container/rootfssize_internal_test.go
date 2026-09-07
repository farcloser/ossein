//go:build darwin && arm64

package container

import "testing"

func TestRootfsSizeFromMemory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mem, want uint64
	}{
		{512, 256},   // below the reserve: half
		{1024, 512},  // at the reserve boundary: half
		{1100, 550},  // just above: half, NOT mem-reserve (the old 76 MiB cliff)
		{2048, 1024}, // crossover: both rules agree
		{4096, 3072}, // the default: mem - reserve, unchanged from before
		{8192, 7168}, // large: mem - reserve
		{0, 0},       // degenerate; Boot substitutes the default before calling
	}

	for _, tc := range cases {
		if got := rootfsSizeFromMemory(tc.mem); got != tc.want {
			t.Errorf("rootfsSizeFromMemory(%d) = %d, want %d", tc.mem, got, tc.want)
		}
	}

	// Monotonic: more memory must never mean less rootfs. Walk the whole
	// relevant range at MiB granularity — this is exactly the property the
	// old mem-reserve cliff violated at 1024→1100.
	prev := uint64(0)

	for mem := uint64(1); mem <= 16384; mem++ {
		got := rootfsSizeFromMemory(mem)
		if got < prev {
			t.Fatalf("non-monotonic: rootfsSizeFromMemory(%d) = %d < %d at mem-1", mem, got, prev)
		}

		prev = got
	}
}
