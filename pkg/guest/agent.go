// Package guest is the typed client for the ossein guest agent — the PID-1 init
// inside the microVM (guest/vminitd + guest/internal/guestagent) — over its
// Connect-on-vsock control channel. The wire contract is
// proto/SandboxContext.proto, vendored verbatim from apple/containerization at
// the ref in proto/PIN, and both ends import ONE generated tree
// (internal/sandbox), so the wire types cannot drift between them.
package guest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/farcloser/ossein/internal/protocol"
	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

// handshakeTimeout bounds Dial's readiness probe and is the sole authority on
// boot-handshake patience: the vsock dialer underneath retries under this
// context, so it must exceed any worst-case guest boot.
const handshakeTimeout = 30 * time.Second

// baseURL is the Connect endpoint. The host part is inert — the transport's
// dialer ignores the address entirely and hands back a vsock connection — but
// net/http still demands a syntactically valid absolute URL.
// unsecure-url-scheme is inapplicable here: this URL is never resolved and
// never leaves the process. The transport's dialer discards the address and
// returns a vsock connection to the VM on the same machine, so there is no
// network hop to protect and no TLS peer to name.
//
//revive:disable-next-line:unsecure-url-scheme
const baseURL = "http://vsock"

// Agent is a connected guest-agent client. It speaks two services over one
// http.Client for the vendored SandboxContext contract.
type Agent struct {
	c    sandboxconnect.SandboxContextClient
	http *http.Client
}

// Dial connects to the guest agent through connect (a factory producing fresh
// vsock conns, since net/http opens a new connection per concurrent call and
// may redial), then verifies liveness AND protocol
// compatibility in one round-trip: the guest echoes protocol.Revision back
// through protocol.RevEnvVar, and anything else is refused. The default embedded
// initfs is build-time paired with this binary, so a mismatch only happens — by
// design — when --initfs/OSSEIN_INITFS substitutes a foreign guest.
func Dial(ctx context.Context, dial func(ctx context.Context) (net.Conn, error)) (*Agent, error) {
	// HTTP/1.1 deliberately, with no h2c: this contract is unary plus ONE
	// server-stream (Copy), which Connect carries over chunked HTTP/1.1. Only
	// bidi streaming would require HTTP/2, and none exists here — so the client
	// never links it.
	//
	// Concurrency is the transport's job, and that is what makes this work at
	// all: WaitProcess blocks for the container's entire life, and net/http
	// simply opens another connection for Kill/Resize/CloseProcessStdin instead
	// of queueing them behind it. Under a hand-rolled single-conn protocol the
	// same pattern would deadlock Ctrl-C.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		// Every "connection" is a fresh vsock dial to the one peer: a proxy has
		// no address to act on, and compression only burns guest CPU on a
		// memory-speed link.
		Proxy:              nil,
		ForceAttemptHTTP2:  false,
		DisableCompression: true,
	}

	client := &http.Client{Transport: transport}

	agent := &Agent{
		c:    sandboxconnect.NewSandboxContextClient(client, baseURL),
		http: client,
	}

	pingCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	rev, err := agent.Getenv(pingCtx, protocol.RevEnvVar)
	if err != nil {
		agent.http.CloseIdleConnections()

		return nil, fmt.Errorf("guest handshake: %w", err)
	}

	if rev != protocol.Revision {
		agent.http.CloseIdleConnections()

		return nil, fmt.Errorf("%w: guest speaks proto rev %q, host requires %q — "+
			"the initfs does not match this ossein binary", ErrAgent, rev, protocol.Revision)
	}

	return agent, nil
}

// Close shuts the control channel down. There is no single connection to close
// any more: the transport owns a pool of vsock conns, so closing means dropping
// all of them. Nothing here can fail, but the error return is kept — callers
// defer it, and the signature is the package's contract.
func (a *Agent) Close() error {
	a.http.CloseIdleConnections()

	return nil
}

// --- filesystem ---

// Mkdir creates a directory in the guest (mkdir -p when all is set).
func (a *Agent) Mkdir(ctx context.Context, path string, all bool, perms uint32) error {
	_, err := a.c.Mkdir(ctx, connect.NewRequest(&pb.MkdirRequest{Path: path, All: all, Perms: perms}))

	return wrap(err, "mkdir %s", path)
}

// Mount performs a mount(2) in the guest root namespace.
func (a *Agent) Mount(ctx context.Context, typ, source, destination string, options []string) error {
	_, err := a.c.Mount(ctx, connect.NewRequest(&pb.MountRequest{
		Type: typ, Source: source, Destination: destination, Options: options,
	}))

	return wrap(err, "mount %s on %s", source, destination)
}

// Umount unmounts a guest path.
func (a *Agent) Umount(ctx context.Context, path string) error {
	_, err := a.c.Umount(ctx, connect.NewRequest(&pb.UmountRequest{Path: path}))

	return wrap(err, "umount %s", path)
}

// WriteFile writes data to a guest path, creating parents as needed.
func (a *Agent) WriteFile(ctx context.Context, path string, data []byte, mode uint32) error {
	_, err := a.c.WriteFile(ctx, connect.NewRequest(&pb.WriteFileRequest{
		Path: path, Data: data, Mode: mode,
		Flags: &pb.WriteFileRequest_WriteFileFlags{CreateParentDirs: true, CreateIfMissing: true},
	}))

	return wrap(err, "write %s", path)
}

// CopyInArchive has vminitd extract a tar+lz4 archive — the rootfs blob the
// host attached as a read-only virtio-blk disk at VM creation — from device
// into path, as guest root: full Linux metadata fidelity with zero extra
// guest binaries and no host-side data path.
func (a *Agent) CopyInArchive(ctx context.Context, path, device string) error {
	// Child context: the deferred stream.Close below is what normally releases
	// the stream, but Close only drains the response body — it cannot unblock a
	// Receive parked on a guest that has gone silent. Cancelling on return kills
	// the underlying request too, so an early COMPLETE does not pin the
	// connection until the boot ctx dies.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := a.c.Copy(streamCtx, connect.NewRequest(&pb.CopyRequest{
		Direction:     pb.CopyRequest_COPY_IN,
		Path:          path,
		CreateParents: true,
		Device:        device,
		IsArchive:     true,
	}))
	if err != nil {
		return wrap(err, "copy-in %s", path)
	}

	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetError() != "" {
			return fmt.Errorf("%w: copy-in %s: guest reported: %s", ErrAgent, path, msg.GetError())
		}

		if msg.GetStatus() == pb.CopyResponse_COMPLETE {
			return nil
		}
	}

	// Receive returned false: either the stream failed, or it ended cleanly.
	if err := stream.Err(); err != nil {
		return wrap(err, "copy-in %s", path)
	}

	// A clean end WITHOUT COMPLETE means the guest handler bailed without
	// reporting — never treat that as a successful rootfs copy.
	return fmt.Errorf("%w: copy-in %s: guest closed the stream before COMPLETE", ErrAgent, path)
}

// --- processes ---

// StdioPorts are host-side vsock listener ports the guest connects back to
// for each stream; nil means the stream is not wired.
type StdioPorts struct {
	Stdin  *uint32
	Stdout *uint32
	Stderr *uint32
}

// CreateProcess registers the container init process under id. Ossein runs
// exactly one process per throwaway VM, and the guest's convention for the
// init process is procID == containerID — so the wrappers take a single id
// and set both wire fields. specJSON is the full OCI runtime spec.
func (a *Agent) CreateProcess(ctx context.Context, processID string, specJSON []byte, stdio StdioPorts) error {
	_, err := a.c.CreateProcess(ctx, connect.NewRequest(&pb.CreateProcessRequest{
		Id:            processID,
		ContainerID:   &processID,
		Configuration: specJSON,
		Stdin:         stdio.Stdin,
		Stdout:        stdio.Stdout,
		Stderr:        stdio.Stderr,
	}))

	return wrap(err, "create process %s", processID)
}

// StartProcess starts a created process and returns its guest pid.
func (a *Agent) StartProcess(ctx context.Context, processID string) (int32, error) {
	resp, err := a.c.StartProcess(
		ctx,
		connect.NewRequest(&pb.StartProcessRequest{Id: processID, ContainerID: &processID}),
	)
	if err != nil {
		return 0, wrap(err, "start process %s", processID)
	}

	return resp.Msg.GetPid(), nil
}

// WaitProcess blocks until the process exits and returns its exit code.
func (a *Agent) WaitProcess(ctx context.Context, processID string) (int32, error) {
	resp, err := a.c.WaitProcess(
		ctx,
		connect.NewRequest(&pb.WaitProcessRequest{Id: processID, ContainerID: &processID}),
	)
	if err != nil {
		return -1, wrap(err, "wait process %s", processID)
	}

	return resp.Msg.GetExitCode(), nil
}

// KillProcess signals the process. The response's `result` field is vestigial
// Apple-proto surface — the guest reports failure via the RPC error code and
// always leaves it zero — so only the error is meaningful.
func (a *Agent) KillProcess(ctx context.Context, processID string, signal int32) error {
	_, err := a.c.KillProcess(
		ctx,
		connect.NewRequest(&pb.KillProcessRequest{Id: processID, ContainerID: &processID, Signal: signal}),
	)

	return wrap(err, "kill process %s", processID)
}

// ResizeProcess resizes the process's TTY.
func (a *Agent) ResizeProcess(ctx context.Context, processID string, rows, cols uint32) error {
	_, err := a.c.ResizeProcess(
		ctx,
		connect.NewRequest(
			&pb.ResizeProcessRequest{Id: processID, ContainerID: &processID, Rows: rows, Columns: cols},
		),
	)

	return wrap(err, "resize process %s", processID)
}

// CloseProcessStdin closes the guest-side stdin of the process.
func (a *Agent) CloseProcessStdin(ctx context.Context, processID string) error {
	_, err := a.c.CloseProcessStdin(
		ctx,
		connect.NewRequest(&pb.CloseProcessStdinRequest{Id: processID, ContainerID: &processID}),
	)

	return wrap(err, "close stdin %s", processID)
}

// --- networking (netlink inside the guest) ---

// LinkUp brings a guest network interface up.
func (a *Agent) LinkUp(ctx context.Context, iface string) error {
	_, err := a.c.IpLinkSet(ctx, connect.NewRequest(&pb.IpLinkSetRequest{Interface: iface, Up: true}))

	return wrap(err, "link up %s", iface)
}

// AddrAdd assigns a CIDR (e.g. "192.168.127.2/24") to iface.
func (a *Agent) AddrAdd(ctx context.Context, iface, cidr string) error {
	_, err := a.c.IpAddrAdd(ctx, connect.NewRequest(&pb.IpAddrAddRequest{Interface: iface, Ipv4Address: cidr}))

	return wrap(err, "addr add %s %s", iface, cidr)
}

// RouteAddDefault installs the guest's default route via gateway.
func (a *Agent) RouteAddDefault(ctx context.Context, iface, gateway string) error {
	_, err := a.c.IpRouteAddDefault(
		ctx,
		connect.NewRequest(&pb.IpRouteAddDefaultRequest{Interface: iface, Ipv4Gateway: gateway}),
	)

	return wrap(err, "default route via %s", gateway)
}

// ConfigureDNS writes a resolv.conf at location (an absolute guest path).
func (a *Agent) ConfigureDNS(ctx context.Context, location string, nameservers []string) error {
	_, err := a.c.ConfigureDns(
		ctx,
		connect.NewRequest(&pb.ConfigureDnsRequest{Location: location, Nameservers: nameservers}),
	)

	return wrap(err, "configure dns at %s", location)
}

// HostsEntry is one /etc/hosts line.
type HostsEntry struct {
	IP        string
	Hostnames []string
}

// ConfigureHosts writes an /etc/hosts at location (an absolute guest path).
func (a *Agent) ConfigureHosts(ctx context.Context, location string, entries []HostsEntry) error {
	pbEntries := make([]*pb.ConfigureHostsRequest_HostsEntry, 0, len(entries))
	for _, e := range entries {
		pbEntries = append(pbEntries, &pb.ConfigureHostsRequest_HostsEntry{
			IpAddress: e.IP, Hostnames: e.Hostnames,
		})
	}

	_, err := a.c.ConfigureHosts(
		ctx,
		connect.NewRequest(&pb.ConfigureHostsRequest{Location: location, Entries: pbEntries}),
	)

	return wrap(err, "configure hosts at %s", location)
}

// --- misc ---

// Getenv reads one environment variable from the guest agent's process.
func (a *Agent) Getenv(ctx context.Context, key string) (string, error) {
	resp, err := a.c.Getenv(ctx, connect.NewRequest(&pb.GetenvRequest{Key: key}))
	if err != nil {
		return "", wrap(err, "getenv %s", key)
	}

	return resp.Msg.GetValue(), nil
}

// SetupEmulator registers a binfmt_misc handler — the Rosetta hook. Values
// mirror the documented rosetta binfmt registration line.
func (a *Agent) SetupEmulator(ctx context.Context, name, binaryPath, offset, magic, mask, flags string) error {
	_, err := a.c.SetupEmulator(ctx, connect.NewRequest(&pb.SetupEmulatorRequest{
		BinaryPath: binaryPath, Name: name, Type: "M",
		Offset: offset, Magic: magic, Mask: mask, Flags: flags,
	}))

	return wrap(err, "setup emulator %s", name)
}

// ProxyVsockOutOf asks vminitd to listen on vsockPort inside the guest and
// forward each accepted connection to the unix socket at guestPath — the
// host end then reaches that guest socket via vm.Connect(vsockPort).
//
// Semantics per vminitd's VsockProxy: OUT_OF = vminitd listens on VSOCK and
// dials the guest unix path (pull traffic out of the guest); INTO = vminitd
// creates/listens on the guest unix path and dials the HOST over vsock
// (push host services into the guest). guestPath is resolved in the guest
// ROOT namespace — container sockets must be rootfs-prefixed.
func (a *Agent) ProxyVsockOutOf(ctx context.Context, id string, vsockPort uint32, guestPath string) error {
	_, err := a.c.ProxyVsock(ctx, connect.NewRequest(&pb.ProxyVsockRequest{
		Id: id, VsockPort: vsockPort, GuestPath: guestPath, Action: pb.ProxyVsockRequest_OUT_OF,
	}))

	return wrap(err, "proxy vsock %d -> %s", vsockPort, guestPath)
}

// StopVsockProxy tears down a proxy previously registered under id, closing
// the guest-side listener.
func (a *Agent) StopVsockProxy(ctx context.Context, id string) error {
	_, err := a.c.StopVsockProxy(ctx, connect.NewRequest(&pb.StopVsockProxyRequest{Id: id}))

	return wrap(err, "stop vsock proxy %s", id)
}

// Sync flushes guest page cache to backing devices (sync(2)).
func (a *Agent) Sync(ctx context.Context) error {
	_, err := a.c.Sync(ctx, connect.NewRequest(&pb.SyncRequest{}))

	return wrap(err, "sync")
}

// wrap folds an RPC failure into ErrAgent while preserving both the operation
// context and the underlying *connect.Error for errors.Is/As — so a caller can
// still recover the code with connect.CodeOf.
func wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%w: %s: %w", ErrAgent, fmt.Sprintf(format, args...), err)
}
