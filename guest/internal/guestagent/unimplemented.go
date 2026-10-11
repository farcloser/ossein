//go:build linux

package guestagent

import (
	"context"

	"connectrpc.com/connect/v2"

	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

// Agent serves the vendored SandboxContext contract itself: the generated
// handler takes plain protobuf messages, so nothing sits between them.
var _ sandboxconnect.SandboxContextHandler = (*Agent)(nil)

// The RPCs below are part of the vendored contract but have no host-side caller
// and no guest implementation. They are spelled out rather than inherited from
// UnimplementedSandboxContextHandler on purpose: the interface demands every
// method, so a NEW rpc in the proto becomes a compile error until someone
// decides what it should do, instead of silently answering Unimplemented at
// runtime.
//
// The names come from the proto (IpRouteAddLink, ...): renaming them to Go's
// initialism style would fail to satisfy the interface.
//
//revive:disable:var-naming

// FilesystemOperation has no caller and no implementation; see above.
func (*Agent) FilesystemOperation(
	_ context.Context, _ *pb.FilesystemOperationRequest,
) (*pb.FilesystemOperationResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "FilesystemOperation is not implemented by this runtime")
}

// DeleteProcess has no caller and no implementation; see above.
func (*Agent) DeleteProcess(_ context.Context, _ *pb.DeleteProcessRequest) (*pb.DeleteProcessResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "DeleteProcess is not implemented by this runtime")
}

// ContainerStatistics has no caller and no implementation; see above.
func (*Agent) ContainerStatistics(
	_ context.Context, _ *pb.ContainerStatisticsRequest,
) (*pb.ContainerStatisticsResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "ContainerStatistics is not implemented by this runtime")
}

// IpRouteAddLink has no caller and no implementation; see above.
func (*Agent) IpRouteAddLink(_ context.Context, _ *pb.IpRouteAddLinkRequest) (*pb.IpRouteAddLinkResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "IpRouteAddLink is not implemented by this runtime")
}

// Setenv has no caller and no implementation; see above.
func (*Agent) Setenv(_ context.Context, _ *pb.SetenvRequest) (*pb.SetenvResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "Setenv is not implemented by this runtime")
}

// Sysctl has no caller and no implementation; see above.
func (*Agent) Sysctl(_ context.Context, _ *pb.SysctlRequest) (*pb.SysctlResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "Sysctl is not implemented by this runtime")
}

// Stat has no caller and no implementation; see above.
func (*Agent) Stat(_ context.Context, _ *pb.StatRequest) (*pb.StatResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "Stat is not implemented by this runtime")
}

// Kill has no caller and no implementation; see above.
func (*Agent) Kill(_ context.Context, _ *pb.KillRequest) (*pb.KillResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, "Kill is not implemented by this runtime")
}
