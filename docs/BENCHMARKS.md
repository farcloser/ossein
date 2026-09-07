# Benchmarks — the ledger

This is the canonical, dated record of ossein's performance numbers: boot
(cold and warm) across reference images, the kernel-compile `bench-run` and
`bench-build` workloads, and the A/Bs that justified design changes. Add a
new dated section when the stack changes; never overwrite old entries —
regressions are only visible against history. Deep-dive analysis lives in
its own doc (e.g. `ROOTFS-MATERIALIZATION.md`); this file holds the numbers.

Method rules (every entry below follows them unless noted):

- Quiet host, `caffeinate -i` (a sleeping laptop poisoned one overnight
  session: wall times inflated 20×, DNS loss failed later runs).
- Boot numbers are the host's `boot complete total=` (ns → ms); the guest
  console `copy-in:` line attributes materialization. Medians over ≥5 runs
  for warm; cold runs are network-bound, so both raw runs are recorded
  instead of pretending a median of 2 is stable.
- Compile workloads time phases IN the container (`date +%s%3N` around each
  phase) so apt/network noise never pollutes the compile number; the kernel
  source tarball comes from a host share, not the network.
- Host wall-clock sensitivity is real: the same warm boot measured ~200 ms
  on an idle morning host and ~260 ms later the same day. Compare numbers
  within a session first, across sessions second.

## Baseline — 2026-08-02, EROFS materialization shipped

Stack: embedded kernel **7.1.5-ossein.2** (EROFS, pruned config), default
codec **goerofs** (uncompressed EROFS written by github.com/forkcloser/erofs
with shared-inode hardlinks), guest mounts the blob read-only under a
tmpfs-upper overlay — materialization is two mounts, no extraction.
Machine: M-series mac, ossein defaults (`--cpus 2`, 4 GiB) unless stated.

### Boot: cold and warm, four reference images

Cold = image cache wiped first: registry pull + flatten + EROFS build +
boot. `flatten` is the "rootfs blob pinned" stage and contains virtually all
of the cold cost; it is network + host-CPU bound. Warm = cached blob;
median of 5.

| image | blob (MiB) | cold 1 / cold 2 (wall) | of which flatten | warm boot (median) | guest materialization |
|---|---|---|---|---|---|
| alpine | 8 | 1.5 s / 2.4 s | 1.3 / 2.3 s | **267 ms** | 182 µs |
| debian bookworm-slim | 97 | 3.6 s / 3.1 s | 3.5 / 2.9 s | **255 ms** | 189 µs |
| ruby | 1114 | 35.0 s / 35.9 s | 34.8 / 35.8 s | **268 ms** | 201 µs |
| rust | 1558 | 45.6 s / 55.8 s | 45.5 / 55.6 s | **254 ms** | 140 µs |

The property this design bought: **warm boot is flat in image size** —
254–268 ms whether the image is 8 MiB or 1.5 GiB, with materialization at
140–200 µs. Under the previous tar+lz4-extract design, warm rust was
~857 ms (592–637 ms of it extraction) and a >RAM image could not run at all.

Cold start is pull-dominated (ruby/rust layers are GiB-scale downloads);
the EROFS build rides inside the flatten stage. Cold numbers move with the
network and registry CDN — treat them as orders of magnitude, not baselines.

### bench-run (kernel vmlinux compile in-rootfs, --cpus 4, --memory 8192)

ossein only, warm image, tarball via share, phases in-container, 3 runs.
(For the cross-runtime picture — orbstack, docker-desktop, apple-container,
podman — run `just bench-run`; it is hours, and the last full sweep predates
this stack.)

| run | untar | defconfig | vmlinux | no-op rebuild | wall (incl. apt) |
|---|---|---|---|---|---|
| 1 | 6169 ms | 1381 ms | 276.7 s | 2236 ms | 306 s |
| 2 | 6439 ms | 1461 ms | 280.6 s | 1974 ms | 300 s |
| 3 | 6328 ms | 1439 ms | 279.2 s | 1951 ms | 299 s |
| **median** | **6328 ms** | **1439 ms** | **279.2 s** | **1974 ms** | **300 s** |

Session note: the pre-flip A/B (below), run the same morning on a fresher
host, measured vmlinux at 257.9–261.2 s across all three modes — ~7% below
this afternoon's runs. That gap is host-state drift between sessions, not
the codec flip: within each session the modes were at parity. When hunting
a regression, re-run the comparison in ONE session.

### bench-build (same compile via buildkitd + buildctl, cold, -j2)

Cold each run (`ossein stop` + `gc --prune-cache`, `--no-cache`): apt +
kernel download + `-j2` compile all re-run inside the buildkitd microVM
(`--cpus 4`; `-j2` is fixed in the Dockerfile for cross-runtime fairness).
This also exercises the buildkit VM's own rootfs through the goerofs path.

| run | wall |
|---|---|
| 1 | 566 s |
| 2 | 562 s |

### The A/B that shipped this stack (2026-08-02, pre-flip, local kernel)

Full context in `ROOTFS-MATERIALIZATION.md`. Kernel-compile phases,
median of 3, identical kernel across modes:

| phase | tmpfs (extract) | overlay-over-tmpfs | erofs+overlay |
|---|---|---|---|
| untar | 6222 ms | 6239 ms | 6219 ms |
| defconfig | 1321 ms | 1351 ms | 1361 ms |
| vmlinux -j4 | 261.2 s | 260.4 s | 257.9 s |
| no-op rebuild (stat storm) | 2032 ms | 1889 ms | 1886 ms |

Runtime parity (spread <1.5%); the stat-heavy no-op phase was consistently
~6% *faster* under overlay. Boot A/B same day (rust, warm, median 5):
tmpfs 857 ms → erofs+overlay 200 ms, materialization 550–630 ms → ~110 µs.

## History — how the copy-in wall fell (rust, --cpus 2, warm)

| date | stack | rust materialization | warm boot |
|---|---|---|---|
| 2026-07-09 | Apple vminitd (v1-containerization) | ~12 s extraction | ~24 s total |
| 2026-07-13 | ossein Go vminitd, vsock+gzip | 236 ms (debian) | ~2 s (debian, incl. 823 ms resolve) |
| 2026-07-31 am | vsock+zstd | 2143 → 2015 ms (extractor syscall rework) | |
| 2026-07-31 | virtio-blk + lz4, serial decode | 950 ms | |
| 2026-07-31 | parallel lz4 decode | 608 ms | ~891 ms |
| 2026-08-01 | initramfs boot + async probe + vmnet per-VM | 552–631 ms | ~765–857 ms |
| 2026-08-02 | **EROFS mount + overlay (shipped)** | **~0.14–0.2 ms** | **~254–268 ms** |

Older per-stage analysis (image resolve, VM create, handshake) lives in
`OSSEIN-PERF.md` (historical — its numbers predate virtio-blk, per-VM vmnet
and EROFS; trust this file over it).

## Refreshing these numbers

- Boot matrix: wipe `~/Library/Caches/ossein/images` per image for cold;
  `build/ossein run <img> true` and read `boot complete total=` (host) +
  `copy-in:` (console log, `--console-log <path>`). 2 cold + 5 warm each.
- bench-run (ossein-focused): warm image, share the kernel tarball via
  `-v`, run the compile with in-container phase timing (see
  `ROOTFS-MATERIALIZATION.md` method notes), 3+ runs, medians.
- Cross-runtime: `just bench-run` / `just bench-build` (slow, cold by
  design, includes other runtimes present on the host).
- Codec A/B and boot phase isolation are GONE, along with the tar+lz4 codec
  they measured. `OSSEIN_ROOTFS_CODEC`, `ossein.copyin=raw|decode` and
  `ossein.rootfsmode=overlay` no longer exist: there is one rootfs format and
  the guest mounts it, so there is no extraction to split into phases. The
  numbers those knobs produced are recorded above and in
  `ROOTFS-MATERIALIZATION.md`; reproducing them means checking out a commit
  from before the removal.
