# Rootfs materialization: where the time goes, and what to do about it

Status: **option 3 shipped** (2026-08-02, kernel 7.1.5-ossein.2 — see
"Shipped" at the bottom for what closed and what is still open). EROFS is the
default codec on every `ossein run`; the code lives in
`guest/internal/guestagent/copyin.go`, `pkg/image/goerofs.go` and
ossein-kernel's `kernel/config/kernel-fragment`.

Everything between here and that final section is the investigation as it was
conducted — the measurements that chose the option and the state of the work
while it was still a prototype. It is kept because the numbers are the
justification for the design, not because any of it is still pending. Where an
earlier section says "uncommitted" or "remaining to ship", read it as the
record of that moment; the Shipped section is the present tense.

This was the performance workstream after the blk+lz4 transport and the
per-VM vmnet networking landed.

The question: `ossein run` is fast for small images (~250 ms) but boot time
grows with image size — rust costs ~890 ms, and essentially all of the growth
is one stage.

## Where we were, before any of this (the tar+lz4 pipeline)

The pipeline, end to end. Steps 1 and 3 are unchanged today; 2 and 4 are what
option 3 replaced:

1. **Resolve** — `~/Library/Caches/ossein/resolve/v1/<sha256(ref+platform)>.json`
   holds manifest digest + OCI config. A warm run is fully offline.
2. **Flatten (cold only)** — layers pulled and decompressed, walked top-first
   with whiteouts resolved and hardlinks repaired (`pkg/image/flatten`), emitted
   as ONE plain tar, piped through lz4 (4 MiB blocks, all cores), padded with an
   lz4 skippable frame to a 512-byte multiple. primordium spools it while
   hashing and commits it to the content store, keyed by
   `manifest-digest + codec + flattenGeneration`.
3. **Attach** — that cached file IS the disk image: attached read-only as a
   virtio-blk device with serial `osrootfs`. No host-side data path.
4. **Materialize (every boot)** — vminitd, at init and before serving,
   resolves the serial via `/sys/block/*/serial`, mounts a tmpfs sized
   `memory − 1 GiB`, decodes lz4 with GOMAXPROCS workers, and untars into the
   tmpfs. The `Copy` RPC the host sends later only *awaits* this.

## The measurement that matters (rust, 2 vCPUs, warm)

```
microVM created     130 ms   VZ config + vmnet network
guest agent up      139 ms   kernel boot + init + vsock dial
rootfs extracted    619 ms   ← all size-dependent cost lives here
  ├─ upstream       152 ms   read 732 MB off the device + lz4 → 1.6 GB
  └─ write side     463 ms   create ~50k files in tmpfs
network up            3 ms
process started      10 ms
                    ─────
total               891 ms
```

Two structural facts:

- **The whole image is re-materialized as individual files on every boot.**
  The write side is ~50k × (openat + write + close + utimensat) plus a 1.6 GB
  memcpy, single-threaded.
- **The rootfs is guest RAM.** rust holds ~1.5 GB of a 4 GB VM as a second
  copy of an image already on disk in compressed form.

NOT YET MEASURED, and it is the most decision-relevant number: the split
within those 463 ms between bulk memcpy (~150–250 ms, irreducible if every
file must exist in RAM) and per-entry syscall/metadata work (~200–300 ms).
One instrumented run separates them.

## Options considered

**1. Parallel extraction.** The usual "tar is a sequential stream" objection
does not apply: the blob is a random-access block device whose format we
generate. Flatten could emit N shards, one lz4 frame each, plus an offset
index; N guest workers each extract their own region. Constraints: hardlinks
must stay in the same shard as their target (flatten already orders
link-after-target), concurrent `mkdir` must tolerate `EEXIST` (it does), and
the parent-resolution cache needs sharding. Wins both halves, scales with
vCPUs; at the default 2 vCPUs realistically 1.6–1.8×, so the write side lands
~260–290 ms. Requires a `flattenGeneration` bump.

**2. Cheaper per-entry work.** Stackable with 1. `io_uring` batching attacks
only the syscall half (needs `CONFIG_IO_URING`, presence unverified).
Decoding straight into `mmap`ed destination pages removes one of the two
copies for large files, and would need a size threshold since it loses on
small ones.

**3. Read-only filesystem image + tmpfs overlay — RECOMMENDED.** Change the
cached artifact from a tar to a mountable EROFS image; the guest mounts it
read-only and stacks a tmpfs upper via overlayfs. Boot cost becomes a mount:
619 ms → single-digit ms, and it stops scaling with image size entirely.

## Why option 3, judged against the goldilocks workload

The benchmark that decides this is `just bench-run` — a full Linux kernel
compile inside the container, which hammers the rootfs with both reads and
writes. Against that workload:

- **Writes are unchanged.** Object files land in the tmpfs upper: same RAM,
  same speed. The in-RAM property is preserved where it matters.
- **Reads converge after first touch.** An image page faults in once from the
  host page cache, then lives in guest page cache — identical to tmpfs from
  the second read onward. A kernel build re-reads the toolchain constantly.
- **~1.5 GB of RAM comes back**, available for page cache and compiler working
  sets instead of holding a duplicate of the image.
- **Eviction under pressure — possibly the decider.** tmpfs pages are
  unevictable and the guest has no swap, so a heavy `make -j` either fits or
  OOMs. EROFS pages are clean and re-readable, so the kernel can drop them.

Risks, in order:

1. **overlayfs metadata overhead** — every lookup walks two layers, and a
   kernel build is stat/open-heavy. Usually a few percent; it is the one thing
   that could eat the win, and `bench-run` will show it.
2. **Copy-up** for writes to paths that came from the image (in-tree builds).
   New files are free; modifying an image file copies it up first.
3. **`CONFIG_EROFS_FS` is almost certainly absent** from the guest kernel
   (overlayfs is present). Fragment change + kernel release — cheap now that
   the release process works, but it is a dependency.

Start with **uncompressed** EROFS: the blob grows (~1.6 GB vs 732 MB on host
disk) but there is zero decompression CPU competing with the compiler, and
disk is the cheapest resource here.

## Suggested plan

1. Instrument the write side to split memcpy from per-entry cost (~20 min,
   de-risks whichever path is chosen).
2. Enable EROFS in ossein-kernel, cut a release.
3. Produce the artifact both ways behind the existing codec/generation cache
   key so both variants coexist and neither can serve the other's bytes.
4. Let `bench-run` arbitrate: full kernel compile, both configurations, same
   host.
5. If overlayfs overhead dominates, fall back to option 1 (parallel
   extraction) — nothing is lost but the experiment.

## Measured results (2026-08-02, measured on the prototype)

Both de-risking experiments and the full option-3 prototype were run. Setup:
quiet caffeinated host, all modes on one locally built 7.1.5 kernel with
`CONFIG_EROFS_FS=y` (fragment needs the `CONFIG_MISC_FILESYSTEMS=y` menu gate
or olddefconfig silently reverts it). Host artifact built by piping the
flattened tar through `mkfs.erofs --tar=f -b4096` (erofs-utils 1.9.2 via
homebrew; `-b4096` is mandatory — the default block size is the HOST page
size, 16 KiB on Apple Silicon, which the 4 KiB-page guest refuses with
EINVAL). Guest sniffs the EROFS superblock magic on the blob device and
mounts (EROFS ro lower + tmpfs upper + overlay) instead of extracting.

**Boot (rust, warm, median of 5, --cpus 2):**

```
              tmpfs (today)    erofs+overlay
boot total       857 ms           200 ms      (4.3x, flat in image size)
materialization  550-630 ms       ~110 µs     (two mounts)
```

**Runtime, `bench-run` workload (vmlinux compile in-rootfs, --cpus 4,
median of 3, phases timed in-container so apt/network noise is excluded):**

```
              tmpfs      overlay-over-tmpfs   erofs+overlay
untar         6222 ms         6239 ms            6219 ms
defconfig     1321 ms         1351 ms            1361 ms
vmlinux      261.2 s         260.4 s            257.9 s
noop make     2032 ms         1889 ms            1886 ms
```

Parity everywhere (spread < 1.5%, no consistent loser); the no-op rebuild —
the pure stat-storm phase built to expose overlayfs lookup overhead — was
consistently ~6% FASTER on both overlay modes across all runs. Risk 1
(overlayfs metadata overhead) is dead: first-touch faults through virtio-blk
are invisible under a compile workload and steady-state lookups ride the
dcache. EROFS fidelity checks passed (file/symlink counts identical,
setuid/ownership preserved, writes land in upper).

Costs measured: rust blob 1520 MiB uncompressed EROFS vs 698 MiB tar+lz4
(host disk only); cold flatten pays mkfs.erofs inside a 26.6 s rust pull.

Adoption punch list beyond the prototype:
- mkfs.erofs bytes must be reproducible for the healing/verify path: pin
  `-T <epoch>` and a fixed/derived `-U` uuid (the prototype pins neither).
- Kernel fragment: golden refresh + verify-config assertion + release; prune
  the default-y riders (EROFS_FS_ZIP drags in XZ decompressors we don't use).
- ~~Decide the erofs-utils dependency shape~~ — RESOLVED (2026-08-02):
  github.com/erofs/go-erofs (pure Go, stdlib-only, Apache-2.0, erofs org,
  primary author is containerd's Derek McGowan, differential-fuzzed against
  mkfs.erofs) was evaluated end-to-end behind `OSSEIN_ROOTFS_CODEC=goerofs`
  (`pkg/image/goerofs.go`, a ~250-line tar→Writer adapter leaning on the
  flatten invariants). Fidelity vs the mkfs.erofs image: 4221-entry metadata
  diff clean, whole-tree content hash identical on rust; deterministic bytes
  (verified via the heal path) PROVIDED xattrs are applied in sorted order —
  map-order application changes on-disk layout. Must pass
  `WithBlockSize(4096)` (guest page size). ONE GAP: no `Writer.Link()`, so
  hardlinks (debian 4 files, rust 6) are materialized as read-back copies —
  correct content/metadata but st_nlink=1 and duplicated bytes (blob 101 vs
  93 MiB on debian). Adoption call: use go-erofs and contribute `Link()`
  upstream (two dirents → one nid + nlink at layout time is a contained
  change; the repo takes outside patches); keep the mkfs.erofs codec variant
  as a differential-test oracle rather than a runtime dependency.
- Fidelity test comparing extracted tree vs mounted tree over a corpus image
  (hardlinks, device nodes, sparse files, security.capability xattrs).
- Not yet measured: memory-pressure benefit (EROFS pages evictable vs
  unevictable tmpfs) — soak with constrained --memory; multi-VM host page
  cache sharing of one blob.

## Design completed on the forkcloser/erofs fork (2026-08-02, later)

The producer decision was superseded by a fork: **github.com/forkcloser/erofs**
(module-renamed fork of erofs/go-erofs) now carries `Writer.Link(old, new)` —
true shared-inode hardlinks: one nid, dirents in any directory, nlink computed
from live names, metadata ops through any name hitting the shared inode, and
an error at Close if a link's target was removed. Covered in-fork by
`mkfs_link_test.go` (file/symlink/device link groups, chmod-through-alias,
error cases, byte-determinism), validated by fsck.erofs, full suite green.

State of the stack at that point (all of it has since landed — see Shipped):

- **ossein** requires `github.com/forkcloser/erofs` (local `replace => ../erofs`,
  same pattern as the historical primordium replace; depguard allowlisted).
  `pkg/image/goerofs.go` converts the flatten tar straight into the Writer —
  hardlinks via `Link`, xattrs applied in sorted order (layout-order
  determinism), `WithBlockSize(4096)`, `WithBuildTime(0,0)`.
- **Tests**: `pkg/image/goerofs_internal_test.go` — corpus fidelity
  (setuid/setgid/sticky, multi-xattr incl. security.capability, devices,
  fifo, symlink+file hardlink groups, 1 MiB file, mixed name forms),
  byte-determinism, orphan-hardlink failure. All host-runnable, no VM.
- **Kernel**: fragment carries the pruned EROFS block (core+XATTR+SECURITY+ACL,
  ZIP/BACKED_BY_FILE explicitly off — no LZ4/XZ decompressor payload; only
  select-rider is XXHASH), golden refreshed (654 symbols, no drift),
  verify-config.sh asserts EROFS_FS/XATTR/SECURITY=y and ZIP=not-y.
- **Guest-verified**: debian and rust images from the fork mount on the pruned
  kernel; every hardlink group shares an inode with nlink=2 (perl pair ino
  verified); warm rust boot ~270-380 ms with mounts at 100-400 µs.

Remaining to ship at that point (the deliberate gates, all since closed):
1. Cut an ossein-kernel RELEASE from the updated fragment and bump the pin in
   ossein's Justfile — an erofs blob on the embedded (EROFS-less) kernel fails
   at boot with ENODEV, so the kernel must land first.
2. Flip the default codec "lz4" → "goerofs" in codecFromEnv (one line), demote
   tar+lz4 to fallback/env choice. The guest needs NO flip: it dispatches on
   the blob's magic.
3. Publish/tag forkcloser/erofs (or keep the local replace) and decide whether
   to offer Link() upstream.
4. Retire the tmpfs sizing note in pkg/container (upper-only now) and the
   ossein.rootfsmode=overlay bench knob once the flip lands.
Pre-existing, unrelated: tools/build-initfs/main.go carries 4 revive
unhandled-error lint hits that predate this work.

## Shipped (2026-08-02, kernel 7.1.5-ossein.2)

Gates 1–3 are closed:

- **Kernel**: 7.1.5-ossein.2 released with the pruned EROFS block; ossein's
  Justfile pin bumped (tag + sha256), cosign-verified, and the embedded
  artifact re-checked against the golden (654 symbols, EROFS asserted).
- **Fork published**: the module lives at **github.com/forkcloser/erofs**
  (pseudo-versioned; Writer.Link included) — ossein requires it directly, the
  local `replace` is gone.
- **Default codec flipped**: `goerofs` is the default. The guest needs no
  configuration: it dispatches on the blob magic. Both paths verified on the
  embedded .2 kernel (default → erofs mount, ~260 ms warm rust median; lz4 →
  extract, debian 51 ms copy-in).
- Tests updated: the cache round-trip test validates the default blob as
  walkable EROFS (superblock magic + fork reader); a serial lz4-codec test
  pins the tar+lz4 decoder round-trip. 97 tests green, lint clean.

**Codec set reduced (2026-08-02, post-flip).** Two of the four went, closing
the cleanup gate named below:

- `erofs` (mkfs.erofs) — REMOVED. It was the adoption-time differential
  oracle, and it did its job: the 4221-entry metadata diff and whole-tree
  content hash above are that comparison, run once, clean. What remains after
  a successful adoption is an unpinnable host dependency
  (brew install erofs-utils) that CI can never exercise, in a project that
  pins everything else. `TestGoEROFSFidelity` checks the writer against the
  source tar — ground truth, not a second opinion — and is the ongoing guard.
  Git history holds the ~70 lines if a second implementation is ever wanted.
- `none` (plain tar) — REMOVED. No test or benchmark referenced it. The
  guest keeps `FormatTar` as the classifier's unrecognized-blob fallback,
  which is not the same thing: nothing writes that format now, and a blob
  reaching that branch means the cache served something no codec produces.
- `lz4` — KEPT, as the A/B baseline the boot benchmarks compare against.

Still open (post-flip cleanup, deliberate): retire the
`ossein.rootfsmode=overlay` bench knob, which measured pure overlayfs
overhead on the extract path and has no purpose now that the overlay is the
product; revisit the `memory − 1 GiB` rootfs tmpfs sizing comment (the tmpfs
now holds only the overlay upper); measure the memory-pressure/evictability
upside when it matters.
