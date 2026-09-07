//go:build linux

package guestagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	pb "github.com/farcloser/ossein/internal/sandbox"
)

// linkErrFmt is the status message format for netlink link lookup failures.
const linkErrFmt = "link %s: %v"

// IpLinkSet brings an interface up/down and optionally sets its MTU (netlink).
// The name is fixed by the generated SandboxContext interface.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface
//revive:disable-next-line:var-naming
func (*Agent) IpLinkSet(_ context.Context, req *pb.IpLinkSetRequest) (*pb.IpLinkSetResponse, error) {
	link, err := netlink.LinkByName(req.GetInterface())
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, linkErrFmt, req.GetInterface(), err)
	}

	if req.Mtu != nil {
		if err := netlink.LinkSetMTU(link, int(req.GetMtu())); err != nil {
			return nil, rpcErrorf(connect.CodeInternal, "set mtu on %s: %v", req.GetInterface(), err)
		}
	}

	if req.GetUp() {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, rpcErrorf(connect.CodeInternal, "link up %s: %v", req.GetInterface(), err)
		}
	} else if err := netlink.LinkSetDown(link); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "link down %s: %v", req.GetInterface(), err)
	}

	return &pb.IpLinkSetResponse{}, nil
}

// IpAddrAdd assigns an IPv4 (and optionally IPv6) CIDR address to an interface.
// The name is fixed by the generated SandboxContext interface.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface
//revive:disable-next-line:var-naming
func (*Agent) IpAddrAdd(_ context.Context, req *pb.IpAddrAddRequest) (*pb.IpAddrAddResponse, error) {
	link, err := netlink.LinkByName(req.GetInterface())
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, linkErrFmt, req.GetInterface(), err)
	}

	for _, cidr := range []string{req.GetIpv4Address(), req.GetIpv6Address()} {
		if cidr == "" {
			continue
		}

		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return nil, rpcErrorf(connect.CodeInvalidArgument, "parse addr %q: %v", cidr, err)
		}

		if err := netlink.AddrAdd(link, addr); err != nil {
			return nil, rpcErrorf(connect.CodeInternal, "add addr %s to %s: %v", cidr, req.GetInterface(), err)
		}
	}

	return &pb.IpAddrAddResponse{}, nil
}

// IpRouteAddDefault installs a default route via the given gateway on an interface.
// The name is fixed by the generated SandboxContext interface.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface
//revive:disable-next-line:var-naming
func (*Agent) IpRouteAddDefault(
	_ context.Context,
	req *pb.IpRouteAddDefaultRequest,
) (*pb.IpRouteAddDefaultResponse, error) {
	link, err := netlink.LinkByName(req.GetInterface())
	if err != nil {
		return nil, rpcErrorf(connect.CodeInternal, linkErrFmt, req.GetInterface(), err)
	}

	for _, gateway := range []string{req.GetIpv4Gateway(), req.GetIpv6Gateway()} {
		if gateway == "" {
			continue
		}

		ip := net.ParseIP(gateway)
		if ip == nil {
			return nil, rpcErrorf(connect.CodeInvalidArgument, "parse gateway %q", gateway)
		}
		// Dst nil => default route.
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: ip}); err != nil {
			return nil, rpcErrorf(connect.CodeInternal, "add default route via %s: %v", gateway, err)
		}
	}

	return &pb.IpRouteAddDefaultResponse{}, nil
}

// ConfigureDns writes a resolv.conf into the container rootfs. Location is the
// rootfs directory; the file lands at <location>/etc/resolv.conf.
// The name is fixed by the generated SandboxContext interface.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface
//revive:disable-next-line:var-naming
func (*Agent) ConfigureDns(_ context.Context, req *pb.ConfigureDnsRequest) (*pb.ConfigureDnsResponse, error) {
	var builder strings.Builder
	for _, ns := range req.GetNameservers() {
		fmt.Fprintf(&builder, "nameserver %s\n", ns)
	}

	if d := req.GetDomain(); d != "" {
		fmt.Fprintf(&builder, "domain %s\n", d)
	}

	if s := req.GetSearchDomains(); len(s) > 0 {
		fmt.Fprintf(&builder, "search %s\n", strings.Join(s, " "))
	}

	if o := req.GetOptions(); len(o) > 0 {
		fmt.Fprintf(&builder, "options %s\n", strings.Join(o, " "))
	}

	if err := writeRootfsFile(req.GetLocation(), "etc/resolv.conf", builder.String()); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "write resolv.conf: %v", err)
	}

	return &pb.ConfigureDnsResponse{}, nil
}

// ConfigureHosts writes an /etc/hosts into the container rootfs. Location is the
// rootfs directory; the file lands at <location>/etc/hosts.
func (*Agent) ConfigureHosts(_ context.Context, req *pb.ConfigureHostsRequest) (*pb.ConfigureHostsResponse, error) {
	var builder strings.Builder
	for _, e := range req.GetEntries() {
		fmt.Fprintf(&builder, "%s\t%s", e.GetIpAddress(), strings.Join(e.GetHostnames(), " "))

		if c := e.GetComment(); c != "" {
			fmt.Fprintf(&builder, "\t# %s", c)
		}

		_ = builder.WriteByte('\n') // strings.Builder never fails
	}

	if err := writeRootfsFile(req.GetLocation(), "etc/hosts", builder.String()); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "write hosts: %v", err)
	}

	return &pb.ConfigureHostsResponse{}, nil
}

// writeRootfsFile writes content to <location>/<rel>, creating parent dirs.
//
// The rootfs is image-controlled, so this is the same containment problem the
// extractor has and it gets the same answer: rootPath resolves the parents
// through the kernel (RESOLVE_IN_ROOT), and the final component is replaced
// rather than followed. Without that, an image shipping etc/resolv.conf as a
// symlink to an absolute guest path made the agent write there as root — the
// container was then left with a dangling link and no DNS.
func writeRootfsFile(location, rel, content string) error {
	confined, err := openRoot(location)
	if err != nil {
		return err
	}
	defer func() { _ = confined.Close() }()

	target, err := confined.Resolve(rel)
	if err != nil {
		return err
	}

	if target == "" {
		return fmt.Errorf("%w: %q names the rootfs itself", errPathEscapes, rel)
	}

	// Replace, never follow: a symlink sitting at the destination must not
	// redirect this write, and O_NOFOLLOW closes the recreate race.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove before write %s: %w", target, err)
	}

	// 0o644 is the standard perm for /etc/resolv.conf and /etc/hosts: the
	// container's non-root processes must be able to read them.
	// #nosec G304 -- target's parents are kernel-confined to the rootfs
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, stdFileMode)
	if err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}

	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()

		return fmt.Errorf("write %s: %w", target, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}

	return nil
}
