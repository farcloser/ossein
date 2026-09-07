#!/usr/bin/env bash
# COLD cross-runtime BUILD bench — measures each runtime's IMAGE-BUILD path (build
# engine / buildkit) on the SAME kernel-compile workload as the run bench, so you
# can compare build-vs-run per runtime. Dockerfile: tools/bench-kernel/Dockerfile.
#
# COLD every iteration: before each build we wipe the base image + build cache, and
# build with --no-cache — so apt + kernel download + compile all re-run, no cache
# edge for anyone. -j is fixed to 2 IN THE DOCKERFILE so build parallelism is equal
# regardless of each build VM's CPU count.
#
# Fairness caveat: you can't --cpuset a build the way you pin a run — the build runs
# in each runtime's build VM with its own CPU count. Fixing -j2 normalizes the
# compile parallelism (the dominant cost); the residual (idle-core contention) is
# noted, not controlled.
#
# ossein builds via its OWN buildkitd microVM (`ossein buildkit --detach` +
# buildctl), which keeps a PERSISTENT cache — so "cold" means pruning it each run
# (ossein gc --prune-cache). Needs `buildctl` on PATH.
#
# ⚠ SLOW: minutes/run × RUNS × up-to-5 runtimes.
#
# usage: bench-build.sh [ossein-bin] [runs]
set -uo pipefail

OSSEIN="${1:-../build/ossein}"
RUNS="${2:-5}"
CPUS=4                                          # pin ossein's buildkit VM to match the run bench

HERE="$(cd "$(dirname "$0")/.." && pwd)"
CTX="$HERE/tools/bench-kernel"                  # build context (the Dockerfile dir)
BASE='debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171' # FROM in the Dockerfile (wiped for cold pull)
TAG='ossein-benchbuild'

# One real docker CLI (from PATH) drives BOTH daemons; --context selects which. label=context.
DOCKER="$(command -v docker 2>/dev/null || true)"
RUNTIMES=( "orbstack=orbstack" "docker-desktop=desktop-linux" )

median() { sort -n | awk '{a[NR]=$1} END{print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}'; }
mean()   { awk '{s+=$1;n++} END{if(n)printf "%.0f",s/n}'; }
row()    { printf "  %-24s %8ss  (median %ss / %s ok)\n" "$1" "$2" "$3" "$4"; }

[ -f "$CTX/Dockerfile" ] || { echo "missing $CTX/Dockerfile" >&2; exit 1; }

# --- cold-wipe per runtime -------------------------------------------------
wipe_docker() { "$1" --context "$2" image rm -f "$BASE" "$TAG" >/dev/null 2>&1; "$1" --context "$2" builder prune -af >/dev/null 2>&1; }
wipe_podman() { podman image rm -f "$BASE" "$TAG" >/dev/null 2>&1; podman builder prune -af >/dev/null 2>&1; }
wipe_apple()  { container image rm "$BASE" "$TAG" >/dev/null 2>&1 || true; container builder prune -f >/dev/null 2>&1 || true; }
wipe_ossein() { "$OSSEIN" stop >/dev/null 2>&1; "$OSSEIN" gc --prune-cache >/dev/null 2>&1; }

# --- ossein build via its buildkitd + buildctl -----------------------------
run_ossein_build() {
  command -v buildctl >/dev/null 2>&1 || { echo "buildctl not on PATH" >&2; return 1; }
  # start a fresh buildkitd microVM (pinned to CPUS vCPUs so ossein's build is
  # CPU-consistent with its run bench) and export BUILDKIT_HOST into this subshell.
  eval "$("$OSSEIN" buildkit --detach --cpus "$CPUS" 2>/dev/null)" || { echo "ossein buildkit --detach failed" >&2; return 1; }
  [ -n "${BUILDKIT_HOST:-}" ] || { echo "no BUILDKIT_HOST from ossein buildkit" >&2; return 1; }
  buildctl build --frontend dockerfile.v0 \
    --local context="$CTX" --local dockerfile="$CTX" --no-cache
}

timed() {  # <wipe args...> -- <run cmd...> -> seconds, or fail (prints tail on error)
  local wipe=() run=() seen=0
  for a in "$@"; do
    if [ "$seen" = 0 ] && [ "$a" = "--" ]; then seen=1; continue; fi
    if [ "$seen" = 0 ]; then wipe+=("$a"); else run+=("$a"); fi
  done
  "${wipe[@]}" 2>/dev/null
  local log t0 t1 rc; log="$(mktemp "${TMPDIR:-/tmp}/bench-build-log.XXXXXX")"
  t0=$(date +%s); "${run[@]}" >"$log" 2>&1; rc=$?; t1=$(date +%s)
  if [ "$rc" -eq 0 ]; then echo $((t1 - t0)); rm -f "$log"
  else { echo "    build FAILED (exit $rc) — last 25 lines:"; tail -25 "$log" | sed 's/^/      | /'; } >&2; rm -f "$log"; return 1; fi
}

run_med() { local vals=() v; for _ in $(seq "$RUNS"); do v="$("$@")" && [ -n "$v" ] && vals+=("$v"); done
  [ ${#vals[@]} -eq 0 ] && { echo "ERR ERR 0"; return; }
  echo "$(printf '%s\n' "${vals[@]}" | mean) $(printf '%s\n' "${vals[@]}" | median) ${#vals[@]}"; }

echo "=== cold cross-runtime BUILD bench: kernel vmlinux compile (Dockerfile) ==="
echo "    context: $CTX   runs: $RUNS   (build -j fixed to 2 in the Dockerfile)"
echo

echo ">> ossein (buildkitd + buildctl)"
read -r m md n <<<"$(run_med timed wipe_ossein -- run_ossein_build)"
row ossein "$m" "$md" "$n"

if command -v container >/dev/null 2>&1; then
  echo ">> apple-container"
  read -r m md n <<<"$(run_med timed wipe_apple -- container build --no-cache -t "$TAG" "$CTX")"
  row apple-container "$m" "$md" "$n"
fi

for spec in "${RUNTIMES[@]}"; do
  IFS='=' read -r label ctx <<<"$spec"
  [ -n "$DOCKER" ] || { echo ">> $label: (no docker CLI on PATH)"; continue; }
  "$DOCKER" --context "$ctx" version >/dev/null 2>&1 || { echo ">> $label: (ctx $ctx unreachable)"; continue; }
  echo ">> $label (ctx=$ctx)"
  read -r m md n <<<"$(run_med timed wipe_docker "$DOCKER" "$ctx" -- \
    "$DOCKER" --context "$ctx" build --no-cache -t "$TAG" "$CTX")"
  row "$label" "$m" "$md" "$n"
done

if command -v podman >/dev/null 2>&1; then
  echo ">> podman"
  read -r m md n <<<"$(run_med timed wipe_podman -- podman build --no-cache -t "$TAG" "$CTX")"
  row podman "$m" "$md" "$n"
fi

echo; echo "done. Lower is better. Every build fully cold (--no-cache + base image + build cache wiped)."
