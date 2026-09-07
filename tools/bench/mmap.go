//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// runMmap times the page-table teardown/permission/fault paths that the arm64 architecture
// features enabled in the sibling ossein-kernel's kernel/config/kernel-fragment are supposed to
// accelerate — the ones the rest of the suite is blind to, because getpid/fork/fs touch no large
// mappings. Each op times ONLY its
// target operation; the mmap/touch setup is kept out of the timer so the rows do not conflate:
//
//	munmap   — teardown of a populated region. Without FEAT_TLBIRANGE (CONFIG_ARM64_TLB_RANGE)
//	           the kernel issues a per-page TLBI loop; with it, one range invalidate. The most
//	           direct probe of TLB_RANGE. Timer wraps only Munmap; mmap+touch are excluded.
//	mprotect — flip a populated region RW->RO->RW. Same range-TLBI question on a different call.
//	fault    — first-write to freshly-mapped clean pages (the touch). FEAT_HAFDBS
//	           (CONFIG_ARM64_HW_AFDBM) sets the dirty bit in hardware; without it every
//	           first-write takes a minor fault. VZ does NOT expose HAFDBS on Apple silicon, so
//	           this row is expected to be FLAT A/B — it is here to confirm that, not to win.
//
// A/B two guest kernels (feature block on vs off); the per-op delta is what the feature buys
// under VZ. Region defaults to 64 MB = 16384 4K pages, big enough that a per-page TLBI loop is
// clearly costlier than a single range op.
func runMmap(args []string) error {
	operation := "munmap"
	if len(args) > 0 {
		operation = args[0]
		args = args[1:]
	}

	pages := int64(16384) // 64 MB at 4K

	if len(args) > 0 {
		if v, err := strconv.ParseInt(args[0], 10, 64); err == nil && v > 0 {
			pages = v
		}
	}

	iterations := int64(2000)

	if len(args) > 1 {
		if v, err := strconv.ParseInt(args[1], 10, 64); err == nil && v > 0 {
			iterations = v
		}
	}

	pageSize := int64(syscall.Getpagesize())
	length := int(pages * pageSize)

	mapRegion := func() ([]byte, error) {
		region, err := syscall.Mmap(-1, 0, length,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
		if err != nil {
			return nil, fmt.Errorf("mmap: %w", err)
		}

		return region, nil
	}
	// touchAll writes one byte per page, forcing every page present and dirty.
	touchAll := func(b []byte) {
		for i := 0; i < len(b); i += int(pageSize) {
			b[i] = 1
		}
	}

	var elapsed time.Duration

	switch operation {
	case "munmap":
		for range iterations {
			region, err := mapRegion()
			if err != nil {
				return err
			}

			touchAll(region)

			began := time.Now()

			if err := syscall.Munmap(region); err != nil {
				return fmt.Errorf("munmap: %w", err)
			}

			elapsed += time.Since(began)
		}

	case "mprotect":
		region, err := mapRegion()
		if err != nil {
			return err
		}

		defer func() { _ = syscall.Munmap(region) }()

		touchAll(region)

		start := time.Now()

		for range iterations {
			if err := syscall.Mprotect(region, syscall.PROT_READ); err != nil {
				return fmt.Errorf("mprotect ro: %w", err)
			}

			if err := syscall.Mprotect(region, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
				return fmt.Errorf("mprotect rw: %w", err)
			}
		}

		elapsed = time.Since(start)

	case "fault":
		for range iterations {
			region, err := mapRegion()
			if err != nil {
				return err
			}

			began := time.Now()

			touchAll(region)

			elapsed += time.Since(began)

			if err := syscall.Munmap(region); err != nil {
				return fmt.Errorf("munmap: %w", err)
			}
		}

	case "collateral":
		// The real TLB_RANGE probe. A large munmap exceeds the per-page-TLBI threshold, so the
		// kernel full-flushes the ASID WITHOUT FEAT_TLBIRANGE (one cheap instruction, but it
		// evicts every TLB entry) vs a bounded range invalidate WITH it (unrelated entries
		// survive). That difference is invisible in the munmap call itself — it shows up as TLB
		// refills on a working set that OUTLIVES the unmap. So: keep a small resident working
		// set (fits in the TLB), and each iteration unmap a large scratch region, then time
		// re-touching the working set. With TLB_RANGE the working set stays TLB-hot across the
		// scratch unmap; without it, the full flush forces a page-table walk per working page.
		const wsPages = 256 // 1 MB — comfortably inside the arm64 TLB

		workingSet, err := syscall.Mmap(-1, 0, int(wsPages*pageSize),
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
		if err != nil {
			return fmt.Errorf("mmap working set: %w", err)
		}

		defer func() { _ = syscall.Munmap(workingSet) }()

		touchAll(workingSet)

		var sink byte

		for range iterations {
			scratch, err := mapRegion()
			if err != nil {
				return err
			}

			touchAll(scratch)
			_ = syscall.Munmap(scratch) // full-flush (no TLB_RANGE) or range invalidate (with)
			began := time.Now()         // now pay the collateral: re-touch the surviving set

			for j := 0; j < len(workingSet); j += int(pageSize) {
				sink += workingSet[j]
			}

			elapsed += time.Since(began)
		}

		_ = sink

	default:
		return fmt.Errorf("unknown mmap op %q (want: munmap | mprotect | fault | collateral)", operation)
	}

	perop := float64(elapsed.Microseconds()) / float64(iterations)
	perpage := float64(elapsed.Nanoseconds()) / float64(iterations) / float64(pages)
	_, _ = fmt.Fprintf(os.Stdout, "mmap/%s pages=%d n=%d total=%s perop=%.1fus perpage=%.1fns\n",
		operation, pages, iterations, elapsed, perop, perpage)

	return nil
}
