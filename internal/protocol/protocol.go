// Package protocol holds the constants the ossein HOST and the ossein GUEST must
// agree on exactly. They live here, in one portable package with no build tags
// and no platform imports, so that both worlds compile against the SAME
// declaration — the agreement is enforced by the type checker instead of by a
// lint recipe grepping two files for matching literals.
//
// A constant belongs here only if BOTH sides hardcode it independently and a
// mismatch breaks the pairing. Values the host merely computes and then hands to
// the guest at runtime — the stdio/copy-in vsock ports, the container rootfs
// path, the static network plan, the Rosetta share tag — are NOT protocol: the
// guest learns them from the wire, so there is nothing to keep in sync.
package protocol

// VsockPort is the guest vsock port serving the SandboxContext API: the
// guest listens on it as PID 1, the host dials it for the control channel. It is
// the one port neither side can be told about, since it is how they first meet.
const VsockPort uint32 = 1024

// Revision is the host↔guest protocol revision. The guest advertises it in its
// environment under RevEnvVar; the host's Dial handshake reads that back and
// refuses any other value, so an initfs built from a different revision is
// rejected instead of misbehaving subtly. Bump it whenever the wire semantics
// change — one edit here now covers both ends.
const Revision = "1"

// RevEnvVar is the guest environment variable carrying Revision. The handshake
// rides on the Getenv RPC — which every guest must implement anyway — so
// liveness and compatibility are proven in a single round-trip, with no
// dedicated version RPC.
const RevEnvVar = "OSSEIN_PROTO_REV"

// InitPath is where the guest binary lives inside the initfs. tools/build-initfs
// packs it at this path and the host boots with init=<InitPath> on the kernel
// cmdline; if the two ever disagreed the kernel would find no init and panic
// before anything could report why.
const InitPath = "/sbin/vminitd"
