# Ossein — startup performance

> **HISTORICAL (superseded 2026-08-02).** Current numbers live in
> `BENCHMARKS.md` — the dated benchmark ledger. Everything below predates
> virtio-blk transport, per-VM vmnet, the initramfs boot, and EROFS
> materialization; keep it for the analysis narrative, not the figures.

## Observed (2026-07-13, ossein Go vminitd, warm image cache)

```
time ./build/ossein run debian echo hi   →   1.982 s total
```

Cold-VM startup for an instant command is **~2 s**. The two dominant costs the
previous version of this note chased are **gone**.

> **The old numbers were the Apple-init baseline.** This file used to report
> `run debian echo` at **~24 s** with a **~12 s** rootfs extraction and a **~10 s**
> unexplained post-boot tail (measured 2026-07-09 against the v1-containerization
> Apple vminitd). ossein's own Go vminitd superseded that: rootfs extraction is
> now **sub-second** and the post-boot tail is **~14 ms**. If you see the 12 s / 10 s
> figures anywhere, they are stale — delete them.

## Measured breakdown — `run debian echo hi`, 1.982 s total (2026-07-13)

Precise per-stage `dur=` (each `Boot` stage line now carries it — see Methodology).
`run debian echo hi`, Boot total **1.895 s** (wall 1.941 s):

| stage (`dur=`) | ms | what |
|---|---|---|
| **image resolve** (`→ image ready`) | **823** | `image.Resolve` → `remote.Get`: TLS + auth + manifest + config to docker.io — **every run, even when the rootfs is already cached** |
| VM config (`→ starting microVM`) | 20 | `vm.New` (was `netstack.Start` + `vm.New`; the userspace net stack is gone — see the 2026-07-30 note) |
| **VM boot + handshake** (`→ vminitd up`) | **814** | `machine.Start` (VZ create + arm64 kernel boot) + `guest.Dial` vsock handshake |
| rootfs extraction (`→ rootfs extracted`) | 236 | tmpfs mount + gunzip/untar debian (`copyInRootfs`) |
| guest network config (`→ boot complete`) | **1.2** | static netlink RPCs — **near-instant** |

> **Superseded 2026-07-30.** Networking moved from an in-process gvisor-tap-vsock
> stack with a static IP plan to Virtualization.framework's own NAT plus a
> userspace DHCP client in vminitd. The "guest network config" stage is no longer
> 1.2 ms: the guest must now acquire a lease it cannot predict. Re-measured on the
> new path, `--network` runs land at **~85-120 ms** for that stage (three runs:
> 122.0 / 94.4 / 85.5 ms), because the first DHCP discover is routinely lost —
> vmnet is not answering the instant PID 1 starts — and the client retransmits at
> 50 ms. The acquisition itself is started eagerly at init so it overlaps rootfs
> extraction; what remains in the stage is the tail of that wait.
| StartProcess | 16 | create OCI process + stdio vsock proxies |
| teardown | 1.6 | `vm_stop=1.6ms  agent_close=80µs  cache_flush=0s` |

**Two costs dominate, near-equal at ~43% each: image resolve (823 ms) and VM boot
(814 ms).** They are **sequential and independent** — the VM config needs nothing
from the resolved image — which is the whole opportunity (below). Everything else
is small: rootfs extraction is **236 ms** (not the old 12 s; not worth clonefile-
ext4 now), and guest network config / StartProcess / teardown are ≤16 ms combined
with the earlier estimate of "~1 s network config" flat wrong (it's ~1 ms).

## Status of the old optimization list

- **Rootfs re-extraction ("the big one", ~12 s)** — **RESOLVED.** Now **236 ms**
  (measured) to untar debian into tmpfs. See "what's left" #4: the clonefile-ext4
  plan would shave it, but 236 ms isn't worth that complexity now.
- **~10 s post-boot tail** — **RESOLVED.** StartProcess is **12 ms**, teardown
  **1.8 ms** (`vm_stop` 1.78 ms). Nothing to attribute; it was the Apple-init path.
- **Faster codec (gzip→zstd)** — **moot.** Sub-second extraction; not worth the
  guest-side decompressor churn.

## What's actually left — ranked by the measured ms

**The single biggest lever is that image resolve (823 ms) and VM boot (814 ms) run
sequentially and are independent.** `Boot` is `resolve → vm.New → Start → handshake
→ extract`, but `vm.New/Start/handshake` need *nothing* from the resolved image.

1. **Overlap image resolve with VM boot** (kick off `vm.New`+`Start`+`guest.Dial`
   concurrently with `image.Resolve`). The rootfs extraction *does* need the image,
   so it waits — but the ~814 ms of VM boot hides behind the ~823 ms resolve.
   **Expected: ~1.9 s → ~1.1 s** (`max(823, 814) + 236 extract + ~40 = ~1.1 s`).
   **The one big win.** Requires making `Boot` concurrent (resolve+`Rootfs()` prefetch
   on a goroutine via errgroup; join before `copyInRootfs`). The image bytes aren't
   touched until `copyInRootfs`, so correctness holds. Trade-offs to weigh when built:
   (a) the VM boots *before* the image is validated — a bad ref / auth / download error
   now wastes an ~814 ms boot (teardown is ~1.6 ms, cheap); (b) cold-pull `flatten()`
   (decompress + zstd) contends for the 2 vCPUs with vminitd boot, so the real win is
   less than the ideal `max(a,b)`; (c) the sequential per-stage `dur=` timing must be
   reworked into two tracks (image vs VM) + join-wait, else the breakdown misleads.
   Mostly matters on cold pulls / `--pull=always`; with `#2` warm runs are already fast.
2. **Offline resolve cache** — **DONE** (`run --pull`, default `missing`). `image.Resolve`
   used to do a full `remote.Get` (TLS + auth + manifest + config to docker.io) on
   every run, even with the rootfs cached — the whole 823 ms. Now `missing`/`never`
   read a cached `ref@platform → (digest, config)` and resolve fully offline (≈ 0);
   `always` keeps the old behavior. Removes the network from the warm path. This gets
   ~1.9 s → ~1.1 s on its own; combining with #1 removes network latency on cold runs
   too.
3. **VM boot + handshake (814 ms)** — VZ VM create + arm64 kernel boot + vsock dial.
   Mostly inherent, and hidden by #1 anyway. Only chase after #1/#2: sub-instrument
   `machine.Start` vs `guest.Dial` to see if the `ConnectRetry` poll or kernel boot
   params leave slack. This is the floor once resolve is dealt with.
4. **rootfs extraction (236 ms)** — modest now. The clonefile-into-ext4 plan (extract
   once per digest, `clonefile` + virtio-blk per run) would shave it, but 236 ms
   isn't worth that complexity unless images grow large.
5. **`buildkit --detach` doubles the rootfs work** — `detach` `warm(img)`s (drains
   the whole rootfs to `io.Discard`) in the foreground, then the child re-resolves
   and re-extracts. Skip `warm` when the digest is already cached. Buildkit path only.

## Methodology

`container.Boot` logs an INFO line per stage, each carrying **`dur=`** (ms since the
previous stage; `boot complete` also prints `total=`) — same convention as the
`start process timing` / `teardown timing` lines. So a single `time ossein run
debian echo` prints the exact per-stage attribution above. (The raw log timestamps
are only second-resolution — trust `dur=`, not the wall-clock deltas between lines.)
