//go:build arm64

// Command wfeprobe answers two questions that decide whether IPI-free polling idle
// (TIF_POLLING_NRFLAG semantics) is implementable in our guest:
//
//  1. Does the hypervisor TRAP WFE? If HCR_EL2.TWE were set, every WFE would be a VM
//     exit and polling idle would be pointless. SEVL pre-arms the event register so the
//     following WFE returns immediately: native is ~10ns, a trap would be ~us.
//  2. Does a WFE-parked CPU wake from a REMOTE STORE at spin speed? That is the whole
//     mechanism -- the wakee arms the exclusive monitor (LDAXR) and parks in WFE; a store
//     from another CPU clears the monitor and wakes it, with no IPI and no host round trip.
//
// MEASURED 2026-07-15 (M5 Pro, macOS 25.5): VZ does NOT trap WFE and the WFE park wakes
// as fast as a spin -- so the mechanism behind OrbStack's ~free cross-vCPU wakeups needs
// only guest-kernel work, not their VMM. See SCHED-PIPE-INVESTIGATION.md.
//
//	host (bare metal)  sevl+wfe 13.36ns  spin 125ns/RT  wfe 112ns/RT
//	ossein (VZ)        sevl+wfe  9.39ns  spin 116ns/RT  wfe 108ns/RT
//	orbstack (own VMM) sevl+wfe  9.43ns  spin 143ns/RT  wfe 117ns/RT
//
// This is also the CI canary: Apple documents no WFE/WFI trap behaviour, so a macOS
// update could change it. If sevl+wfe ever jumps to microseconds, polling idle is dead.
package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func sevlWfe(n int64)
func spinWait(addr *uint32, want uint32)
func wfeWait(addr *uint32, want uint32)

// pingpong bounces a flag between two OS threads pinned apart, measuring the
// round-trip of "store here -> observed there". waiter selects spin vs WFE park.
func pingpong(rounds int, wait func(*uint32, uint32)) time.Duration {
	var flag [64]uint32 // own cacheline-ish

	var waiter sync.WaitGroup

	start := make(chan struct{})

	waiter.Go(func() {
		runtime.LockOSThread()

		<-start

		for i := 1; i <= rounds; i++ {
			wait(&flag[0], uint32(i*2-1))             // wait for ping
			atomic.StoreUint32(&flag[0], uint32(i*2)) // pong
		}
	})

	runtime.LockOSThread()
	close(start)

	began := time.Now()

	for i := 1; i <= rounds; i++ {
		atomic.StoreUint32(&flag[0], uint32(i*2-1)) // ping
		wait(&flag[0], uint32(i*2))                 // wait for pong
	}

	d := time.Since(began)

	waiter.Wait()

	return d / time.Duration(rounds)
}

func main() {
	_, _ = fmt.Fprintf(os.Stdout, "cpus=%d\n", runtime.NumCPU())

	const wfeOps = 5_000_000

	// picosPerNano/trapThresholdNs: report in ns with 3 decimals, and call the
	// hypervisor "trapping" when one WFE costs ≥100ns (native is ~10ns).
	const picosPerNano = 1000

	began := time.Now()

	sevlWfe(wfeOps)
	per := time.Since(began).Nanoseconds() * picosPerNano / wfeOps // picoseconds
	_, _ = fmt.Fprintf(os.Stdout, "  sevl+wfe      : %6.2f ns/op   %s\n", float64(per)/picosPerNano,
		verdict(float64(per)/picosPerNano))

	const rounds = 200_000

	_, _ = fmt.Fprintf(os.Stdout, "  pingpong spin : %6.1f ns/RT\n", float64(pingpong(rounds, spinWait).Nanoseconds()))
	_, _ = fmt.Fprintf(os.Stdout, "  pingpong wfe  : %6.1f ns/RT\n", float64(pingpong(rounds, wfeWait).Nanoseconds()))
}

func verdict(ns float64) string {
	const trapThresholdNs = 100
	if ns < trapThresholdNs {
		return "-> WFE executes NATIVELY (no trap/exit)"
	}

	return "-> WFE appears TRAPPED (hypervisor exit per op)"
}
