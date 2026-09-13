#!/usr/bin/env bash
# COLD cross-runtime RUN bench — measures each runtime's container `run` path on a
# real heavy workload: a Linux kernel compile.
#
# Workload: `run <debian> sh -c '<apt toolchain> + <fetch kernel> + make -j vmlinux'`,
# done ENTIRELY IN THE CONTAINER ROOTFS (no host mount) — so it exercises CPU +
# fork/exec + syscalls + rootfs I/O, and NOT virtio-fs (ossein's known-weak share,
# which would otherwise be conflated into the number).
#
# COLD every iteration for every runtime: the base image is wiped so it re-pulls,
# apt + kernel source re-download, kernel recompiles — no cache edge for anyone;
# the pull/apt/download noise is equal and averages out over RUNS; the multi-minute
# compile dominates.
#
# Fairness: all pinned to CPUS visible CPUs (nproc==CPUS → same make -j). Per-VM
# runtimes (ossein, apple-container) get MEM MiB — a kernel build emits ~1-2 GB of
# objects into the RAM-backed rootfs (docker/orbstack build to disk and don't care;
# that per-VM memory-sizing is itself a fair thing to note).
#
# ⚠ SLOW: minutes/run × RUNS × up-to-5 runtimes. Ctrl-C is safe between runs.
#
# usage: bench-run.sh [ossein-bin] [runs]
set -uo pipefail

OSSEIN="${1:-../build/ossein}"
RUNS="${2:-5}"
CPUS=4
CPUSET="0-$((CPUS - 1))"
CCPUS=$((CPUS - 1))
MEM=8192                                   # MiB for per-VM runtimes (kernel objects are big)

BASE='debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171'
KURL='https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-7.1.3.tar.xz'

# The whole workload, run in the container rootfs (/tmp), no mount. Build only
# `vmlinux` (kernel image, no modules) to keep each run bounded (~minutes).
WORKLOAD='set -e
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq --no-install-recommends build-essential bc bison flex \
  libssl-dev libelf-dev xz-utils curl ca-certificates >/dev/null 2>&1
cd /tmp
curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors "'"$KURL"'" -o k.tar.xz
tar xf k.tar.xz
cd linux-*
make -s defconfig
make -s -j'"$CPUS"' vmlinux'

# One real docker CLI (from PATH) drives BOTH daemons; --context selects which. label=context.
DOCKER="$(command -v docker 2>/dev/null || true)"
RUNTIMES=( "orbstack=orbstack" "docker-desktop=desktop-linux" )
PODMAN_NP=""

median() { sort -n | awk '{a[NR]=$1} END{print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}'; }
mean()   { awk '{s+=$1;n++} END{if(n)printf "%.0f",s/n}'; }
row()    { printf "  %-24s %8ss  (median %ss / %s ok)\n" "$1" "$2" "$3" "$4"; }

# Both caches: blobs AND the tag→digest resolve cache. Wiping only the blobs
# leaves a pinned resolution whose blob is gone; that is a half-cold state no
# other runtime has (their `image rm` drops the tag resolution too), and it is
# unfair to ossein besides — everyone else re-resolves the tag, ossein must too.
wipe_ossein()  { rm -rf "$HOME/Library/Caches/ossein/images" "$HOME/Library/Caches/ossein/resolve" 2>/dev/null; }
wipe_docker()  { "$1" --context "$2" image rm -f "$BASE" >/dev/null 2>&1; }
wipe_podman()  { podman image rm -f "$BASE" >/dev/null 2>&1; }
wipe_apple()   { container image rm "$BASE" >/dev/null 2>&1 || container images rm "$BASE" >/dev/null 2>&1; }

timed() {  # <wipe-fn args...> -- <run cmd...> -> whole seconds, or fail
  local wipe=() run=() seen=0
  for a in "$@"; do
    if [ "$seen" = 0 ] && [ "$a" = "--" ]; then seen=1; continue; fi
    if [ "$seen" = 0 ]; then wipe+=("$a"); else run+=("$a"); fi
  done
  "${wipe[@]}" 2>/dev/null
  local log t0 t1 rc; log="$(mktemp "${TMPDIR:-/tmp}/bench-run-log.XXXXXX")"
  t0=$(date +%s); "${run[@]}" >"$log" 2>&1; rc=$?; t1=$(date +%s)
  if [ "$rc" -eq 0 ]; then echo $((t1 - t0)); rm -f "$log"
  else { echo "    run FAILED (exit $rc) — last 25 lines:"; tail -25 "$log" | sed 's/^/      | /'; } >&2; rm -f "$log"; return 1; fi
}

run_med() { local vals=() v; for _ in $(seq "$RUNS"); do v="$("$@")" && [ -n "$v" ] && vals+=("$v"); done
  [ ${#vals[@]} -eq 0 ] && { echo "ERR ERR 0"; return; }
  echo "$(printf '%s\n' "${vals[@]}" | mean) $(printf '%s\n' "${vals[@]}" | median) ${#vals[@]}"; }

echo "=== cold cross-runtime RUN bench: kernel vmlinux compile (in-rootfs) ==="
echo "    base: $BASE   kernel: $(basename "$KURL")   runs: $RUNS   cpus: $CPUS"
echo

echo ">> ossein"
# ossein boots on its embedded kernel + initfs (the shipping artifacts) — no override,
# matching how bench-build measures ossein as it ships.
read -r m md n <<<"$(run_med timed wipe_ossein -- \
  "$OSSEIN" run --cpus "$CPUS" --memory "$MEM" "$BASE" sh -c "$WORKLOAD")"
row ossein "$m" "$md" "$n"

if command -v container >/dev/null 2>&1; then
  echo ">> apple-container"
  read -r m md n <<<"$(run_med timed wipe_apple -- \
    container run --rm --cpus "$CCPUS" -m "${MEM}M" "$BASE" sh -c "$WORKLOAD")"
  row apple-container "$m" "$md" "$n"
fi

for spec in "${RUNTIMES[@]}"; do
  IFS='=' read -r label ctx <<<"$spec"
  [ -n "$DOCKER" ] || { echo ">> $label: (no docker CLI on PATH)"; continue; }
  "$DOCKER" --context "$ctx" version >/dev/null 2>&1 || { echo ">> $label: (ctx $ctx unreachable)"; continue; }
  echo ">> $label (ctx=$ctx)"
  read -r m md n <<<"$(run_med timed wipe_docker "$DOCKER" "$ctx" -- \
    "$DOCKER" --context "$ctx" run --rm --cpuset-cpus="$CPUSET" "$BASE" sh -c "$WORKLOAD")"
  row "$label" "$m" "$md" "$n"
done

if command -v podman >/dev/null 2>&1; then
  PODMAN_NP=$(podman run --rm "$BASE" nproc 2>/dev/null | tr -d '[:space:]')
  if [ "$PODMAN_NP" = "$CPUS" ]; then
    echo ">> podman (machine=$CPUS vCPU)"
    read -r m md n <<<"$(run_med timed wipe_podman -- podman run --rm "$BASE" sh -c "$WORKLOAD")"
    row podman "$m" "$md" "$n"
  else echo ">> podman: machine ${PODMAN_NP:-?} vCPU, need $CPUS ('podman machine set --cpus $CPUS'); skipped"; fi
fi

echo; echo "done. Lower is better. Every run fully cold, all work in the container rootfs."
