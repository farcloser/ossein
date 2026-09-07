#!/usr/bin/env bash
# Cross-runtime comparison across the bench suite, same host CPU / same static
# binaries / same image:
#   - ossein on the optimized kernel   (per-VM, no seccomp)
#   - ossein on the baseline kernel     (isolates the kernel-config delta inline)
#   - apple-container (Apple's own VZ-based runtime — the closest architectural peer)
#   - OrbStack + Docker Desktop, each with and without their default seccomp.
#     PINNED per-runtime via `docker --context` (orbstack / desktop-linux): OrbStack
#     hijacks the default endpoint, so without an explicit context both CLIs hit
#     OrbStack. The preflight prints each daemon's uname -r and warns if two match.
#   - Podman (rootless podman machine — shared-VM, same class as Orb/Docker), with
#     and without seccomp. Rootless can't pin via --cpuset-cpus, so the machine
#     must be sized to CPUS vCPUs (podman machine set --cpus CPUS); the preflight
#     checks nproc and skips podman if it isn't CPUS-wide.
# Median of RUNS.
#
# forkexec is measured on BOTH paths, because where the exec target lives is what
# dominates the cost:
#   forkexec-rootfs — target on the container-local rootfs (tmpfs / overlayfs).
#                     The container hot path: entrypoints exec off the rootfs.
#   forkexec-virtio — target exec'd in place across the virtio-fs share (-v). Isolates
#                     virtio-fs execve latency (per-spawn host metadata round-trips).
# Compare rootfs-to-rootfs and virtio-to-virtio ACROSS runtimes — never one's
# rootfs against another's virtio.
#
# FAIRNESS: every runtime is pinned to CPUS *visible* CPUs. ossein gets CPUS real
# vCPUs; docker gets --cpuset-cpus (an affinity mask — NOT --cpus, which is only a
# CFS quota and leaves nproc reporting the full host). The preflight prints each
# runtime's nproc so you can see the counts match before trusting the numbers.
#
# NOT a boot / end-to-end comparison (shared-VM vs per-VM + re-extraction).
#
# usage: bench-external.sh <ossein> <initfs> <optimized-kernel> <baseline-kernel> [runs]
set -uo pipefail

OSSEIN="${1:?usage: bench-external.sh <ossein> <initfs> <optimized-kernel> <baseline-kernel> [runs]}"
INITFS="$2"
K="$3"
KBASE="$4"
RUNS="${5:-5}"

CPUS=4                       # visible CPUs held equal across every runtime
CPUSET="0-$((CPUS - 1))"     # docker affinity mask -> nproc == CPUS
CCPUS=$((CPUS - 1))          # apple-container runs requested+1 vCPUs (extra mgmt
                             # vCPU): --cpus $CCPUS -> nproc == CPUS. Verified.

HERE="$(cd "$(dirname "$0")/.." && pwd)"
B="$HERE/build"
IMG="docker.io/library/debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"

# Podman: rootless podman machine (shared-VM, like Orb/Docker). Uses its default
# connection; override natively with CONTAINER_HOST=unix://... if you must.
# ROOTLESS CAN'T PIN with --cpuset-cpus (the cpuset controller isn't delegated to
# the user cgroup), so we do NOT pass it. Fairness instead requires the *machine*
# to have CPUS vCPUs:  podman machine set --cpus CPUS  (stop/set/start). We read
# nproc in the preflight and only include podman if it actually equals CPUS —
# otherwise its numbers would be run on the wrong CPU count, so we skip it.
PODMAN=(podman)
PODMAN_NP=""   # machine nproc, filled by the preflight; gates the bench rows
podman_avail() { command -v podman >/dev/null 2>&1; }

# Docker-CLI runtimes: <label>=<docker-context>. ONE real docker CLI (resolved from PATH)
# targets BOTH daemons — --context selects which, so no per-runtime binary is needed. It MUST
# be explicit: OrbStack hijacks the default endpoint, so without --context the CLI talks to
# OrbStack (silently testing it twice). The preflight prints each daemon's uname -r and
# hard-warns if two contexts coincide.
DOCKER="$(command -v docker 2>/dev/null || true)"
RUNTIMES=( "orbstack=orbstack" "docker-desktop=desktop-linux" )
KVER_SEEN=""   # accumulates "|kver|" to detect the same daemon behind two contexts

# bench name -> target path + args under /bench (word-split at call site).
# fsbench-*-{rootfs,virtio} isolate virtio-fs perf: the virtio − rootfs delta on
# stat (open latency) and read (bandwidth) is the runtime's virtio-fs overhead.
BENCHES="forkexec-rootfs forkexec-virtio fileio \
         fsbench-stat-rootfs fsbench-stat-virtio fsbench-read-rootfs fsbench-read-virtio"
bench_target() {
  case "$1" in
    forkexec-rootfs)     echo "/bench/bench --type=forkexec rootfs" ;;
    forkexec-virtio)     echo "/bench/bench --type=forkexec virtio" ;;
    fileio)              echo "/bench/bench --type=fileio" ;;
    fsbench-stat-rootfs) echo "/bench/bench --type=fs rootfs stat" ;;
    fsbench-stat-virtio) echo "/bench/bench --type=fs virtio stat" ;;
    fsbench-read-rootfs) echo "/bench/bench --type=fs rootfs read" ;;
    fsbench-read-virtio) echo "/bench/bench --type=fs virtio read" ;;
  esac
}

for f in "$OSSEIN" "$INITFS" "$K" "$KBASE"; do [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }; done
[ -x "$B/bench" ] || { echo "missing $B/bench (run: just build-bench)" >&2; exit 1; }

median() { sort -n | awk '{a[NR]=$1} END{print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}'; }
val()  { sed -n 's/.*perop=\([0-9.]*\)[a-z]*.*/\1/p'; }
unit() { sed -n 's/.*perop=[0-9.]*\([a-z]*\).*/\1/p'; }

run_med() {  # <full run command...> -> "<median> <unit>"
  local vals=() out v u=""
  for _ in $(seq "$RUNS"); do
    out=$("$@" 2>/dev/null)
    v=$(printf '%s' "$out" | val)
    if [ -n "$v" ]; then
      vals+=("$v"); u=$(printf '%s' "$out" | unit)
    else
      echo "  warn: no perop= from: $* — run dropped from median" >&2
    fi
  done
  [ ${#vals[@]} -eq 0 ] && { echo "ERR -"; return; }
  echo "$(printf '%s\n' "${vals[@]}" | median) $u"
}

row() { printf "  %-30s %10s %-3s\n" "$1" "$2" "$3"; }

# Every target reports the kernel it actually ran: this suite compares guest kernels, so the
# version is the primary variable and should never be inferred.
kernel_of() { "$@" uname -r 2>/dev/null | tr -d '[:space:]'; }

# --- fairness preflight: how many CPUs does each runtime actually expose? ---
echo "=== nproc preflight (must all read $CPUS for a fair spawn comparison) ==="
np_ossein=$(OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
row "ossein/opt (--cpus $CPUS)" "${np_ossein:-?}" "cpu  kernel=$(kernel_of env OSSEIN_KERNEL="$K" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG")"
if [ -e "$KBASE" ]; then
  row "ossein/baseline" "" "     kernel=$(kernel_of env OSSEIN_KERNEL="$KBASE" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" "$IMG")"
else
  row "ossein/baseline" "(absent)" "SKIPPED — no $KBASE"
fi
if command -v container >/dev/null 2>&1; then
  # per-VM like ossein, but runs requested+1 vCPUs, so --cpus $CCPUS -> nproc CPUS.
  npc=$(container run --rm --cpus "$CCPUS" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  row "apple-container (--cpus $CCPUS)" "${npc:-?}" "cpu  kernel=$(kernel_of container run --rm --cpus "$CCPUS" "$IMG")"
fi
for spec in "${RUNTIMES[@]}"; do
  IFS='=' read -r label ctx <<<"$spec"
  [ -n "$DOCKER" ] || { row "$label" "(no docker CLI on PATH)" ""; continue; }
  kver=$("$DOCKER" --context "$ctx" run --rm "$IMG" uname -r 2>/dev/null | tr -d '[:space:]')
  if [ -z "$kver" ]; then
    row "$label (ctx=$ctx)" "(unreachable)" "not running / bad context"
    continue
  fi
  np=$("$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET" "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  row "$label (ctx=$ctx, --cpuset $CPUSET)" "${np:-?}" "cpu  kernel=$kver"
  case "$KVER_SEEN" in *"|$kver|"*)
    echo "  !! WARNING: '$kver' already seen — $label resolves to the SAME daemon as another runtime (context not isolated). Its numbers are a DUPLICATE." >&2 ;;
  esac
  KVER_SEEN="$KVER_SEEN|$kver|"
done
if podman_avail; then
  PODMAN_NP=$("${PODMAN[@]}" run --rm "$IMG" nproc 2>/dev/null | tr -d '[:space:]')
  if [ "$PODMAN_NP" = "$CPUS" ]; then
    row "podman (rootless; machine=$CPUS vCPU)" "$PODMAN_NP" "cpu  kernel=$(kernel_of "${PODMAN[@]}" run --rm "$IMG")"
  else
    row "podman (rootless)" "${PODMAN_NP:-?}" "cpu — need machine=$CPUS: 'podman machine set --cpus $CPUS'; SKIPPED"
  fi
fi

for bn in $BENCHES; do
  # shellcheck disable=SC2046  # intentional word-split of the target spec
  set -- $(bench_target "$bn")   # $@ = target path [+ args]
  echo
  echo "=== $bn (median x$RUNS, lower is better) ==="

  read -r v u <<<"$(run_med env OSSEIN_KERNEL="$K"     OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" -v "$B:/bench" "$IMG" "$@")"
  row "ossein/opt (no seccomp)"      "$v" "$u"
  read -r v u <<<"$(run_med env OSSEIN_KERNEL="$KBASE" OSSEIN_INITFS="$INITFS" "$OSSEIN" run --cpus "$CPUS" -v "$B:/bench" "$IMG" "$@")"
  row "ossein/baseline (no seccomp)" "$v" "$u"

  if command -v container >/dev/null 2>&1; then
    read -r v u <<<"$(run_med container run --rm --cpus "$CCPUS" --volume "$B:/bench" "$IMG" "$@")"
    row "apple-container" "$v" "$u"
  fi

  for spec in "${RUNTIMES[@]}"; do
    IFS='=' read -r label ctx <<<"$spec"
    [ -n "$DOCKER" ] || { row "$label" "(no docker CLI on PATH)" ""; continue; }
    read -r on uo  <<<"$(run_med "$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET"                                   -v "$B:/bench" "$IMG" "$@")"
    read -r off uf <<<"$(run_med "$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET" --security-opt seccomp=unconfined -v "$B:/bench" "$IMG" "$@")"
    row "$label (seccomp on)"  "$on"  "$uo"
    row "$label (seccomp off)" "$off" "$uf"
  done

  # podman: no --cpuset-cpus (rootless can't); only run if the machine is CPUS-wide
  # (checked in the preflight) so the numbers are on the right CPU count.
  if podman_avail && [ "$PODMAN_NP" = "$CPUS" ]; then
    read -r on uo  <<<"$(run_med "${PODMAN[@]}" run --rm                                   -v "$B:/bench" "$IMG" "$@")"
    read -r off uf <<<"$(run_med "${PODMAN[@]}" run --rm --security-opt seccomp=unconfined -v "$B:/bench" "$IMG" "$@")"
    row "podman (seccomp on)"  "$on"  "$uo"
    row "podman (seccomp off)" "$off" "$uf"
  fi
done
