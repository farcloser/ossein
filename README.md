# Ossein
> The fastest macOS container builder in the solar system
> 
> (and minimalistic, rootless, daemonless, microVM-based container runtime)

## TL;DR

```
# just build embeds the verified guest kernel + initfs INTO the binary —
# fully self-contained, no artifact paths needed:
just build
time ./build/ossein run debian echo hi

# Local dev / bench A/B can override the embedded artifacts:
OSSEIN_KERNEL=/path/to/kernel-arm64 OSSEIN_INITFS=/path/to/initfs.ext4 ossein run debian echo hi
```

> `just build` is the only sanctioned build: `go install .../cmd/ossein@<ver>`
> can never work, deliberately — the //go:embed guest artifacts are produced by
> the harness (verified kernel fetch + in-repo initfs build) and are not part
> of the module zip.

## Motivation

Running buildkitd on macOS is not new.

Docker and OrbStack obviously offer it.
However, they are close source, moving targets, large system wide daemons that cannot be
pinned nor sealed into a project hermetic tooling.
Further, they are fairly large systems offering a lot more than just "build".

DIY-ers generally turn to lima, which is a great, extremely flexible project.
However, it requires a lot of yak-shaving to isolate and pin per-project.
It also runs full-blown linux systems, and like the other two, its feature surface is
much larger than "just build".

If what you want is indeed "just build", an unprivileged build tool that runs on demand,
is fast, ultra light-weight, can be pinned and sealed, with good isolation guarantees,
you are on your own.

Ossein was built to bridge that gap:
- everything can be pinned and sealed into per-project tooling (including the kernel)
- single unprivileged binary, no system daemon
- one-off micro-VM per build, in memory
- fast


## Usage(s)

The best and intended way to run ossein is through aqua.

```
cd myproject
aqua add ...

ossein build 

```

## Persistent build cache (per project)

The micro-VM is throwaway and its rootfs lives in memory, so by itself a build
keeps nothing. To make BuildKit's cache (content store, cache metadata,
snapshots) survive and warm the *next* build, ossein backs `/var/lib/buildkit`
with a per-project ext4 image on the host — created and formatted on first use
(pure Go, no `mkfs`), reused thereafter.

The cache is keyed by directory, so it stays scoped to the project like the rest
of the tooling:

```
cd myproject
eval "$(ossein buildkit --detach)"   # first run: creates the project's cache volume
# … build … (cache is populated)
ossein stop
eval "$(ossein buildkit --detach)"   # later: same dir → same cache, builds start warm
```

### Where the cache lives

By default the image is stored **centrally**, keyed by the project directory, so
it never touches your tree (no `.gitignore`, no Time Machine / cloud-sync churn
on a large growing file):

```
~/Library/Application Support/ossein/buildkit/<hash>/cache.img   # persistent, sparse
```

The `--cache` flag controls this — it takes either a **name** or a **path**:

| `--cache` value | where the cache lives |
|---|---|
| *(omitted)* | central, keyed by the current directory |
| a bare **name** (`--cache api`) | central, keyed by that name — pin a stable key across a moved/renamed checkout, or share one cache between directories |
| a **path** (`--cache ./.ossein/cache`, `/abs/path`) | **project-local** at that path (an auto-written `.gitignore` keeps the image out of version control) |

Run `ossein buildkit --print-cache [--cache …]` to print the resolved directory
without booting anything.

### Operational notes

- **One writer** — a cache volume is locked while in use; a second
  `ossein buildkit` for the same cache fails fast with an actionable message
  (`run ossein stop first, or use --cache <name>`) rather than corrupt it.
- **Clean shutdown flushes the cache** — `ossein stop` sends SIGTERM and waits up
  to `--grace` (default 60s) for buildkitd to shut down and the cache to sync to
  disk before escalating to SIGKILL. Bump it if a very large cache ever needs
  longer; lower it to force-kill a stuck instance sooner.
- **Reclaim** — caches are kept by design. `ossein gc --prune-cache` deletes the
  central ones not currently in use; plain `ossein gc` leaves them alone.
  Project-local caches live in your tree, so they're yours to manage.

