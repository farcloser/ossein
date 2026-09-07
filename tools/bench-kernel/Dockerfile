# Heavy from-source build for the cross-runtime BUILD bench (tools/bench-build.sh).
# Same workload as the RUN bench (tools/bench-run.sh): compile a pinned Linux
# kernel (vmlinux, arm64 defconfig) — thousands of files, heavy cc/ld fork-exec.
# -j is FIXED here (not $(nproc)) so build parallelism is equal across runtimes
# regardless of each build VM's CPU count. Self-contained; BASE/KURL are pinned.
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
# Deliberately unpinned apt versions: this image is a benchmark harness, not a
# shipped artifact, and pinning would break it on every debian point release.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential bc bison flex libssl-dev libelf-dev xz-utils curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-7.1.3.tar.xz" -o k.tar.xz \
 && tar xf k.tar.xz && rm k.tar.xz
# WORKDIR cannot take the extracted tree: its name is a glob (linux-<version>),
# and WORKDIR does not expand one — so `cd` is the only option here.
# hadolint ignore=DL3003
RUN cd /build/linux-* && make -s defconfig && make -s -j2 vmlinux
