// One module, two build worlds, separated by build tags rather than by a module
// boundary, and by directory so the split is visible: the HOST binary (cmd/ossein
// over pkg/*) is darwin/arm64 + CGO because it links Virtualization.framework,
// while the GUEST binary (everything under guest/) is linux/arm64 and CGO-free.
// internal/ holds only what BOTH sides import — the wire types and the constants
// they must agree on. Nothing links both worlds: every non-portable file carries
// //go:build darwin or //go:build linux, so the linux-only deps below (vsock,
// netlink, capability) never reach the host binary and vz never reaches the guest.
module github.com/farcloser/ossein

go 1.27.0

require (
	connectrpc.com/connect v1.20.0
	github.com/alecthomas/kong v1.16.1
	github.com/diskfs/go-diskfs v1.9.4
	github.com/forkcloser/erofs v0.0.0-20260816062731-b38a91c663af
	github.com/google/go-containerregistry v0.21.9
	github.com/mdlayher/vsock v1.3.0
	github.com/moby/sys/capability v0.4.0
	github.com/mycophonic/primordium v0.8.1-0.20260816071930-93b4edabd29b
	github.com/opencontainers/runtime-spec v1.3.0
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/mod v0.40.0
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.45.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/anchore/go-lzo v0.1.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.8 // indirect
	github.com/elliotwutingfeng/asciiset v0.0.0-20260801111138-45c5fff54b41 // indirect
	github.com/forkcloser/blake3 v0.0.0-20260803064325-464007c72da9 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/google/go-licenses/v2 v2.0.1 // indirect
	github.com/google/licenseclassifier/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/lmittmann/tint v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mdlayher/socket v0.6.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/otiai10/copy v1.10.0 // indirect
	github.com/sergi/go-diff v1.2.0 // indirect
	github.com/sirupsen/logrus v1.10.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/ulikunitz/xz v0.5.16 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.opencensus.io v0.24.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/vuln v1.7.0 // indirect
	k8s.io/klog/v2 v2.90.1 // indirect
)

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	github.com/google/go-licenses/v2
	golang.org/x/tools/cmd/deadcode
	golang.org/x/vuln/cmd/govulncheck
)
