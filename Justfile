# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# ossein links Virtualization.framework via Code-Hex/vz: CGO is mandatory, and the
# binary is darwin/arm64-only.
export GO_CGO := '1'

# go-licenses cannot locate the license of these two Apache-2.0 modules (the
# buildkit client's provenance types): in-toto/attestation's Go module lives in
# a subdirectory below the repository's LICENSE, and in-toto-golang trips the
# same module-layout detection (google/go-licenses#186). Both licenses were
# read from the module cache when this was added; re-check on a major bump.
export LINT_GO_LICENSES_FLAGS := '--ignore=github.com/in-toto/attestation --ignore=github.com/in-toto/in-toto-golang'

# ossein-kernel release embedded into the ossein binary (pkg/guestartifacts);
# non-semver, distro-kernel style tag. The pinned kernel MUST be built with
# CONFIG_BLK_DEV_INITRD=y: the guest boots a cpio via rdinit= with no root
# device, and a kernel without it hangs, surfacing only as a handshake timeout.
# Bump tag and sha256 together (fetch-kernel prints the real digest on mismatch).
guest_kernel_repo := "farcloser/ossein-kernel"
guest_kernel_tag := "7.1.5-ossein.2"
guest_kernel_sha256 := "e73700fdcb05673b18bcb7f9cbca2a46b89ad27f9631474c77c707aab4f828c2"
# Who must have signed the kernel: a REGEXP over the certificate's SAN, anchored
# on the GitHub user id. With keyless signing the email claim is mutable account
# state (the email-privacy toggle flips it between the bare address and the
# noreply form); the numeric id is not. Anything else fails on purpose. Pinning
# a release-workflow identity instead would remove this problem.
guest_kernel_identity := '^(142371135\+[^@]+@users\.noreply\.github\.com|apostasie@farcloser\.world)$'
guest_kernel_issuer := "https://github.com/login/oauth"

# buildkit image reference used by `ossein buildkit`, linked into the binary at build time.
# To bump: pick the release, then re-resolve the digest with
#   just buildkit-digest v0.33.0
buildkit_repo := "docker.io/moby/buildkit"
buildkit_tag := "v0.32.0"
buildkit_digest := "sha256:1f8167fcb0eca5b7126353d35299386945cbb8949cc516c592a49f80cfce4fa2"
buildkit_ref := buildkit_repo + ":" + buildkit_tag + "@" + buildkit_digest

# The FIRST recipe defined here becomes `just`'s default.
# Host and guest live in ONE module (see go.mod) but not one build: the shared Go
# stack (golangci, tidy-diff, vuln, licenses) analyzes what builds NATIVELY, which
# for a CGO project is darwin/arm64 only — so every linux-tagged guest package is
# invisible to it. The guest-* and tools-lint recipes are that missing half, and
# they are not optional extras: without them the PID-1 code ships unanalyzed and
# the linux-only bench tools are analyzed by nothing at all.
lint: _guest-artifacts do::lint::default do::lint::go::default do::lint::go::bce do::lint::go::escape do::lint::go::deadcode tools-lint
    {{ linux_env }} golangci-lint run {{ guest_pkgs }}
    # build/tools/govulncheck is built natively by the shared vuln leg this recipe
    # depends on; run it, not a PATH one.
    {{ linux_env }} build/tools/govulncheck {{ guest_pkgs }}

fix: do::fix::default do::fix::go::default
test: _guest-artifacts do::test::go::default guest-check

# linux_env is the cross-compile environment for every leg whose target is the
# microVM rather than the mac — the guest, and the linux-only bench tools.
# CGO_ENABLED=0 is stated rather than left to go's default: GO_CGO above declares
# this a cgo project for the HOST, and a linux cross-compile that inherited it
# would try to build runtime/cgo against the macOS SDK. Both targets are CGO-free.
linux_env := "CGO_ENABLED=0 GOOS=linux GOARCH=arm64"

# guest_pkgs are the linux-only packages: PID 1, the agent, and the stage2 runtime.
# Listed explicitly rather than ./... because ./... under GOOS=linux would also drag
# in the portable host packages the native legs already cover.
guest_pkgs := "./guest/..."

# Compile + vet gate for the guest. It is what CI can run without a Linux
# runner, NOT the whole story: `guest-test` below actually executes the guest's
# tests, and the two are separate recipes on purpose — this one is fast,
# hermetic and hard-fails CI; that one needs a bootable VM this machine may not
# grant (Virtualization.framework refuses processes without the entitlement,
# and no CI runner ossein uses has it).
#
# Anything a guest test can assert WITHOUT root or Linux belongs in a portable
# package instead, where `do test go` runs it on the mac — internal/rootfsblob
# is the worked example: the host writes the rootfs blob and the guest
# dispatches on its bytes, so the classifier lives in internal/ and both sides'
# tests exercise the same function.
guest-check:
    {{ linux_env }} go build {{ guest_pkgs }}
    {{ linux_env }} go vet {{ guest_pkgs }}

# Run the guest's own tests, inside a guest. Cross-compiles every guest package
# that has tests into a static test binary, drops them in one directory, and
# executes them in a single container: root, a real Linux filesystem, mount and
# mknod — the things the assertions are about.
#
# `--privileged` because these mount: the rootfs materialization tests stack a
# real overlay, which is the point of running them here rather than mocking the
# syscall. `--no-network` because nothing here talks to anything.
#
# Requires a working `just build` and a host that can boot a VM. Not wired into
# `test` for that reason — a green `just test` must not depend on the machine
# holding a virtualization entitlement.
guest-test dir=(justfile_directory() / "build/guest-tests"):
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf {{ dir }} && mkdir -p {{ dir }}
    # One binary per test-carrying package; `go list` finds them so a new test
    # file is picked up without editing this recipe. Both TestGoFiles (internal
    # tests) and XTestGoFiles (package_test) count — httpd's whole suite is the
    # latter, and keying on the first alone would have silently skipped it.
    # Quadrupling is just's escape for a literal opening brace pair; the
    # closing pair needs none.
    for pkg in $({{ linux_env }} go list \
            -f '{{{{if or .TestGoFiles .XTestGoFiles}}{{{{.ImportPath}}{{{{end}}' {{ guest_pkgs }}); do
        name="$(basename "$pkg")"
        echo "▶ building ${pkg}"
        {{ linux_env }} go test -c -o "{{ dir }}/${name}.test" "$pkg"
    done
    # The runner is a file rather than an inline -c so nothing has to survive
    # quoting through just, the shell, and ossein's argv. Quoted heredoc: this
    # is data for the GUEST's /bin/sh, expanded there and nowhere here.
    cat > {{ dir }}/run.sh <<'RUNNER'
    #!/bin/sh
    set -e
    for t in /t/*.test; do
        echo "▶ $t"
        "$t" -test.v
    done
    RUNNER
    chmod +x {{ dir }}/run.sh
    ./build/ossein run --privileged --no-network -v "{{ dir }}:/t" \
        docker.io/library/alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce /t/run.sh

# tools/bench is //go:build linux, so the native legs never load it. ./tools/...
# rather than the one package: whatever lands here next is covered by default.
tools-lint:
    {{ linux_env }} golangci-lint run ./tools/...

# Shared host↔guest constants (vsock port, protocol revision, init path) live in
# internal/protocol; never copy one into each side.

# The buildkit pin, stamped into the binary by the shared release build.
# BUILD_GO_LDFLAGS is spliced INSIDE that recipe's own -ldflags, last, so it adds to
# the version stamp and CGO linkmode instead of replacing them (which is what a
# -ldflags smuggled through BUILD_GO_FLAGS would have done).
export BUILD_GO_LDFLAGS := '-X main.buildkitImage=' + buildkit_ref

# cmd/ holds only the ossein product — the runtime and its docker-shaped front,
# cmd/ossein-docker — guest/ and tools/ are deliberately outside it so `do build go`
# builds exactly those two. Only ossein is entitled below: the front execs it
# rather than touching Virtualization itself.
#
# The binary is finished under a scratch name and swapped in with ONE rename.
# Never touch build/ossein in place: macOS SIGKILLs a running process whose
# mapped code changes under it ("Killed: 9", exit 137), and both steps here do
# exactly that when pointed at the live file — go's cached-link path copies
# into the existing inode, and codesign always rewrites in place. A bench or
# a sibling project (build-curl points its OSSEIN_BIN here) may be mid-run.
build: fetch-kernel _embed-initfs
    #!/usr/bin/env bash
    set -euo pipefail
    # Unlink first: a running instance keeps the old inode alive, and go then
    # creates a fresh file instead of copying into the live one.
    rm -f build/ossein build/ossein.new build/ossein-docker
    just do build go
    # Sign a copy and swap it in: codesign rewrites in place, and the fresh
    # (still unsigned) file may already have been exec'd by someone.
    cp build/ossein build/ossein.new
    codesign --force --sign - --timestamp=none --entitlements vz.entitlements build/ossein.new
    mv -f build/ossein.new build/ossein

# Resolve the digest a buildkit tag currently points at, for bumping buildkit_tag +
# buildkit_digest above. Anonymous Docker Hub pull token; prints the OCI index digest
# (the multi-arch one — ossein selects the per-platform child itself, since a --platform
# run may need linux/amd64 under Rosetta).
buildkit-digest tag=buildkit_tag:
    #!/usr/bin/env bash
    set -euo pipefail
    repo="{{ buildkit_repo }}"; repo="${repo#docker.io/}"
    token=$(curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" \
        | jq -r .token)
    curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors -o /dev/null -D - \
        -H "Authorization: Bearer ${token}" \
        -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
        "https://registry-1.docker.io/v2/${repo}/manifests/{{ tag }}" \
        | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: //p'

# Ensure the real embed artifacts exist for a compile (lint/test/go build), producing
# them only if absent. `just build` always refreshes them via its own deps.
_guest-artifacts:
    #!/usr/bin/env bash
    set -euo pipefail
    [ -f pkg/guestartifacts/kernel-arm64 ] || just fetch-kernel
    [ -f pkg/guestartifacts/initfs.cpio ]  || just _embed-initfs

# Build the initfs and place the real file where //go:embed bundles it.
_embed-initfs: initfs
    cp build/initfs.cpio pkg/guestartifacts/initfs.cpio

# Fetch + cosign/sha verify the pinned guest kernel and place it where //go:embed
# bundles it (pkg/guestartifacts/kernel-arm64). BUILD-TIME only — the runtime carries
# no network or cosign.
fetch-kernel:
    #!/usr/bin/env bash
    set -euo pipefail
    dest=pkg/guestartifacts/kernel-arm64
    base="https://github.com/{{ guest_kernel_repo }}/releases/download/{{ guest_kernel_tag }}"
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    for a in kernel-arm64 SHA256SUMS SHA256SUMS.cosign.bundle; do
        curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors -o "$tmp/$a" "$base/$a"
    done
    cosign verify-blob --bundle "$tmp/SHA256SUMS.cosign.bundle" \
        --certificate-identity-regexp "{{ guest_kernel_identity }}" \
        --certificate-oidc-issuer "{{ guest_kernel_issuer }}" "$tmp/SHA256SUMS"
    # Hash from the project root, not from a `cd "$tmp"`: pinned tools resolve from the working directory.
    actual=$(sha256sum "$tmp/kernel-arm64" | cut -d' ' -f1)
    signed=$(grep ' kernel-arm64$' "$tmp/SHA256SUMS" | cut -d' ' -f1)
    if [ "$actual" != "$signed" ]; then
        echo "kernel digest mismatch: signed manifest says $signed, got $actual" >&2
        exit 1
    fi
    # The in-repo digest pin: cosign proves WHO signed, this proves WHICH bytes —
    # a re-published tag or re-signed asset cannot slip through.
    if [ "$actual" != "{{ guest_kernel_sha256 }}" ]; then
        echo "kernel digest mismatch: pinned {{ guest_kernel_sha256 }}, got $actual" >&2
        echo "(bumping the kernel? update guest_kernel_sha256 in the Justfile)" >&2
        exit 1
    fi
    mkdir -p "$(dirname "$dest")"
    cp "$tmp/kernel-arm64" "$dest"
    echo ">> embedded guest kernel {{ guest_kernel_tag }} -> $dest"

# Build the guest initfs → build/initfs.cpio: cross-compile PID 1 static, build the
# host-side packaging tool, then pack the binary at /sbin/vminitd (matching the
# kernel's init= cmdline, so a fresh initfs is a drop-in for an unchanged host).
initfs:
    {{ linux_env }} go build -trimpath -ldflags "-s -w" -o build/vminitd ./guest/vminitd
    go build -o build/build-initfs ./tools/build-initfs
    build/build-initfs -in build/vminitd -out build/initfs.cpio

# Regenerate the Connect tree from the vendored proto (see proto/PIN). ONE proto, ONE
# output tree (internal/sandbox), imported by both the host client (pkg/guest) and
# the guest server (guest/internal/guestagent) — so there is no copy to keep in sync.
# The generated files are project-formatted (.golangci.yml sets
# formatters.exclusions.generated: disable), hence the trailing fmt.
proto:
    #!/usr/bin/env bash
    set -euo pipefail
    # protoc-gen-connect-go is pinned by the `tool` directive in go.mod, NOT by
    # aqua: the generator and the connect runtime must be the same version, and
    # one go.mod entry is the only pin that makes skew impossible. Built here
    # rather than installed, so nothing leaks onto the hermetic PATH.
    mkdir -p build
    go build -o build/protoc-gen-connect-go connectrpc.com/connect/cmd/protoc-gen-connect-go
    protoc -I proto \
      --plugin=protoc-gen-connect-go=build/protoc-gen-connect-go \
      --go_out=internal/sandbox --go_opt=paths=source_relative \
      --go_opt=MSandboxContext.proto=github.com/farcloser/ossein/internal/sandbox \
      --connect-go_out=internal/sandbox --connect-go_opt=paths=source_relative \
      --connect-go_opt=MSandboxContext.proto=github.com/farcloser/ossein/internal/sandbox \
      proto/SandboxContext.proto
    golangci-lint fmt

# ---------------------------------------------------------------------------
# Benches. Kernels and perf-arm64 come from ../ossein-kernel/build (build them
# there: `just kernel`); ossein's own bench artifacts stay in ./build.
# ---------------------------------------------------------------------------

# Where the sibling ossein-kernel project drops its build outputs (kernels + perf-arm64).
kernel_build := "../ossein-kernel/build"

build-bench:
    {{ linux_env }} go build -trimpath -ldflags "-s -w" -o build/bench ./tools/bench

# The cross-runtime benches compare against runtimes installed OUTSIDE the hermetic
# sandbox (apple-container in /usr/local/bin, podman in homebrew, docker/orbstack in
# ~/.orbstack/bin) — which limen's hermetic PATH deliberately hides, so their
# `command -v` gates silently skip them. These benches are inherently non-hermetic
# (that's the point), so restore the AMBIENT PATH for them. env_var reads just's
# inherited env (the hermetic override only applies to recipe shells), i.e. the real
# user PATH. The pure-ossein `bench` recipe stays hermetic — it needs no external CLI.
ambient_path := env_var('PATH')

# cross-runtime suite vs OrbStack + Docker Desktop, each with/without seccomp, plus
# ossein on the baseline kernel. All runtimes pinned to 2 visible CPUs. Kernels come from
# the ossein-kernel project (build them there first).
bench-external ossein="build/ossein" initfs="build/initfs.cpio" kernel=(kernel_build / "kernel-arm64") baseline=(kernel_build / "kernel-arm64.baseline"): build-bench
    PATH="{{ ambient_path }}" bash tools/bench-external.sh {{ ossein }} {{ initfs }} {{ kernel }} {{ baseline }}

# Runs the in-tree perf-arm64 built by ossein-kernel, copied in because the bench
# mounts THIS repo's build/. `nopatch` adds a second ossein row on the unpatched
# kernel — same binary/initfs/config, so the two rows isolate kernel/patches/.
bench-perf ossein="build/ossein" initfs="build/initfs.cpio" kernel=(kernel_build / "kernel-arm64") nopatch=(kernel_build / "kernel-arm64.nopatch"): build-wfeprobe
    @test -f {{ kernel_build }}/perf-arm64 || { echo "missing {{ kernel_build }}/perf-arm64 — build the kernel first: (cd ../ossein-kernel && just kernel)" >&2; exit 1; }
    cp -f {{ kernel_build }}/perf-arm64 build/perf-arm64
    PATH="{{ ambient_path }}" bash tools/bench-perf.sh {{ ossein }} {{ initfs }} {{ kernel }} {{ nopatch }}

# The tuning loop: bench-perf restricted to the two ossein kernels (the patched/nopatch A/B —
# what OUR change did) + orbstack (the reference to beat). Skips apple-container/docker/podman/
# lima, which are context rather than feedback and cost most of the wall-clock. ~4min vs ~15.
# (`just` recipe args are positional, so this is a recipe rather than a flag on bench-perf;
# the script itself just honours BENCH_FOCUS=1 if you prefer the env var.)
bench-perf-focus ossein="build/ossein" initfs="build/initfs.cpio" kernel=(kernel_build / "kernel-arm64") nopatch=(kernel_build / "kernel-arm64.nopatch"): build-wfeprobe
    @test -f {{ kernel_build }}/perf-arm64 || { echo "missing {{ kernel_build }}/perf-arm64 — build the kernel first: (cd ../ossein-kernel && just kernel)" >&2; exit 1; }
    cp -f {{ kernel_build }}/perf-arm64 build/perf-arm64
    BENCH_FOCUS=1 PATH="{{ ambient_path }}" bash tools/bench-perf.sh {{ ossein }} {{ initfs }} {{ kernel }} {{ nopatch }}

# Each runtime's `run` path: a kernel vmlinux compile in the container rootfs, no
# mount, COLD every iteration.
bench-run ossein="build/ossein" runs="5":
    PATH="{{ ambient_path }}" bash tools/bench-run.sh {{ ossein }} {{ runs }}

# Each runtime's image-BUILD path (ossein via buildkitd + buildctl), same workload.
bench-build ossein="build/ossein" runs="5":
    PATH="{{ ambient_path }}" bash tools/bench-build.sh {{ ossein }} {{ runs }}

# Cross-build the WFE probe → build/wfeprobe (linux/arm64, static). Answers whether the
# hypervisor traps WFE and whether a WFE-parked CPU wakes from a remote store at spin
# speed — the two facts that decide if IPI-free polling idle is possible in the guest.
# Also the canary: Apple guarantees nothing here, so a macOS update could change it.
# build/ossein run --cpus 4 -v "$PWD/build:/bench" debian /bench/wfeprobe
build-wfeprobe:
    {{ linux_env }} go build -trimpath -ldflags "-s -w" -o build/wfeprobe ./tools/wfeprobe
