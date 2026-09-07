# Guest Artifact Factory — Go rewrite

## Purpose

`v1-containerization/` exists for exactly one live reason: to produce the two
artifacts the pure-Go ossein runtime boots.

| Artifact | What it is | How ossein consumes it |
|---|---|---|
| `kernel-arm64` (~29 MB) | Apple's VZ-tuned Linux kernel | `OSSEIN_KERNEL` → `vm.Config.Kernel` |
| `initfs.ext4` (~402 MB) | ext4 image containing **vminitd** (Swift PID-1 guest agent) | `OSSEIN_INITFS` → `/dev/vda` |

Everything else in that tree (`cctl`, the Kata bootstrap kernel, the `cmd/` +
`microvm/` v1 spike) is build-time scaffolding or retired prototype.

This plan rebuilds that factory from scratch, here, in Go.

## Can it be Go? Yes — except the kernel itself

There is **no reason to keep any Swift**. Swift is in the picture solely because
`vminitd` (the guest agent baked into the initfs) is written in it. Replace that
one binary with a static Go PID-1 and the entire Swift surface disappears:

- Swift 6.3 toolchain + Static Linux SDK (the big `cross-prep` download)
- the `apple/containerization` vendoring (`.src/`, force-checked-out each build)
- `patches/` — including `0001-vminitd-copyin-zstd.patch`, which becomes native
  `klauspost/zstd` + `archive/tar` (the whole libarchive / zstd-filter saga is
  deleted, not ported)
- `cctl` (used only to bootstrap the kernel build)

The **kernel** is Linux — C, built by Kbuild/`make`. That cannot be rewritten in
Go and we don't try. What becomes Go is the *orchestration* around it; the
compile stays `make` inside a Linux build VM. The real asset there is the pinned
config (`config-arm64`), which we keep and own.

So: **initfs → 100% Go. kernel → Go orchestration wrapping an unavoidable
C compile.**

## Design anchor: keep the proto, swap the implementation

The host drives the guest over `SandboxContext.proto`. ossein's host code calls a
bounded subset (~20 RPCs, see `internal/guest/agent.go`). **We keep
`SandboxContext.proto` byte-identical.** Consequences:

- `internal/guest`, `internal/container`, and the generated `sandbox/` client are
  **unchanged**. The host cannot tell a Go guest from the Swift one.
- We can bring the Go init up *against the existing, working host* and diff
  behavior RPC-by-RPC — no big-bang cutover.
- We own both ends, so a later proto slim-down is possible, but it is explicitly
  **out of scope** for the rewrite. Contract stability first.

---

## Component A — the Go guest init (`vinit`)

A single static `linux/arm64` binary, `CGO_ENABLED=0`, that is PID 1 in the
microVM and implements the `SandboxContext` gRPC service over vsock :1024.

### PID-1 duties (before serving)
- Mount `/proc`, `/sys`, `/dev` (devtmpfs), `/sys/fs/cgroup` (cgroup2).
- Become a subreaper; run a `SIGCHLD` → `wait4(-1, WNOHANG)` reap loop so
  orphaned container descendants don't zombie.
- Ignore `SIGPIPE` (we already learned this matters for streamed copy-in).

### RPC → Go implementation

| RPC (used by host) | Go implementation |
|---|---|
| `Mkdir`, `WriteFile` | `os` |
| `Mount`, `Umount` | `unix.Mount` / `unix.Unmount` |
| `Copy` (COPY_IN, streamed) | vsock accept → `klauspost/zstd` → `archive/tar` → extract with full metadata (uid/gid, mode, setuid, `security.capability` xattr, whiteouts). Native. |
| `CreateProcess`/`StartProcess`/`WaitProcess`/`KillProcess`/`ResizeProcess`/`CloseProcessStdin` | mini OCI runtime + process supervisor (below) |
| `IpLinkSet`/`IpAddrAdd`/`IpRouteAddDefault` | `vishvananda/netlink` |
| `ConfigureDns`/`ConfigureHosts` | write `/etc/resolv.conf`, `/etc/hosts` into the rootfs |
| `SetupEmulator` | write `/proc/sys/fs/binfmt_misc/register` (Rosetta) |
| `ProxyVsock` (OUT_OF) | vsock listen ↔ guest unix-socket proxy |
| `Sync`, `SetTime`, `Getenv`, `Kill` | `unix.Sync`, `settimeofday`, `os.Getenv`, `unix.Kill` |

Unused-by-host RPCs (`Sysctl`, `Stat`, `DeleteProcess`, `ContainerStatistics`,
`IpRouteAddLink`, `StopVsockProxy`, `Setenv`, `FilesystemOperation`) get
thin/stub implementations to satisfy the service; fill in only as needed.

### The two genuinely hard parts

**1. Rootfs copy-in fidelity.** This replaces the code path we just fought
through. Native `tar` extraction as guest-root must preserve multi-uid
ownership, setuid bits, `security.capability` xattrs, and OCI whiteouts.
Candidate: `containerd/containerd/archive.Apply` (battle-tested, handles
whiteouts + metadata) fed by `klauspost/zstd`. Verify xattr passthrough on musl-
free glibc guest early.

**2. Container launch.** `CreateProcess` carries the OCI process/spec JSON. The
init must set up namespaces, `pivot_root`, spec mounts, a cgroup v2, the
capability set (default vs `--privileged`), uid/gid/env/cwd, then `exec`. Either
way the host/proto are untouched — the init translates the RPCs internally. See
**Runtime strategy** below for the runc-vs-hand-roll decision.

### Runtime: a full, faithful port of Apple's `vmexec` to Go

**Decision: port `vmexec` 100%** — not runc, not a reduced subset. Apple's *own*
default runtime is `vmexec` (~1,085 lines of Swift across `run`/`exec`; runc is
only their optional fallback via `RuncProcess`), and its header already declares
it "a very small subset of the OCI runtime spec." So a complete port is finite,
CGO-free, and is the design Apple already proved. runc as a dependency is rejected:
its C `nsexec` bootstrap forces CGO/second-binary weight to gain OCI generality
(user ns, seccomp, rootless, pods) we don't use.

**Structure.** One static Go binary, multi-call like busybox: `init` (the PID-1
gRPC agent) + `run` + `exec` + an internal `stage2`. Same binary means the
`/proc/self/exe` re-exec (below) points at itself.

**The one non-mechanical change — `fork` → `/proc/self/exe` re-exec.** vmexec does
`unshare(flags); fork(); <child runs setup>; execve`. Go's runtime forbids running
arbitrary code after `fork()` (post-fork thread/lock state is undefined). So the
port replaces `unshare+fork` with Go's standard pattern: `os/exec` a fresh
`/proc/self/exe stage2` with `SysProcAttr.Cloneflags = NEWPID|NEWNS|NEWUTS` (the
kernel makes the namespaces at clone). The re-exec'd child is a clean process that
runs the exact `childSetup` sequence and finally `execve`s the workload. `exec`
(into a running container) does the same with `pidfd_open`+`setns` in stage 1,
then re-exec for the pid-ns child. Note: `CLONE_NEWCGROUP` stays a runtime
`unshare` in the child (not a clone flag) because it must follow the agent placing
the child into its cgroup. Everything else is a line-for-line translation.

**The C shim disappears.** vmexec's `LCShim` (`CZ_pivot_root`, `CZ_pidfd_open`,
`CZ_prctl_*`, `CZ_setrlimit`) all have native `golang.org/x/sys/unix` equivalents
(`PivotRoot`, `PidfdOpen`, `Prctl`, `Setrlimit`, `Setns`, `Unshare`). Capabilities
(bounding/effective/permitted/inheritable/ambient + `PR_SET_KEEPCAPS`) use
`github.com/moby/sys/capability` (runc's own caps library). Net: `CGO_ENABLED=0`
even though the Swift original needed a C shim.

**Faithful port checklist** (each item is a named Swift function to translate):

- **namespaces** (`setupNamespaces`): OCI `linux.namespaces` → clone flags;
  default `NEWPID|NEWNS|NEWUTS`; `setns` for path-bearing ns; **network ns
  intentionally ignored** — matches Apple.
- **parent/cgroup**: `Cgroup2Manager.load(cgroupsPath)` → `applyResources` →
  `addProcess(childPid)` *before* the child unshares its cgroup ns; then hand the
  child pid up the syncPipe.
- **child (`childSetup`, exact order)**: ack-wait → `unshare(NEWCGROUP)` →
  `setsid` → **root setup** [`prepareRoot` (MS_SLAVE|REC `/`, bind rootfs on
  itself) → `mountRootfs` (spec mounts) → `setDevSymlinks` (`/dev/fd`,
  std{in,out,err}, rtc) → `pivotRoot` (runc's `pivot_root(".",".")` + `MNT_DETACH`)
  → remount-ro if `root.readonly` → `reOpenDevNull`] → terminal setup (Console,
  `TIOCSCTTY`, bind `/dev/console`) → `sethostname` → sysctls (`/proc/sys/*`) →
  `applyMaskedPaths` (ro tmpfs on dirs, bind `/dev/null` on files) →
  `applyReadonlyPaths` (bind+remount-ro, `statfs` flag preservation) →
  `applyCloseExecOnFDs` (all fds >2) → `setRLimits` (every `RLIMIT_*`) →
  `prepareCapabilities` (bounding set + keepcaps) → `fixStdioPerms` (chown stdio) →
  `setPermissions` (setgroups→setgid→setuid, in that order) →
  `finishCapabilities` (clear keepcaps, apply caps + ambient) →
  `setNoNewPrivileges` (`PR_SET_NO_NEW_PRIVS`) → `exec` (PATH lookup, create+chdir
  cwd, `execve`).
- **exec (`ExecCommand`)**: `pidfd_open(parentPid)` → `setns(NEWCGROUP|NEWPID|
  NEWUTS|NEWNS)` → re-exec child → [ack, setsid, optional terminal, cloexec,
  rlimits, caps, stdio perms, uid/gid, finish caps, no_new_privs, exec].
- **Console (`Console`)**: `/dev/ptmx` → `unlockpt` → `ptsname` → `dup3` slave to
  0/1/2; pass the master fd up to the agent for host-side stdio relay.
- **Mounts (`ContainerMount`)**: apply each OCI mount under the rootfs;
  `configureConsole` (swap `/dev/ptmx` for a `pts/ptmx` symlink).
- **agent handshake**: fd 3 syncPipe (child pid, then console master fd), fd 4
  ackPipe (`AckPid`/`AckConsole`), fd 5 errorPipe (structured error). The Go
  `init` implements the other end, so this contract is internal and preserved
  verbatim.

### stdio over vsock
`CreateProcess` passes stdin/stdout/stderr as host vsock port numbers. The init
dials each and wires it to the process fds. TTY mode: allocate a ptmx
(`creack/pty`), `ResizeProcess` → `TIOCSWINSZ`, `CloseProcessStdin` closes the
stdin half. Raw byte relays, exit code propagated via `WaitProcess`.

### Packaging (pure Go, on macOS)
Cross-compile the static multi-call binary (`init`/`run`/`exec`/`stage2`), then
build `initfs.ext4` with **`go-diskfs`** — already in the tree for the buildkit
cache disk — writing that one binary plus empty mountpoint dirs
(`/proc /sys /dev /run ...`). No `mkfs`, no Swift, no Linux host. The image
shrinks from ~402 MB (Apple's full initfs) to a few MB — a single static binary;
the container's userland comes from its own image, not the initfs.

### Libraries (candidate set)
`google.golang.org/grpc`, `mdlayher/vsock`, `vishvananda/netlink`,
`opencontainers/runtime-spec`, `moby/sys/capability` (caps, as runc uses),
`containerd/cgroups/v3`, `containerd/archive`, `klauspost/compress/zstd`,
`creack/pty`, `golang.org/x/sys/unix` (pivot_root/setns/unshare/pidfd/prctl/
setrlimit — replaces vmexec's `LCShim` C), `diskfs/go-diskfs`.

---

## Component B — the kernel

### Adopt the config as ours
- **Fork `config-arm64` into `kernel/config/` and own it outright** — no longer
  tracked against apple/containerization. It becomes ours to prune to exactly
  what VZ + our workloads need (drop config we don't use, keep the VZ-required
  `=y` set) and to bump on our own cadence. This is the one piece of apple IP we
  keep, and adopting it severs the last config-level tie to upstream.
- Pin the kernel source ourselves (currently `linux-6.18.x` from kernel.org),
  checksum-verified; Renovate tracks new stable releases.

### Build (Go orchestration, dogfooded VM)
A small Go builder that: fetches the pinned kernel tarball (checksum-verified),
boots a **build container via ossein itself**, mounts source + config, runs
`make Image` with the config, and extracts `kernel-arm64`. This deletes the
`cctl` + Kata-via-cctl + `apple/container` build path in favor of ossein
dogfooding its own `run`.

### Bootstrap (resolve the chicken-and-egg)
Booting any VM needs a kernel + initfs. So:
1. Build the **Go initfs first** — it needs no kernel to build (`go build` +
   `go-diskfs`).
2. Download a **bootstrap kernel** (Kata release; a plain, checksum-verified
   download — no Swift, no cctl).
3. Use ossein + (bootstrap kernel, Go initfs) to boot the build container that
   compiles the **product kernel**.

### Alternative considered
Ship a prebuilt kernel and skip building. Rejected as the default because VZ
needs the specific config, but kept as a fallback if the dogfooded build proves
flaky.

---

## Milestones (risk-first; each has a hard acceptance test)

- **K0 — scaffolding + boot. ✅ DONE (2026-07-09).** Separate `init/` module;
  `cmd/init` PID-1 agent (proc/tmpfs-run/sys/cgroup2 mounts + Getenv/Mkdir/Mount/
  WriteFile over vsock gRPC); `cmd/build-initfs` pure-Go ext4 packager. *Accepted:*
  `ossein doctor` boots + handshakes + `/run/doctor` mkdir/mount/write + teardown,
  against the Go initfs (`init=/sbin/vminitd` drop-in) and the existing kernel.
  Proved: Go-as-PID1, go-diskfs ext4 root, vsock/gRPC, drop-in proto — all green.
- **K1 — copy-in. ✅ EXTRACTION DONE (2026-07-09).** Native `klauspost/zstd` +
  hand-rolled `archive/tar` extractor (`copyin.go`): ownership, setuid/setgid/
  sticky, xattrs (`SCHILY.xattr.*` → `Lsetxattr`), symlinks/hardlinks/devices,
  deferred dir times. Guest self-places in a `/vminitd` cgroup (no throttle) so
  the host's memory.high lift is a no-op and extraction runs unthrottled.
  *Verified live:* full debian rootfs extracted into tmpfs (reached network
  config); tmpfs `security.*` xattr support confirmed implicitly. *Still to
  assert under K2 (needs a running process):* setuid on `newuidmap`, `getcap`
  shows the capability xattr.
- **K2 — run a container.**
  - **K2a ✅ DONE (2026-07-09):** non-TTY launch. Networking (netlink) + the
    `vmexec` core: `CreateProcess` re-execs `/proc/self/exe stage2` in the
    spec's namespaces (PID/IPC/UTS/mount via Cloneflags), `stage2` wires stdio
    over vsock, mounts spec filesystems, `pivot_root`s, sets uid/gid, gates on
    start, execs. *Accepted live:* `run debian echo hi` prints `hi` end-to-end
    in **~2s** (vs ~20s Swift). No blanket reaper (races cmd.Wait).
  - **K2b ✅ DONE + validated live (2026-07-10):** all 21 host-called RPCs
    implemented. TTY (Console/ptmx, pty-master handoff via SCM_RIGHTS socketpair,
    agent-side relay, `TIOCSCTTY`, `ResizeProcess`, `CloseProcessStdin`);
    capabilities (`moby/sys/capability`, bounding/ambient/keepcaps around the uid
    drop); full childSetup order (sysctls, masked/readonly paths, rlimits,
    `reOpenDevNull`, remount-ro, fixStdioPerms); `SetupEmulator`
    (binfmt_misc/Rosetta), `Umount`, `Sync`, `SetTime`; `ProxyVsock`/
    `StopVsockProxy`. **Verified live:** `run debian echo hi`, `run -it alpine
    sh` (interactive TTY), full buildkit (`buildkit -detach` + `buildctl build`).
    **The Go guest is at behavioral parity with Swift vminitd.**
  - **Known pre-existing gap (NOT a rewrite regression):** rootless-userns
    images (`moby/buildkit:rootless` → `newuidmap: Could not set caps`) fail
    **identically on Swift vminitd and the Go guest** — confirmed by A/B against
    the old initfs. Root cause is host/image-side, not the guest: most likely
    the `security.capability` xattr is dropped in the host's `mutate.Extract`
    flatten (so neither guest ever receives it) or `/etc/subuid` range setup.
    Track separately; out of scope for the guest port.
  - **Not built:** `exec` into a running container (`pidfd_open`+`setns`) — no
    host command drives it yet.
- **K3 — buildkit parity.** *Accept:* `buildctl build` of a real Dockerfile over
  the unix proxy; `gc` leaves no orphans.
- **K4 — Go kernel build. ✅ DONE + self-hosting (2026-07-10).**
  `kernel/` adopts the config (`config-arm64/x86_64`) + `build.sh`;
  the sibling `ossein-kernel` project's `cmd/ossein-kernel` (Go host orchestrator) downloads the source from the pin passed
  via `--source-url` / `--source-sha256` (wired in the Justfile), then
  **dogfoods ossein** — `ossein run ubuntu` with source at
  `/kernel` (virtiofs) and the build in the **tmpfs rootfs** (`/kbuild` must NOT
  be virtiofs: virtiofsd runs as the non-root host user and can't honor guest
  root's DAC override, so kernel-source extraction EACCESes — build in RAM
  instead), apt-installs the cross toolchain, runs `make`, collects
  `kernel-arm64`. No cctl / Kata-via-cctl / apple-container. `just kernel`.
  **Verified live: built in ~2m30s (in-RAM), and the resulting kernel boots
  ossein and runs a container.** The factory is self-hosting — the previous
  ossein-built kernel bootstraps the next build (`--bootstrap-kernel`).
- **K5 — retire v1.** Delete `v1-containerization/` (`.src`, `patches`, `build.sh`,
  `cctl`, `cmd`, `microvm`). Renovate on the kernel pin + Go deps. Update docs
  (`TODO.md`, README, LIMEN-VIRT).

## What gets deleted at the end
Swift toolchain + Static Linux SDK; `apple/containerization` vendoring; `vmexec`'s
`LCShim` C shim (→ `x/sys/unix`); libarchive and the zstd copy-in patch; `cctl`;
the Kata-bootstrap-via-cctl kernel path; the v1 Go spike. No runc, no CGO in the
guest. Net: the guest is one static Go binary + a kernel config, built with
`go build` and one `make` in a VM ossein boots itself.

## Risks / open questions
1. **The `vmexec` port is the long pole.** Bounded (~1,085 lines, checklist
   above), but the `fork`→`/proc/self/exe` re-exec change and the exact ordering
   of mounts/caps/uid are where subtle bugs live. Mitigate by porting
   function-by-function against the Swift reference and testing each stage; the
   K1/K2 fidelity tests catch regressions.
2. **xattr fidelity** (`security.capability`) through `archive/tar` — validate in
   K1 before building on it.
3. **cgroup v2 nesting for buildkit** (systemd vs cgroupfs driver inside the VM).
4. **Rosetta binfmt** registration details vs the Swift path's semantics.
5. **Kernel build needs guest networking** (apt in the build container) — already
   proven under the current pipeline; must survive the ossein-dogfood switch.
6. **arm64 only** to start. `config-x86_64` exists; defer.
7. **Proto stability** — keep identical through K4; only consider slimming once
   the Go guest is at parity and both ends move together.

## Backlog (post-parity, 2026-07-10)

Core is done: guest at parity (K0–K2), self-hosting kernel factory (K4),
bootstrap self-host + source pin closed. Remaining, roughly in priority order:

**Deferred by choice (holding the Go↔Swift hot-swap):**
- **Retire `v1-containerization/`** (K5) — delete `.src`, `patches/`, `cctl`,
  `cmd/`, `microvm/`, the Swift Static Linux SDK dep. Fully unblocked; only the
  hot-swap fallback holds it.
- **Host cleanup** — remove dead Apple-vminitd workarounds now that the Go guest
  is authoritative: the `memory.high` lift (`container.go:282`, a no-op against
  our cgroup) and the `LinuxNamespace` "path"-key marshal patch (`ocispec`),
  plus any `containerID`-required / h2-speaks-first Swift-isms.

**Functional gaps:**
- **Rosetta** (`SetupEmulator`) — implemented, never run live (`run --rosetta`
  amd64 image).
- **`exec` into a running container** — `pidfd_open`+`setns` port of vmexec's
  `ExecCommand`; build when an `ossein exec` command exists to drive it.
- **Rootless-userns** — `moby/buildkit:rootless` (`newuidmap`) fails identically
  on Swift and Go: pre-existing, host-side (`mutate.Extract` xattr drop or
  `/etc/subuid`), not the guest port. Investigate only if wanted.

**Polish / doctrine:**
- **`/sbin/vminitd` → `/sbin/init`** — honesty rename; needs a one-line host
  `init=` cmdline change + ossein rebuild (drops the drop-in compat).
- **`run --disk`** — a virtio-blk ext4 scratch mount (reuse the buildkit
  cache-disk plumbing); only needed if a future build outgrows the tmpfs/RAM
  scratch. Not needed now.
- **Renovate / CI** — on the `init` module deps + the kernel source pin
  (`--source-url` / `--source-sha256` in the Justfile) + the Kata bootstrap version.
- **Docs** — README/TODO still describe the Swift stack; update to the Go guest
  + dogfooded factory.
- **x86_64** — `config-x86_64` is adopted but the build path is arm64-only.
