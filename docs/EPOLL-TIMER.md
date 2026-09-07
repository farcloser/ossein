# epoll-wait measures timer granularity, not epoll — and OrbStack has none

> **MEASURED 2026-07-15**, this host (18-core Apple Silicon, macOS 25.5), same pinned
> `build/perf-arm64` mounted into every runtime, `--cpus 4` / `--cpuset-cpus=0-3` throughout.
> Acted on: `tools/bench-perf.sh` now names the row for what it measures.

**TL;DR — OrbStack is not bad at epoll. `perf bench epoll wait` is rate-limited by a
`nanosleep(500ns)` in its own writer thread, and OrbStack's guest rounds every sub-millisecond
sleep up to ~1 ms.** That starves the event producer, so the row reports ~66k against our ~550k
— an 8.4x gap in which epoll plays no part. Our win is real but it is a win on *short-sleep
granularity* (`CONFIG_HIGH_RES_TIMERS=y`), not on epoll, and the row's old name said otherwise.

## 1. It is not general slowness

Same binary, same 4 CPUs. OrbStack is within ~20% everywhere else and 8.4x off on one row:

| bench | ossein | orbstack | ratio |
|---|---|---|---|
| syscall basic | 0.085 us/op | 0.102 us/op | 1.20x |
| sched pipe | 0.620 us/op | 0.931 us/op | 1.50x |
| futex hash | 6.30 M ops/s | 5.86 M ops/s | 1.08x |
| **epoll wait** | **551 k ops/s** | **66 k ops/s** | **8.36x** |

> **Unit note.** `perf bench -f simple syscall basic` prints TOTAL SECONDS for its 1e7-call
> loop, not usecs/op — so the raw readings (0.846 / 1.018) are seconds and the per-op costs
> are a tenth of that. `bench-perf.sh` labelled that row `usecs/op` and now says `sec/10M
> calls`. The ratio, and every cross-runtime conclusion, is unchanged.

Ruled out by measurement, not by argument:

- **seccomp** — orbstack seccomp on 241,452 vs off 239,846 ops/s. No effect.
- **thread-count skew** — the bench sizes itself from `/sys/devices/system/cpu/online`
  (`perf_cpu_map__new_online_cpus()` → `nthreads = nr - 1`), *not* from the cpuset, so a
  runtime exposing more online CPUs than its cpuset would silently spawn more workers and
  post a lower per-thread average. OrbStack masks sysfs to the cpuset (`online = 0-3`), so
  both sides ran **3 workers**. Worth knowing anyway: the `nproc` preflight and the bench
  read different sources of truth, so the preflight cannot catch this.
- **clocksource** — `arch_sys_counter` on both.

## 2. What the bench actually does

One op is two syscalls, no batching (`tools/perf/bench/epoll-wait.c`):

```c
ret = epoll_wait(efd, &ev, 1, to);   /* maxevents=1, to=-1 */
r   = read(fd, &val, sizeof(val));
```

The per-op costs indict the asymmetry before any hypothesis:

- **ossein**: 551k ops/s = **1.8 us/op** per worker.
- **orbstack**: 66k ops/s = **15.2 us/op** — **8.4x ossein's**, while its raw syscall cost is
  only 1.2x ours and its pipe and futex numbers are within 1.5x. Nothing OrbStack does per
  syscall or per wakeup can fund a 13.4 us/op gap.

(Neither side is bound by raw syscall entry: two syscalls cost ~0.19 us, a tenth of ossein's
1.8 us. The rest is epoll's own machinery plus contention on the single shared queue. That is
fine — it is the *ratio* that has to be explained, and per-syscall cost cannot explain it.)

The missing time is not in the workers. It is in the writer thread that feeds them:

```c
struct timespec ts = { .tv_sec = 0, .tv_nsec = 500 };   /* 500 ns */
for (iter = 0; !wdone; iter++) {
    for (i = 0; i < nthreads; i++)
        for (j = 0; j < nfds; j++)
            write(w->fdmap[j], &val, sizeof(val));      /* nthreads x nfds = 192 events */
    nanosleep(&ts, NULL);
}
```

Every worker op consumes one of those writes. So **the writer's loop rate is the ceiling on
the entire benchmark**, and one `nanosleep` sits in that loop.

## 3. The prediction, and the test that could have refuted it

If OrbStack is sleep-bound, giving the writer more fds per loop amortizes the fixed sleep and
throughput scales. If ossein is consumer-bound, it will not move. Per-thread ops/s:

| nfds | ossein | orbstack |
|---|---|---|
| 64 | 534 k | 70 k |
| 256 (4x) | 469 k | **257 k (3.7x)** |
| 1024 (16x) | (see §6) | 374 k — saturating |

OrbStack scales ~linearly, then stops at 1024 — exactly when 3072 write syscalls (~1.8 ms)
overtake the ~1 ms sleep and it becomes write-bound instead. ossein does not scale at all; it
drops slightly as the writer starts competing for CPU.

## 4. The mechanism, measured directly

Inference from throughput got the *shape* right (a large fixed per-loop cost) and the
*mechanism* wrong: modelling the loop as `writes + fixed 1 ms sleep` yields inconsistent
sleeps (718 us at nfds=64, 214 us at 256, **negative** at 1024) and a 913 us loop sitting
*below* a supposed 1 ms floor. So measure the sleep instead of backing it out (§7):

| requested | ossein | orbstack |
|---|---|---|
| 500 ns | **54 us** | **925 us** |
| 1 us | 54.7 us | 998 us |
| 10 us | 68.9 us | 999 us |
| 100 us | 186 us | 999 us |
| 1 ms | 1.32 ms | **1.996 ms** |

(median of 300, `clock_nanosleep(CLOCK_REALTIME, 0, ...)` — what glibc's `nanosleep()` issues
on arm64.)

- **OrbStack collapses every sub-ms request to ~1 ms**, and 1 ms to ~2 ms: round up to the
  next tick, plus a tick of guarantee. Textbook **jiffy-granular timers, ~HZ=1000, high-res
  timers unavailable or inactive** in that guest.
- **ossein lands at ~54 us** — that is the default **50 us `timer_slack_ns`**, not a tick.
  `CONFIG_HIGH_RES_TIMERS=y` is doing the work. Note we run `CONFIG_HZ_250`: a 4 ms tick. If
  we ever lost high-res timers we would not merely match OrbStack here, we would post ~4x
  *worse* than they do. This row is a live check on that.

With the real numbers the model closes to within a few percent:

| | writer loop | ops/loop | predicted | observed |
|---|---|---|---|---|
| orbstack, nfds=64 | ~925 us (all sleep) | 192 | 192/0.925ms /3 = **69k** | **70,092** |
| orbstack, nfds=256 | ~999 us (all sleep) | 768 | 768/0.999ms /3 = **256k** | **256,971** |
| orbstack, nfds=1024 | ~2.7 ms (writes dominate) | 3072 | **375k** | **374,158** |
| ossein, nfds=64 | 120 us (writer keeps up) | — | 3 workers / 1.8us = **1.67M** total | **1.60M** total |

OrbStack's workers are not slow, and §3 proves it on OrbStack's own numbers rather than by
extrapolation: fed enough fds (nfds=1024) they reach **1.12M ops/s** total. At the default
nfds=64 they deliver 210k — **~19% of their own demonstrated capability**. The other 81% is
spent waiting on a sleeping writer.

**Why would OrbStack ship that?** Unknown — their kernel config is not public and
`/proc/timer_list` is not exposed in their guest, so the tick-granularity conclusion is
inference from the quantization (strong: 500 ns, 1 us, 10 us, 50 us and 100 us all landing on
999 us is hard to read another way). A *plausible* motive is their idle-wakeup optimization —
coarser timers mean fewer timer interrupts and fewer VM exits, which is the metric they
advertise (SCHED-RESPONSE.md §5, "1-5 wakeups/s idle"). Treat the motive as speculation and
the quantization as measured.

## 5. When this matters

**For epoll itself: essentially never.** The dominant real epoll workload is a server blocking
in `epoll_wait(-1)` woken by network IO. Timer granularity is irrelevant there — and this
bench deliberately avoids that path: the writer keeps fds hot so workers *don't* block
("minimizing each thread's chances of epoll_wait not finding any ready read events and
blocking as this is not what we want to stress" — its own header).

**For short sleeps: a lot, and it is a real property of a real runtime.** On OrbStack anything
pacing itself under a millisecond quantizes to ~1 ms: `usleep`/`nanosleep` < 1 ms, `poll` /
`select` / `epoll_wait` with 0-1 ms timeouts, timerfd, spin-then-sleep backoff, rate limiters,
Go's `time.Sleep` and netpoll deadlines, Node timers. A loop expecting `usleep(100)` gets
~1 ms: 10x its intended latency. Tight-timing test suites will lie.

Caveat in our favour, stated honestly: our ~54 us is *also* not honouring 500 ns. Code needing
real precision uses absolute timers or `PR_SET_TIMERSLACK`. We are 17x better here, not correct.

## 6. Side finding — we cannot run this row at high nfds

`perf bench epoll wait -f 1024` fails under ossein with `setrlimit: Operation not permitted`.
The bench raises `RLIMIT_NOFILE` to `nfds x nthreads x 2 + 50` (6194); our guest's hard limit
is lower and we grant no `CAP_SYS_RESOURCE`. Docker/OrbStack default to a huge nofile limit,
so it works there. It does not affect the default `-f 64` row, but it is an asymmetry in what
we are *able* to measure, and worth fixing if we ever want the nfds sweep in CI.

## 7. Reproduce

Cross-runtime rows (`$B` = `build/`, mounted at `/bench`):

```
build/ossein run --cpus 4 --mount "$PWD/build:/bench" debian \
  /bench/perf-arm64 bench -f simple epoll wait -r 3 [-f <nfds>]
docker --context orbstack run --rm --cpuset-cpus=0-3 -v "$PWD/build:/bench" debian \
  /bench/perf-arm64 bench -f simple epoll wait -r 3 [-f <nfds>]
```

The sleep probe of §4 is not in the tree (built ad hoc, `CGO_ENABLED=0 GOOS=linux
GOARCH=arm64`, run the same way as `wfeprobe`). It is ~20 lines and worth promoting to
`cmd/nanoprobe` if this ever needs to be a canary like the WFE one:

```go
package main

import ("fmt"; "runtime"; "sort"; "syscall"; "time"; "unsafe")

func sleepFor(ns int64) time.Duration {
	ts := syscall.Timespec{Sec: ns / 1e9, Nsec: ns % 1e9}
	start := time.Now()
	// clock_nanosleep(CLOCK_REALTIME=0, flags=0 (relative), &ts, NULL) — what glibc's
	// nanosleep() issues on arm64, i.e. exactly what the epoll bench's writer calls.
	syscall.Syscall6(syscall.SYS_CLOCK_NANOSLEEP, 0, 0, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	return time.Since(start)
}

func main() {
	runtime.LockOSThread()
	for _, ns := range []int64{500, 1_000, 10_000, 50_000, 100_000, 1_000_000} {
		d := make([]time.Duration, 0, 300)
		for i := 0; i < 300; i++ { d = append(d, sleepFor(ns)) }
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		fmt.Printf("%-12v min %10v  median %10v  p90 %10v\n",
			time.Duration(ns), d[0], d[len(d)/2], d[len(d)*9/10])
	}
}
```

## 8. Loose end

Two consecutive OrbStack epoll runs read **241,452** and **239,846** ops/s — 3.6x the norm —
then eight further runs all landed at 66-72k and it never recurred. Unexplained. If their
guest can occasionally do better than tick-granular sleeps, the mechanism above is incomplete
and something (a boosted timer mode? a warm path?) is intermittently available. Worth a second
look if that number is ever seen again — it is exactly the shape the `+n%/-m%` spread split in
`bench-perf.sh` was added to surface.

## References

- `tools/perf/bench/epoll-wait.c` (linux-7.1.3) — writer thread, `nanosleep(500ns)`,
  `epoll_wait(..., 1, -1)`, and the header stating that worker blocking is explicitly *not*
  what it stresses.
- `tools/lib/perf/cpumap.c` — `perf_cpu_map__new_online_cpus()` reads
  `devices/system/cpu/online`, not the cpuset.
- `ossein-kernel/kernel/config/kernel-fragment:127` — `CONFIG_HIGH_RES_TIMERS=y`; `:131` — `CONFIG_HZ_250`.
- SCHED-RESPONSE.md §2, §5 — OrbStack VMM identification and their idle-wakeup claim.
