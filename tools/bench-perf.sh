#!/usr/bin/env bash
# Cross-runtime `perf bench` comparison. The SAME pinned perf binary — built from our
# kernel's in-tree tools/perf with every optional lib off, so `perf bench` is pure libc —
# is mounted into every runtime and run on whatever kernel that runtime provides. It
# measures the kernel/userspace primitives containers actually hit:
#   sched-pipe       context-switch latency (pipe ping-pong)      usecs/op  lower
#   syscall-basic    raw syscall latency (getppid loop)           sec/10M   lower
#   syscall-fork     process creation (THE container-start path)  sec/10k   lower
#   syscall-execve   program exec  (THE container-start path)     sec/10k   lower
#   sched-messaging  scheduler throughput under load (hackbench)  sec       lower
#   futex-hash       futex op throughput (glibc/pthreads/Go)      Mops/sec  higher
#   epoll-wait       short-sleep granularity — NOT epoll, see below  Mops/sec  higher
#   mem-memcpy       memory bandwidth                             GB/sec    higher
#
# epoll-wait DOES NOT MEASURE EPOLL, and its name is the only reason anyone thinks it does.
# The bench's writer thread feeds every worker — 192 eventfds (nthreads x nfds), then
# `nanosleep(500ns)` — and that sleep, not epoll, sets the ceiling: throughput comes out at
# (nthreads x nfds) ops per sleep. A kernel whose short sleeps round up to a timer tick
# starves the workers and posts a number ~8x lower while its epoll is fine. Measured
# 2026-07-15 (EPOLL-TIMER.md): a 500ns sleep costs us ~54us (the 50us default timer slack;
# CONFIG_HIGH_RES_TIMERS=y) and OrbStack ~925us (tick-granular, ~HZ=1000) — so we post
# ~550k and they post ~66k, and NEITHER number is about epoll. We are consumer-bound at our
# real ceiling (3 workers / 1.8us per op = 1.67M ops/s); they are writer-bound at 192/ms.
# The row still earns its place — sub-ms sleep granularity is a real kernel property that
# real workloads pay for (short poll timeouts, backoff loops, Go/Node timers) — but read it
# as THAT, and never as evidence about epoll or about a runtime's IO path.
# Median of RUNS, with the OBSERVED spread, each side reported separately: `0.081 +4%/-2%`
# means the slowest of the RUNS samples came in 4% above the median and the fastest 2% below.
# This row's measured noise floor, not an estimate. The two sides together span what a single
# +-N% used to state: on a quiet host, ~2-6% total for the usecs/op benches, ~5-10% for
# sched-messaging (it is short and fork-heavy).
# The sides are NOT expected to match, and the asymmetry is the point: a bench can only get so
# fast, so the minus side is close to the floor of what the machine can do, while anything
# stealing the host stretches the plus side without touching it. A fat +N% next to a thin -M%
# is interference, not a slower kernel — the median is still the reading. Both sides fat means
# the row is genuinely unstable.
# RULE OF THUMB: a gap smaller than the two rows' spreads is not a result. If every row's
# spread inflates at once, the host is busy — stop and re-run rather than believe it.
# The preflight prints each target's KERNEL VERSION: this suite compares guest kernels, so
# that is the primary variable and it should never be inferred.
# `perf bench` is userspace + unprivileged (no PMU, no perf_event_open), so it runs in an
# ordinary container with no added caps — a fair cross-runtime probe.
#
# Every runtime is pinned to CPUS *visible* CPUs; the per-runtime pinning rationale
# (docker --cpuset mask, apple-container's +1 mgmt vCPU, rootless podman/lima needing a
# CPUS-wide machine because cpuset isn't delegated) is the same as bench-external.sh —
# this reuses it verbatim and adds lima (whose in-VM CLI is detected, not assumed).
#
# NOT a boot / end-to-end comparison (shared-VM vs per-VM + re-extraction).
#
# ossein appears TWICE when an unpatched kernel is supplied: same binary, same initfs, same
# config — kernel/patches/ is the only variable, so the delta between the two rows is exactly
# what our patches buy (and would expose one that costs). Build it with `just kernel-nopatch`.
#
# usage: bench-perf.sh <ossein> <initfs> <kernel> [nopatch-kernel] [runs]
set -uo pipefail

OSSEIN="${1:?usage: bench-perf.sh <ossein> <initfs> <kernel> [nopatch-kernel] [runs]}"
INITFS="$2"
K="$3"
KNOPATCH="${4:-}"      # optional; row is skipped when absent/missing
RUNS="${5:-5}"

# BENCH_FOCUS=1 (or `just bench-perf-focus`): only the rows that matter while tuning OUR kernel
# — both ossein kernels (the patched/nopatch A/B, i.e. what our changes did) and orbstack
# (the reference to beat). Drops apple-container/docker-desktop/podman/lima, which are
# context rather than feedback and cost most of the wall-clock. ~4min instead of ~15.
FOCUS="${BENCH_FOCUS:-}"
focus_skip() { [ -n "$FOCUS" ]; }

CPUS=4                       # visible CPUs held equal across every runtime
CPUSET="0-$((CPUS - 1))"     # docker affinity mask -> nproc == CPUS
CCPUS=$((CPUS - 1))          # apple-container runs requested+1 vCPUs -> --cpus $CCPUS == CPUS

HERE="$(cd "$(dirname "$0")/.." && pwd)"
B="$HERE/build"
IMG="docker.io/library/debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"
PERF=/bench/perf-arm64       # $B is mounted at /bench in every runtime; perf lives there

[ -x "$B/perf-arm64" ] || { echo "missing $B/perf-arm64 (build it first: just kernel)" >&2; exit 1; }
for f in "$OSSEIN" "$INITFS" "$K"; do [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }; done

# name : perf-bench-args : unit : better-direction : parse-mode (see pval)
#
# -r 3: futex/epoll default to a 10s/8s fixed runtime each — at median x RUNS x every runtime
# that dominates wall-clock, and their reported stddev is ~0.02%, so 3s is plenty.
# -s 64MB: mem memcpy's 1MB default completes in ~16us against a whole-microsecond timer, so
# the result quantizes into a handful of discrete values (four runtimes tie to 6 decimals).
# 64MB takes ~1ms -> ~0.1% resolution instead of ~6%.
# UNITS ARE NOT FREE — `-f simple` prints ONE number and it is not always the one the bench's
# default output highlights. For sched/syscall it is TOTAL SECONDS, not usecs/op:
#   sched-pipe     -l defaults to 1e6, so total-secs and usecs/op are NUMERICALLY EQUAL.
#                  The usecs/op label is right BY LUCK; pass -l and it silently becomes a lie.
#   syscall-basic  -l defaults to 1e7, so total-secs is 10x the usecs/op. Labelled in secs
#                  here because that is what the number IS (per-op = this / 10, ~0.088us).
# Both are lower-is-better and every runtime runs the same -l, so cross-runtime comparison was
# never affected — only the unit on the page. Check this again if you ever add a row or an -l.
PBENCHES=(
  "sched-messaging:sched messaging:sec:lower:last"
  "sched-pipe:sched pipe:usecs/op:lower:last"
  "syscall-basic:syscall basic:sec/10M calls:lower:last"
  # fork+execve ARE the container-startup path — the thing ossein exists to make fast
  # (OSSEIN-PERF.md) — and they are fork/slab-heavy, the ground SLUB_TINY cost us six months on
  # (SCHED-MESS.md). Nothing else in this suite covers process creation. First readings
  # 2026-07-15: fork 139 us/op vs 200 for BOTH nopatch and orbstack — our patches buy ~31% that
  # no other row credits; execve 346 vs orbstack's 295 — a ~15% gap where THEY win, on the path
  # we care most about. Both default to -l 10000 (the bench caps it itself "to save time"), so
  # no -l here — and note -f simple prints TOTAL SECONDS at ms resolution, hence the units.
  # fork is noisy run-to-run (70-139 us/op observed); that is what median x RUNS + the spread
  # column are for. Read the spread before believing any fork delta.
  "syscall-fork:syscall fork:sec/10k forks:lower:last"
  "syscall-execve:syscall execve:sec/10k execs:lower:last"
  "futex-hash:futex hash -r 3:Mops/sec:higher:avgops"
  # Named for what it measures, not for the perf sub-command it runs — the writer's
  # nanosleep(500ns) is the ceiling here, not epoll. See the header + EPOLL-TIMER.md.
  "epoll-wait (short-sleep granularity, NOT epoll):epoll wait -r 3:Mops/sec:higher:avgops"
  "mem-memcpy:mem memcpy -s 64MB:GB/sec:higher:bps"
)

# Rootless shared-VM runtimes: can't pin with --cpuset-cpus (the cpuset controller isn't
# delegated to the user cgroup), so the MACHINE/VM must be CPUS-wide. The preflight reads
# nproc and only benches them if it equals CPUS (else the numbers are on the wrong count).
PODMAN=(podman)
PODMAN_NP=""
podman_avail() { command -v podman >/dev/null 2>&1; }
LIMA_NP=""
LIMA_CLI=""    # container CLI *inside* the lima VM; template-dependent, detected in the preflight
lima_avail()   { command -v lima   >/dev/null 2>&1; }   # `lima <cli>` runs <cli> in the default VM

# Docker-CLI runtimes: <label>=<docker-context>. ONE real docker CLI (resolved from PATH)
# targets BOTH daemons — --context selects which, so no per-runtime binary is needed. It MUST
# be explicit: OrbStack hijacks the default endpoint, so without --context the CLI hits OrbStack.
DOCKER="$(command -v docker 2>/dev/null || true)"
RUNTIMES=( "orbstack=orbstack" "docker-desktop=desktop-linux" )
focus_skip && RUNTIMES=( "orbstack=orbstack" )
KVER_SEEN=""

median() { sort -n | awk '{a[NR]=$1} END{print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}'; }

# Extract the result. `-f simple` is NOT honored uniformly, so this is per-bench ($PARSE, set
# by the bench loop). Taking "the last number" is a trap: futex/epoll ignore -f simple entirely
# and their LAST line is "total secs = 8" (a fixed param) or "Futex hashing: auto resized to 16
# buckets" (a bucket count) — both constants that masquerade as measurements.
#   last   -f simple honored: the output IS the number (usecs/op, sec).
#   avgops -f simple ignored: parse "Averaged <N> operations/sec" -> Mops/sec.
#   bps    -f simple honored but prints raw BYTES/sec (perf's own "GB/sec" is /2^30) -> GB/sec.
pval() {  # (perf output on stdin) -> number; $PARSE selects the extractor
  local out; out=$(cat)
  case "$PARSE" in
    avgops) printf '%s\n' "$out" | sed -n 's/.*Averaged *\([0-9][0-9]*\) *operations\/sec.*/\1/p' \
              | tail -n1 | awk 'NF{printf "%.2f\n", $1/1000000}' ;;
    bps)    printf '%s\n' "$out" | grep -oE '^[0-9]+(\.[0-9]+)?$' \
              | tail -n1 | awk 'NF{printf "%.2f\n", $1/1000000000}' ;;
    *)      printf '%s\n' "$out" | grep -oE '[0-9]+(\.[0-9]+)?' | tail -n1 ;;
  esac
}

# -> "<median> <spread>", where spread is the preformatted "+u%/-d%" token: how far ABOVE the
# median the max sits and how far BELOW it the min sits, over the RUNS samples we already take.
# The run-to-run noise floor OF THIS ROW, measured rather than assumed. Read it before believing
# any delta — a 3% gap between two rows spanning 8% of noise is not a result. It also catches a
# disturbed host: if every row's spread inflates at once, something else is running.
prun_med() {
  local vals=() out v med spread
  for _ in $(seq "$RUNS"); do
    out=$("$@" 2>/dev/null)
    v=$(printf '%s' "$out" | pval)
    if [ -n "$v" ]; then vals+=("$v"); else
      echo "  warn: no result from: $* — run dropped from median" >&2
    fi
  done
  [ ${#vals[@]} -eq 0 ] && { echo "ERR -"; return; }
  med=$(printf '%s\n' "${vals[@]}" | median)
  # Each side separately, not one +-: run-to-run noise here is ASYMMETRIC — a bench can only
  # get so fast, but anything stealing the host makes a single run arbitrarily slow, so the
  # max drifts far while the min barely moves. Collapsing that into one number reads as
  # symmetric jitter and hides which side is fat. One token (no spaces) — callers `read` it.
  spread=$(printf '%s\n' "${vals[@]}" | awk -v m="$med" '
    NR==1 {mn=mx=$1}
    {if ($1<mn) mn=$1; if ($1>mx) mx=$1}
    END {if (m+0>0) printf "+%.0f%%/-%.0f%%", (mx-m)/m*100, (m-mn)/m*100; else printf "?"}')
  echo "$med $spread"
}

row() { printf "  %-30s %14s %s\n" "$1" "$2" "$3"; }

# Every target reports the kernel it actually ran, not the one we think it runs: the whole
# suite compares GUEST KERNELS, so the version is the primary variable and belongs in the
# report. (It has already caught a mismatch once — see the ossein/nopatch row.)
kernel_of() { "$@" uname -r 2>/dev/null | tr -d '[:space:]'; }

# lima hardcodes `Ciphers=^aes128-gcm@openssh.com,aes256-gcm@openssh.com` (AES-GCM for
# throughput on AES-accelerated hardware). An OpenSSH built WITHOUT OpenSSL has no AES-GCM at
# all — only aes*-ctr + chacha20 — so it rejects that spec outright ("Bad SSH2 cipher spec")
# and lima dies before it ever connects. Homebrew ships such a build, and it shadows the
# AES-GCM-capable /usr/bin/ssh on PATH.
# Shim ONLY ssh (one symlink in a temp dir, prepended) rather than prepending /usr/bin wholesale
# — that would also shadow homebrew's git/python/etc — so podman/lima/nerdctl still resolve from
# homebrew. No-op when PATH's ssh already does AES-GCM.
if ! focus_skip && lima_avail && ! ssh -Q cipher 2>/dev/null | grep -q 'aes128-gcm@openssh.com'; then
  if /usr/bin/ssh -Q cipher 2>/dev/null | grep -q 'aes128-gcm@openssh.com'; then
    SSH_SHIM="$(mktemp -d)"; ln -s /usr/bin/ssh "$SSH_SHIM/ssh"
    export PATH="$SSH_SHIM:$PATH"
    trap 'rm -rf "$SSH_SHIM"' EXIT
    echo "note: PATH ssh has no AES-GCM (OpenSSH built without OpenSSL?) — using /usr/bin/ssh for lima only" >&2
  else
    echo "warn: no AES-GCM-capable ssh on PATH or in /usr/bin — lima will fail to connect" >&2
  fi
fi

# --- WFE canary (SCHED-RESPONSE.md 3.5) ---
# kernel/patches/0002 (polling idle) is viable ONLY because VZ does not trap WFE: the idle
# CPU parks in monitor-armed WFE waiting for a remote store, and wakers elide the IPI. Apple
# documents NO WFI/WFE trap behaviour — it is empirical, per-stack, and a macOS update could
# change it. If it ever does, the patch silently becomes a pessimization: every idle poll
# turns into a VM exit. So check it on every run rather than trusting a number from the day
# it was written. Native is ~10ns/op; a trap is microseconds.
if [ -x "$B/wfeprobe" ]; then
  wfe_ns=$(OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" \
             -v "$B:/bench" "$IMG" /bench/wfeprobe 2>/dev/null \
           | sed -n 's/.*sevl+wfe *: *\([0-9.]*\) ns.*/\1/p')
  if [ -z "$wfe_ns" ]; then
    echo "warn: wfeprobe produced no result — WFE canary not checked" >&2
  # Both verdicts go to STDOUT, with the bench numbers they guard — a canary whose alarm lands
  # in a different stream than the result it invalidates is one you read the numbers without.
  # No `===` on the pass line: that decoration means "section header" everywhere else here, and
  # a one-line verdict wearing it reads as a section that printed nothing.
  elif awk -v n="$wfe_ns" 'BEGIN{exit !(n < 100)}'; then
    echo "WFE canary: ${wfe_ns} ns/op — native, VZ does not trap WFE (polling idle valid)"
  else
    echo "!! WFE CANARY FAILED: sevl+wfe = ${wfe_ns} ns/op (expected <100)."
    echo "!! The hypervisor now appears to TRAP WFE. kernel/patches/0002 (polling idle) turns"
    echo "!! every idle poll into a VM exit — REVERT IT and re-measure. See SCHED-PIPE-INVESTIGATION.md."
  fi
else
  echo "note: no $B/wfeprobe — WFE canary skipped (build it: just build-wfeprobe)" >&2
fi
echo

# --- fairness preflight: how many CPUs does each runtime actually expose? ---
echo "=== nproc preflight (must all read $CPUS for a fair comparison) ==="
np_ossein=$(OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
kv_ossein=$(kernel_of env OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG")
row "ossein (--cpus $CPUS)" "${np_ossein:-?}" "cpu  kernel=${kv_ossein:-?}"
if [ -n "$KNOPATCH" ]; then
  if [ -e "$KNOPATCH" ]; then
    np_np=$(OSSEIN_KERNEL="$KNOPATCH" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
    kv_np=$(kernel_of env OSSEIN_KERNEL="$KNOPATCH" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG")
    row "ossein/nopatch (--cpus $CPUS)" "${np_np:-?}" "cpu  kernel=${kv_np:-?}"
  else
    row "ossein/nopatch" "(absent)" "SKIPPED — build it: just kernel-nopatch"
  fi
fi
if ! focus_skip && command -v container >/dev/null 2>&1; then
  npc=$(container run --rm --cpus "$CCPUS" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  kvc=$(kernel_of container run --rm --cpus "$CCPUS" "$IMG")
  row "apple-container (--cpus $CCPUS)" "${npc:-?}" "cpu  kernel=${kvc:-?}"
fi
for spec in "${RUNTIMES[@]}"; do
  IFS='=' read -r label ctx <<<"$spec"
  [ -n "$DOCKER" ] || { row "$label" "(no docker CLI on PATH)" ""; continue; }
  kver=$("$DOCKER" --context "$ctx" run --rm "$IMG" uname -r 2>/dev/null | tr -d '[:space:]')
  if [ -z "$kver" ]; then row "$label (ctx=$ctx)" "(unreachable)" "not running / bad context"; continue; fi
  np=$("$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  row "$label (ctx=$ctx, --cpuset $CPUSET)" "${np:-?}" "cpu  kernel=$kver"
  case "$KVER_SEEN" in *"|$kver|"*)
    echo "  !! WARNING: '$kver' already seen — $label resolves to the SAME daemon as another runtime; its numbers are a DUPLICATE." >&2 ;;
  esac
  KVER_SEEN="$KVER_SEEN|$kver|"
done
if ! focus_skip && podman_avail; then
  PODMAN_NP=$("${PODMAN[@]}" run --rm "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  if [ "$PODMAN_NP" = "$CPUS" ]; then row "podman (rootless; machine=$CPUS vCPU)" "$PODMAN_NP" "cpu  kernel=$(kernel_of "${PODMAN[@]}" run --rm "$IMG")"
  else row "podman (rootless)" "${PODMAN_NP:-?}" "cpu — need machine=$CPUS ('podman machine set --cpus $CPUS'); SKIPPED"; fi
fi
if ! focus_skip && lima_avail; then
  # WHICH container CLI lives in the VM is template-dependent — the docker/podman templates ship
  # no nerdctl at all — so detect it instead of assuming. All three take the same
  # `run --rm -v --security-opt` flags, so the bench rows work unchanged whichever we find.
  for c in nerdctl docker podman; do
    if lima "$c" --version >/dev/null 2>&1; then LIMA_CLI="$c"; break; fi
  done
  if [ -z "$LIMA_CLI" ]; then
    row "lima" "(no CLI)" "SKIPPED — no nerdctl/docker/podman inside the lima VM"
  else
    # `-v` binds a host path only if lima mounts it into the VM (default: ~ and /tmp/lima).
    # build/ under $HOME is reachable read-only, which is all perf needs (read+exec).
    lima_err="$(lima "$LIMA_CLI" run --rm "$IMG" nproc 2>&1 >/dev/null | tr '\n' ' ')"
    LIMA_NP=$(lima "$LIMA_CLI" run --rm "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
    if [ "$LIMA_NP" = "$CPUS" ]; then
      row "lima/$LIMA_CLI (rootless; VM=$CPUS vCPU)" "$LIMA_NP" "cpu  kernel=$(kernel_of lima "$LIMA_CLI" run --rm "$IMG")"
    elif [ -z "$LIMA_NP" ]; then
      # Distinguish "probe errored" from "wrong vCPU count" — reporting the latter for the former
      # sends you off resizing a VM when the real fault is the instance being unreachable.
      row "lima/$LIMA_CLI (rootless)" "(unreachable)" "SKIPPED — probe failed: ${lima_err:-no output}"
    else
      row "lima/$LIMA_CLI (rootless)" "$LIMA_NP" "cpu — need VM=$CPUS vCPUs (limactl stop + edit --cpus); SKIPPED"
    fi
  fi
fi

for spec in "${PBENCHES[@]}"; do
  IFS=: read -r name pargs unit better PARSE <<<"$spec"   # PARSE is global: pval reads it
  echo
  echo "=== $name — perf bench $pargs ($better is better, $unit; median x$RUNS) ==="
  # shellcheck disable=SC2086  # intentional word-split of the perf sub-args (e.g. "sched pipe")
  set -- bench -f simple $pargs

  read -r v sp <<<"$(prun_med env OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" -v "$B:/bench" "$IMG" "$PERF" "$@")"
  row "ossein" "$v" "${sp}  $unit"

  # Same binary/initfs/config — kernel/patches/ is the only difference, so this row IS the
  # cost/benefit of our patches. Skipped unless `just kernel-nopatch` has been run.
  if [ -n "$KNOPATCH" ] && [ -e "$KNOPATCH" ]; then
    read -r v sp <<<"$(prun_med env OSSEIN_KERNEL="$KNOPATCH" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" -v "$B:/bench" "$IMG" "$PERF" "$@")"
    row "ossein/nopatch" "$v" "${sp}  $unit"
  fi

  if ! focus_skip && command -v container >/dev/null 2>&1; then
    read -r v sp <<<"$(prun_med container run --rm --cpus "$CCPUS" --volume "$B:/bench" "$IMG" "$PERF" "$@")"
    row "apple-container" "$v" "${sp}  $unit"
  fi

  for rspec in "${RUNTIMES[@]}"; do
    IFS='=' read -r label ctx <<<"$rspec"
    [ -n "$DOCKER" ] || { row "$label" "(no docker CLI on PATH)" ""; continue; }
    read -r on ons <<<"$(prun_med  "$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET"                                   -v "$B:/bench" "$IMG" "$PERF" "$@")"
    read -r off offs <<<"$(prun_med "$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET" --security-opt seccomp=unconfined -v "$B:/bench" "$IMG" "$PERF" "$@")"
    row "$label (seccomp on)"  "$on"  "${ons}  $unit"
    row "$label (seccomp off)" "$off" "${offs}  $unit"
  done

  if ! focus_skip && podman_avail && [ "$PODMAN_NP" = "$CPUS" ]; then
    read -r on ons <<<"$(prun_med  "${PODMAN[@]}" run --rm                                   -v "$B:/bench" "$IMG" "$PERF" "$@")"
    read -r off offs <<<"$(prun_med "${PODMAN[@]}" run --rm --security-opt seccomp=unconfined -v "$B:/bench" "$IMG" "$PERF" "$@")"
    row "podman (seccomp on)"  "$on"  "${ons}  $unit"
    row "podman (seccomp off)" "$off" "${offs}  $unit"
  fi

  if ! focus_skip && [ -n "$LIMA_CLI" ] && [ "$LIMA_NP" = "$CPUS" ]; then
    read -r on ons <<<"$(prun_med  lima "$LIMA_CLI" run --rm                                   -v "$B:/bench" "$IMG" "$PERF" "$@")"
    read -r off offs <<<"$(prun_med lima "$LIMA_CLI" run --rm --security-opt seccomp=unconfined -v "$B:/bench" "$IMG" "$PERF" "$@")"
    row "lima/$LIMA_CLI (seccomp on)"  "$on"  "${ons}  $unit"
    row "lima/$LIMA_CLI (seccomp off)" "$off" "${offs}  $unit"
  fi
done
