#!/usr/bin/env bash
# vsock proxy throughput A/B — does the relay's copy-buffer size matter?
#
# The buildkit data path is: host unix socket → bidiPipe (pkg/container) → vz
# vsock → guest agent bidiCopy (vsock ⇄ unix) → buildkitd. Both relays are
# plain io.Copy today, which for these conn types means the generic 32 KiB
# loop (neither *vsock.Conn nor vz's conn offers ReaderFrom/WriterTo, and on
# darwin *net.UnixConn.ReadFrom is the same generic loop). This script measures
# that stock shape against variants where BOTH relays use io.CopyBuffer with a
# larger buffer — the one userspace lever there is (mdlayher/vsock itself is a
# one-syscall-per-call wrapper with nothing to tune).
#
# Variants are built with `go build -overlay`: the two relay files are copied
# to a scratch dir, patched there, and mapped over the real paths at build
# time. Every go invocation runs from the repo root through the ordinary aqua
# shim under the repo's own policy — nothing is bypassed, and the working tree
# is never modified. Per variant we rebuild the guest initfs (the agent's
# bidiCopy) and the host bench binary (which links pkg/container's bidiPipe),
# then run tools/vsockbench through the production ExposeUnix path. Needs VZ
# (a plain terminal, not a sandbox) and the debian image (pulled on first use).
#
# usage: bench-vsock.sh [bytes] [streams] [runs] [sizes...]
#   bytes   per stream per run (default 2 GiB)
#   streams concurrent connections (default 1)
#   runs    per direction per variant (default 3)
#   sizes   copy buffer sizes to try besides stock (default: 262144 1048576)
# BENCH_BUILD_ONLY=1 builds every variant and stops (no VZ needed).
set -euo pipefail

BYTES="${1:-2147483648}"
STREAMS="${2:-1}"
RUNS="${3:-3}"
shift $(( $# > 3 ? 3 : $# )) || true
SIZES=("$@")
[ ${#SIZES[@]} -gt 0 ] || SIZES=(262144 1048576)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

LINUX_ENV=(env CGO_ENABLED=0 GOOS=linux GOARCH=arm64)
OUT="$ROOT/build/vsockbench-results.txt"
: >"$OUT"

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

# The two relay call sites, one per side of the proxy.
GUEST_RELAY=guest/internal/guestagent/proxy.go
HOST_RELAY=pkg/container/container.go

# build_variant LABEL [OVERLAY] — produce build/vsockbench-<LABEL> (host) and
# build/initfs-<LABEL>.cpio (guest), optionally through a go overlay file.
build_variant() {
    local label="$1" overlay="${2:-}"
    local -a ov=()
    [ -n "$overlay" ] && ov=(-overlay "$overlay")

    "${LINUX_ENV[@]}" go build "${ov[@]}" -trimpath -ldflags "-s -w" \
        -o "build/vminitd-$label" ./guest/vminitd
    go build -o build/build-initfs ./tools/build-initfs
    build/build-initfs -in "build/vminitd-$label" -out "build/initfs-$label.cpio"
    CGO_ENABLED=1 go build "${ov[@]}" -o "build/vsockbench-$label" ./tools/vsockbench 2>&1 \
        | grep -v 'duplicate libraries' || true
    codesign --force --sign - --timestamp=none --entitlements vz.entitlements \
        "build/vsockbench-$label" 2>/dev/null
}

# make_overlay SIZE — write patched copies of both relay files under SCRATCH
# and print the path of an overlay JSON mapping them over the originals. The
# struct wrappers hide ReaderFrom/WriterTo so the buffer is actually used
# (io.CopyBuffer defers to those fast paths otherwise).
make_overlay() {
    local size="$1"
    local dir="$SCRATCH/buf$size"
    local repl="_, _ = io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, $size))"
    mkdir -p "$dir"

    local f
    for f in "$GUEST_RELAY" "$HOST_RELAY"; do
        grep -q '_, _ = io.Copy(dst, src)' "$f" || {
            echo "no io.Copy(dst, src) in $f — patch target moved" >&2
            exit 1
        }
        sed "s|_, _ = io.Copy(dst, src)|$repl|" "$f" >"$dir/$(basename "$f")"
    done

    cat >"$dir/overlay.json" <<EOF
{"Replace": {
  "$ROOT/$GUEST_RELAY": "$dir/$(basename "$GUEST_RELAY")",
  "$ROOT/$HOST_RELAY": "$dir/$(basename "$HOST_RELAY")"
}}
EOF
    echo "$dir/overlay.json"
}

run_variant() {
    local label="$1"
    echo "== $label" | tee -a "$OUT"
    "build/vsockbench-$label" \
        -initfs "build/initfs-$label.cpio" -kernel pkg/guestartifacts/kernel-arm64 \
        -bench-dir build -bytes "$BYTES" -streams "$STREAMS" -runs "$RUNS" -label "$label" \
        | tee -a "$OUT"
}

[ -f pkg/guestartifacts/kernel-arm64 ] || just fetch-kernel
"${LINUX_ENV[@]}" go build -trimpath -ldflags "-s -w" -o build/vsockpeer ./tools/vsockpeer

echo ">> building stock"
build_variant stock

for size in "${SIZES[@]}"; do
    echo ">> building buf$size"
    build_variant "buf$size" "$(make_overlay "$size")"
done

if [ "${BENCH_BUILD_ONLY:-0}" = 1 ]; then
    echo ">> BENCH_BUILD_ONLY: variants built, not run"
    ls -la build/vsockbench-* build/initfs-*.cpio
    exit 0
fi

run_variant stock
for size in "${SIZES[@]}"; do run_variant "buf$size"; done

echo
echo ">> results in $OUT"
