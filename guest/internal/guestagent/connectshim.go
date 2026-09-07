//go:build linux

package guestagent

import (
	"context"

	"connectrpc.com/connect"

	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

// ConnectShim adapts *Agent to the generated Connect handler interfaces. The
// Agent's own methods take plain protobuf types; Connect wants them inside
// Request/Response envelopes, and unwrapping at this one edge keeps every
// handler in the package free of transport types — the property they had under
// grpc, and the reason swapping transport touched this file instead of twenty.
//
// Unimplemented RPCs are spelled out below rather than inherited from an
// "UnimplementedServer" embed. That is deliberate: Connect's interface demands
// every method, so a NEW rpc in the proto becomes a compile error until someone
// decides what it should do, instead of silently answering Unimplemented at
// runtime the way the grpc embed did.
type ConnectShim struct{ agent *Agent }

// NewConnectShim wraps an agent for serving over Connect.
func NewConnectShim(agent *Agent) ConnectShim { return ConnectShim{agent: agent} }

var _ sandboxconnect.SandboxContextHandler = ConnectShim{}

// Every method below must match the generated interface EXACTLY, and those
// names come from the vendored proto (IpLinkSet, ConfigureDns, …). Renaming
// them to Go's initialism style would simply fail to satisfy the interface, so
// the rule is scoped off for the adapters rather than fought.
//
//revive:disable:var-naming

// SetTime adapts Agent.SetTime to the Connect handler signature.
func (s ConnectShim) SetTime(
	ctx context.Context, req *connect.Request[pb.SetTimeRequest],
) (*connect.Response[pb.SetTimeResponse], error) {
	resp, err := s.agent.SetTime(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// SetupEmulator adapts Agent.SetupEmulator to the Connect handler signature.
func (s ConnectShim) SetupEmulator(
	ctx context.Context, req *connect.Request[pb.SetupEmulatorRequest],
) (*connect.Response[pb.SetupEmulatorResponse], error) {
	resp, err := s.agent.SetupEmulator(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// WriteFile adapts Agent.WriteFile to the Connect handler signature.
func (s ConnectShim) WriteFile(
	ctx context.Context, req *connect.Request[pb.WriteFileRequest],
) (*connect.Response[pb.WriteFileResponse], error) {
	resp, err := s.agent.WriteFile(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// FilesystemOperation is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) FilesystemOperation(
	_ context.Context, _ *connect.Request[pb.FilesystemOperationRequest],
) (*connect.Response[pb.FilesystemOperationResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "FilesystemOperation is not implemented by this runtime")
}

// CreateProcess adapts Agent.CreateProcess to the Connect handler signature.
func (s ConnectShim) CreateProcess(
	ctx context.Context, req *connect.Request[pb.CreateProcessRequest],
) (*connect.Response[pb.CreateProcessResponse], error) {
	resp, err := s.agent.CreateProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// DeleteProcess is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) DeleteProcess(
	_ context.Context, _ *connect.Request[pb.DeleteProcessRequest],
) (*connect.Response[pb.DeleteProcessResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "DeleteProcess is not implemented by this runtime")
}

// StartProcess adapts Agent.StartProcess to the Connect handler signature.
func (s ConnectShim) StartProcess(
	ctx context.Context, req *connect.Request[pb.StartProcessRequest],
) (*connect.Response[pb.StartProcessResponse], error) {
	resp, err := s.agent.StartProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// KillProcess adapts Agent.KillProcess to the Connect handler signature.
func (s ConnectShim) KillProcess(
	ctx context.Context, req *connect.Request[pb.KillProcessRequest],
) (*connect.Response[pb.KillProcessResponse], error) {
	resp, err := s.agent.KillProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// WaitProcess adapts Agent.WaitProcess to the Connect handler signature.
func (s ConnectShim) WaitProcess(
	ctx context.Context, req *connect.Request[pb.WaitProcessRequest],
) (*connect.Response[pb.WaitProcessResponse], error) {
	resp, err := s.agent.WaitProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// ResizeProcess adapts Agent.ResizeProcess to the Connect handler signature.
func (s ConnectShim) ResizeProcess(
	ctx context.Context, req *connect.Request[pb.ResizeProcessRequest],
) (*connect.Response[pb.ResizeProcessResponse], error) {
	resp, err := s.agent.ResizeProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// CloseProcessStdin adapts Agent.CloseProcessStdin to the Connect handler signature.
func (s ConnectShim) CloseProcessStdin(
	ctx context.Context, req *connect.Request[pb.CloseProcessStdinRequest],
) (*connect.Response[pb.CloseProcessStdinResponse], error) {
	resp, err := s.agent.CloseProcessStdin(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// ContainerStatistics is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) ContainerStatistics(
	_ context.Context, _ *connect.Request[pb.ContainerStatisticsRequest],
) (*connect.Response[pb.ContainerStatisticsResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "ContainerStatistics is not implemented by this runtime")
}

// ProxyVsock adapts Agent.ProxyVsock to the Connect handler signature.
func (s ConnectShim) ProxyVsock(
	ctx context.Context, req *connect.Request[pb.ProxyVsockRequest],
) (*connect.Response[pb.ProxyVsockResponse], error) {
	resp, err := s.agent.ProxyVsock(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// StopVsockProxy adapts Agent.StopVsockProxy to the Connect handler signature.
func (s ConnectShim) StopVsockProxy(
	ctx context.Context, req *connect.Request[pb.StopVsockProxyRequest],
) (*connect.Response[pb.StopVsockProxyResponse], error) {
	resp, err := s.agent.StopVsockProxy(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// IpLinkSet adapts Agent.IpLinkSet to the Connect handler signature.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface (see the block note above)
func (s ConnectShim) IpLinkSet(
	ctx context.Context, req *connect.Request[pb.IpLinkSetRequest],
) (*connect.Response[pb.IpLinkSetResponse], error) {
	resp, err := s.agent.IpLinkSet(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// IpAddrAdd adapts Agent.IpAddrAdd to the Connect handler signature.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface (see the block note above)
func (s ConnectShim) IpAddrAdd(
	ctx context.Context, req *connect.Request[pb.IpAddrAddRequest],
) (*connect.Response[pb.IpAddrAddResponse], error) {
	resp, err := s.agent.IpAddrAdd(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// IpRouteAddLink is part of the vendored contract but has no host-side caller and no
// guest implementation.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface (see the block note above)
func (ConnectShim) IpRouteAddLink(
	_ context.Context, _ *connect.Request[pb.IpRouteAddLinkRequest],
) (*connect.Response[pb.IpRouteAddLinkResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "IpRouteAddLink is not implemented by this runtime")
}

// IpRouteAddDefault adapts Agent.IpRouteAddDefault to the Connect handler signature.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface (see the block note above)
func (s ConnectShim) IpRouteAddDefault(
	ctx context.Context, req *connect.Request[pb.IpRouteAddDefaultRequest],
) (*connect.Response[pb.IpRouteAddDefaultResponse], error) {
	resp, err := s.agent.IpRouteAddDefault(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// ConfigureDns adapts Agent.ConfigureDns to the Connect handler signature.
//
//nolint:staticcheck // ST1003: name fixed by the generated interface (see the block note above)
func (s ConnectShim) ConfigureDns(
	ctx context.Context, req *connect.Request[pb.ConfigureDnsRequest],
) (*connect.Response[pb.ConfigureDnsResponse], error) {
	resp, err := s.agent.ConfigureDns(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// ConfigureHosts adapts Agent.ConfigureHosts to the Connect handler signature.
func (s ConnectShim) ConfigureHosts(
	ctx context.Context, req *connect.Request[pb.ConfigureHostsRequest],
) (*connect.Response[pb.ConfigureHostsResponse], error) {
	resp, err := s.agent.ConfigureHosts(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Mount adapts Agent.Mount to the Connect handler signature.
func (s ConnectShim) Mount(
	ctx context.Context, req *connect.Request[pb.MountRequest],
) (*connect.Response[pb.MountResponse], error) {
	resp, err := s.agent.Mount(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Umount adapts Agent.Umount to the Connect handler signature.
func (s ConnectShim) Umount(
	ctx context.Context, req *connect.Request[pb.UmountRequest],
) (*connect.Response[pb.UmountResponse], error) {
	resp, err := s.agent.Umount(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Setenv is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) Setenv(
	_ context.Context, _ *connect.Request[pb.SetenvRequest],
) (*connect.Response[pb.SetenvResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "Setenv is not implemented by this runtime")
}

// Getenv adapts Agent.Getenv to the Connect handler signature.
func (s ConnectShim) Getenv(
	ctx context.Context, req *connect.Request[pb.GetenvRequest],
) (*connect.Response[pb.GetenvResponse], error) {
	resp, err := s.agent.Getenv(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Mkdir adapts Agent.Mkdir to the Connect handler signature.
func (s ConnectShim) Mkdir(
	ctx context.Context, req *connect.Request[pb.MkdirRequest],
) (*connect.Response[pb.MkdirResponse], error) {
	resp, err := s.agent.Mkdir(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Sysctl is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) Sysctl(
	_ context.Context, _ *connect.Request[pb.SysctlRequest],
) (*connect.Response[pb.SysctlResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "Sysctl is not implemented by this runtime")
}

// Stat is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) Stat(
	_ context.Context, _ *connect.Request[pb.StatRequest],
) (*connect.Response[pb.StatResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "Stat is not implemented by this runtime")
}

// Sync adapts Agent.Sync to the Connect handler signature.
func (s ConnectShim) Sync(
	ctx context.Context, req *connect.Request[pb.SyncRequest],
) (*connect.Response[pb.SyncResponse], error) {
	resp, err := s.agent.Sync(ctx, req.Msg)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(resp), nil
}

// Kill is part of the vendored contract but has no host-side caller and no
// guest implementation.
func (ConnectShim) Kill(
	_ context.Context, _ *connect.Request[pb.KillRequest],
) (*connect.Response[pb.KillResponse], error) {
	return nil, rpcErrorf(connect.CodeUnimplemented, "Kill is not implemented by this runtime")
}

// Copy adapts Agent.Copy to the Connect server-stream handler signature.
func (s ConnectShim) Copy(
	ctx context.Context, req *connect.Request[pb.CopyRequest], stream *connect.ServerStream[pb.CopyResponse],
) error {
	return s.agent.Copy(ctx, req.Msg, stream)
}

//revive:enable:var-naming
