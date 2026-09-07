# ossein's fork of Code-Hex/vz

`github.com/Code-Hex/vz/v3` — the Go binding for Apple's Virtualization
framework — carried in-tree with upstream [PR #205][pr] (vmnet networking)
applied and everything ossein does not use removed.

ossein imports it by its IN-TREE path — `github.com/farcloser/ossein/third_party/vz`
— not by the upstream path with a `replace`.

That distinction matters. A `replace` directive applies only to the MAIN
module: it is ignored when ossein is consumed as a library, so any importer
would resolve `github.com/Code-Hex/vz/v3` from the proxy, get upstream
WITHOUT PR #205, and fail to compile on `vz.NewVmnetNetworkDeviceAttachment`
and the missing `vmnet` package. Owning the path makes ossein self-contained:
what builds here builds for an importer.

[pr]: https://github.com/Code-Hex/vz/pull/205

## Why fork at all

ossein needs `VZVmnetNetworkDeviceAttachment` (macOS 26+), which lets it
create its OWN vmnet network per microVM instead of joining the host-wide
shared one.

That is not a nicety. `VZNATNetworkDeviceAttachment` silently attaches every
guest to a single system-wide network — one bridge, one subnet, one DHCP
server — whose configuration lives in root-owned system state
(`/Library/Preferences/SystemConfiguration/com.apple.vmnet.plist`,
`/etc/bootpd.plist`) that any other VM product can rewrite for everybody.
Observed 2026-08-01: a peer vmnet client renumbered the shared network from
192.168.64.0/24 to 192.168.138.0/23 and left bootpd with `dhcp_enabled` set
to false. Every ossein guest then failed to boot — no lease, no route — while
the bridge and the NAT engine kept working fine for the client that owned
them. A tenant on that network has no defence.

Owning the network also removed DHCP from the boot path entirely: because
ossein picks the network, the guest's address is known before the VM starts,
so it is applied statically over netlink (~2 ms, against the 85–300 ms a
lease acquisition cost). See `pkg/vm/network.go`.

## Why it cannot be a smaller vendor

The binding has to live INSIDE this package. `vz.NetworkDeviceAttachment`
has an unexported marker method:

```go
type NetworkDeviceAttachment interface {
	objc.NSObject
	fmt.Stringer
	networkDeviceAttachment()   // unexported: only this package can implement it
}
```

so no external package can produce a value that
`vz.NewVirtioNetworkDeviceConfiguration` will accept. Vendoring the module is
the only way to carry the addition until it ships upstream.

The alternative upstream offers — `vmnet/fileadapter`, pairing vmnet with the
released `FileHandleNetworkDeviceAttachment` — was rejected deliberately: it
forwards every packet through a Go userspace loop, which is the cost profile
ossein removed when it dropped gvisor-tap-vsock. `VZVmnetNetworkDeviceAttachment`
leaves the datapath in the framework.

## Ossein's changes on top of PR #205

Each edit is marked `OSSEIN FORK` in the source.

1. **`internal/osversion/virtualization_helper.h` — SDK gate is `#error`.**
   Upstream compiles the vmnet calls out on a pre-26 SDK and raises an
   Objective-C exception at RUNTIME, which for ossein means a VM that boots
   with no network from a build that looked fine. Now the build fails
   instead. Verified: `__MAC_OS_X_VERSION_MAX_ALLOWED` of 250000 fails,
   260000 compiles.
2. **`vmnet/vmnet_darwin.go` — version guards.** `IPv4Subnet` and
   `IPv6Prefix` gained the `macOSAvailable(26)` check every other entry
   point in that file already had.
3. **`socket.go` — `VirtioSocketListener` close/accept lifecycle.** Upstream
   `Close` pushed a sentinel onto the cap-1 `acceptch` and then closed it.
   Both halves are broken. The push deadlocks whenever a connection is already
   buffered — the framework delivered one nobody accepted — and `Close` is
   called from teardown paths, so the process hangs. Closing the channel turns
   any delivery still racing `Close` into a "send on closed channel" panic on a
   goroutine no `recover` covers, killing the host process. Buffered
   connections were also dropped without closing their file descriptors, and a
   second `Accept` after `Close` blocked forever because the single sentinel
   had already been consumed.

   Now: `Close` deregisters first (`removeSocketListenerForPort` is
   `dispatch_sync`'d onto the VM queue, the same serial queue the accept
   delegate runs on, so no delivery can appear afterwards), closes a dedicated
   `closed` channel — unbounded, idempotent, releases every waiter — joins
   in-flight deliveries on a `WaitGroup`, then drains and closes whatever was
   buffered before deleting the cgo handle. The delivery closure registers
   itself on the VM queue and does its blocking send from a goroutine, which
   is why `shouldAcceptNewConnectionHandler` now calls the handler
   synchronously instead of `go handler(...)`: the registration has to be
   ordered against `Close`, and spawning at the call site left it unordered.
   `Accept` selects on `closed` and returns the exported `ErrListenerClosed`.
4. **`socket.go` — `VirtioSocketConnection.CloseWrite`.** Upstream exposes
   neither a write shutdown nor the underlying conn, so anything relaying
   between a vsock hop and another socket could not propagate a half-close and
   had to tear the whole connection down — cutting off a peer mid-reply and
   breaking EOF-framed protocols through `ossein buildkit`'s socket proxy. The
   framework hands over a socket file descriptor that
   `newVirtioSocketConnection` already wraps with `net.FileConn`, whose
   concrete type has `CloseWrite`; it was only ever hidden behind an
   unexported field. `pkg/vm` asserts the method at compile time so a fork
   refresh that drops it fails the build instead of silently degrading every
   proxied connection.
5. **Three unused notification APIs removed, and with them a dependency.**
   `VirtualMachine.StateChangedNotify`,
   `VirtualMachine.NetworkDeviceAttachmentWasDisconnected` and the whole
   `NetworkBlockDeviceStorageDeviceAttachment` (NBD) family were the only
   users of `github.com/Code-Hex/go-infinity-channel`, a 92-line unbounded
   channel. None was reachable from ossein — it polls `State()` and attaches
   only local virtio-blk disks — but the channels were constructed EAGERLY per
   VM, so every boot spawned buffering goroutines and queued state transitions
   nobody would ever read.

   The Objective-C side is untouched, deliberately: its delegates still declare
   and call `emitAttachmentWasDisconnected`,
   `closeAttachmentWasDisconnectedChannel`, `attachmentDidEncounterErrorHandler`
   and `attachmentWasConnectedHandler`, so those four Go exports remain, as
   documented discards. Deleting them would mean editing the `.m` files, which
   is exactly the mechanical prune recorded below as having gone wrong. With no
   Go caller constructing an NBD attachment, its delegate is never instantiated
   and its two callbacks never fire in practice.

   `internal/sliceutil` went too — `watchDisconnected` was its only caller.
   `go-infinity-channel` left `go.mod` and depguard's allowlist in
   `.golangci.yml`.
6. **Whitespace normalized.** `just lint` runs `git-validation`'s
   `dangling-whitespace` rule over every commit, and it has no path
   exclusions: vendored code is held to the same rule as ossein's own. As
   imported, this tree carried trailing whitespace, blank lines at EOF, and a
   setext `=======` heading underline in `README.md` that git reports as a
   leftover conflict marker. All four are fixed; the README heading became
   ATX. Re-normalize after any fork refresh, or the whole lint gate goes red
   over code nobody is reviewing.
7. **Stripped to the bone** — see below.

## What was stripped, and why

Roughly 14,000 lines became ~7,500 (100 files to 58). Removed:

- **macOS guests entirely** — `MacPlatformConfiguration`, hardware models,
  machine identifiers, auxiliary storage, restore images, the installer and
  its progress observer, `MacOSBootLoader`, Mac graphics. ossein boots Linux.
- **The Cocoa GUI** — `virtualization_view.{m,h}`, `StartGraphicApplication`,
  the app-launch glue. ossein has no window.
- **Human-interface and multimedia devices** — audio, clipboard, graphics,
  keyboard, pointing devices, USB controllers and their ObjC callbacks.
- **Unused attachment families** — NAT, bridged, and file-handle network
  attachments (ossein uses vmnet only); USB mass storage and NVMe
  controllers (virtio-blk only).
- **Unused vmnet surface** — `fileadapter/`, `pktdesc`, the `vmnet_interface`
  lifecycle and packet-IO bindings, xpc serialization. ossein binds network
  creation, subnet readback, and the attachment.
- **Save/restore machine state**, tests, examples, `cmd/`, `testdata/`.

What remains was checked for reachability WITHIN this package: no declaration
is orphaned. That is a weaker bar than "ossein uses it", and the difference is
not small — `deadcode ./cmd/ossein` reports ~60 functions here unreachable from
ossein's main, among them the EFI bootloader, the memory balloon device,
`NewMultipleDirectoryShare`, the Rosetta socket-caching options and
`NewMACAddress`. Read that number with care: `deadcode` cannot see cgo, so
every `//export`ed callback is listed as unreachable while in fact being called
from Objective-C.

Those remaining unreachable declarations are deliberately kept. They are pure
Go with no dependency cost, and each deletion widens the diff against upstream
that this fork works to keep narrow. Change 5 above was the exception because
it bought something specific: an entire module out of the dependency graph.
That is the bar for removing more — a dependency, or a real hazard, not
tidiness.

The Objective-C side likewise carries functions with no Go caller. An attempt
to prune those mechanically removed live ones and was reverted, so it is
deliberately left alone: dead weight in the binary, not a correctness or review
hazard, and the C files are held close to upstream.

This is a floor, not a policy. **If ossein later needs something that was
stripped, take it back from upstream** — the removals are mechanical and the
provenance below says exactly which commit to lift from. Nothing here is a
judgement that these features are bad, only that carrying unreviewed code we
never call is worse than fetching it when it is wanted.

## Removing this fork

Upstream merging PR #205 is not obviously imminent, so treat this as
long-lived. If it does land in a release:

1. rewrite the imports back — in ossein's own code (`pkg/vm/*.go`) replace
   `github.com/farcloser/ossein/third_party/vz` with
   `github.com/Code-Hex/vz/v3`
2. `go get github.com/Code-Hex/vz/v3@<release>` and restore it to depguard's
   allowlist in `.golangci.yml`. Note upstream will bring back
   `go-infinity-channel`, removed here by change 5
3. delete `third_party/vz`, then `just lint && just test`

Changes 1, 2 and 5 above (the SDK gate, the version guards, and the unused-API
removal) are ossein policy
rather than upstream bugs, so re-apply them upstream or in `pkg/vm` — or open
PRs for them. Changes 3 and 4 are NOT policy: 3 fixes a hang and two panics
that exist in upstream today, and 4 unhides a capability the underlying
descriptor already has. Both belong upstream on their own merits and should be
offered as PRs; until they land, dropping this fork means carrying them
forward, and `pkg/vm`'s compile-time assertion will catch losing 4.

Refreshing the fork against a newer upstream instead: re-apply 1 through 6,
then re-strip, or keep upstream whole if the surface has become wanted.

## Provenance

- upstream: https://github.com/Code-Hex/vz
- PR: https://github.com/Code-Hex/vz/pull/205, vendored at head `e27a5fb`
- LICENSE (MIT) retained unmodified
- `.golangci.yml` excludes this tree from lint and formatting: it is held as
  close to upstream as possible so the diff stays readable
- the vendored code's import paths were rewritten from
  `github.com/Code-Hex/vz/v3/...` to `github.com/farcloser/ossein/third_party/vz/...`;
  its `go.mod`/`go.sum` were removed so it is part of ossein's module rather
  than a nested one. Its own dependency, `Code-Hex/go-infinity-channel`, is
  now a direct requirement of ossein

## Build constraints

Every Go file carries `//go:build darwin` (upstream leaves most of them
unconstrained and relies on `#cgo darwin` directives, which do not stop a
non-darwin toolchain from compiling the `import "C"` preamble). Without the
constraint, `go vet`, `golangci-lint` and `go test ./...` on the linux and
windows legs fail on `Foundation/Foundation.h: No such file or directory`.
A refresh from upstream must re-apply the tag.
