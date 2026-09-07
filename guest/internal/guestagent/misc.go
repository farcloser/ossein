//go:build linux

package guestagent

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	pb "github.com/farcloser/ossein/internal/sandbox"
)

// binfmtRegister is the binfmt_misc control file for registering interpreters.
const binfmtRegister = "/proc/sys/fs/binfmt_misc/register"

// Umount unmounts a path with the given flags.
func (*Agent) Umount(_ context.Context, req *pb.UmountRequest) (*pb.UmountResponse, error) {
	if err := unix.Unmount(req.GetPath(), int(req.GetFlags())); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "umount %s: %v", req.GetPath(), err)
	}

	return &pb.UmountResponse{}, nil
}

// Sync flushes filesystem buffers (used before VM stop so cache-disk writes
// reach the host image).
func (*Agent) Sync(_ context.Context, _ *pb.SyncRequest) (*pb.SyncResponse, error) {
	unix.Sync()

	return &pb.SyncResponse{}, nil
}

// SetTime sets the guest wall clock.
func (*Agent) SetTime(_ context.Context, req *pb.SetTimeRequest) (*pb.SetTimeResponse, error) {
	tv := unix.Timeval{Sec: req.GetSec(), Usec: int64(req.GetUsec())}
	if err := unix.Settimeofday(&tv); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "settimeofday: %v", err)
	}

	return &pb.SetTimeResponse{}, nil
}

// SetupEmulator registers a binfmt_misc interpreter (e.g. Rosetta for amd64).
// The registration string format is the kernel's:
// :name:type:offset:magic:mask:interpreter:flags.
func (*Agent) SetupEmulator(_ context.Context, req *pb.SetupEmulatorRequest) (*pb.SetupEmulatorResponse, error) {
	registration := fmt.Sprintf(":%s:%s:%s:%s:%s:%s:%s",
		req.GetName(), req.GetType(), req.GetOffset(),
		req.GetMagic(), req.GetMask(), req.GetBinaryPath(), req.GetFlags())

	// The register pseudo-file always exists on the mounted binfmt_misc; the
	// mode argument is only used on create and thus never takes effect.
	if err := os.WriteFile(binfmtRegister, []byte(registration), stdFileMode); err != nil {
		return nil, rpcErrorf(connect.CodeInternal, "register binfmt %q: %v", req.GetName(), err)
	}

	return &pb.SetupEmulatorResponse{}, nil
}
