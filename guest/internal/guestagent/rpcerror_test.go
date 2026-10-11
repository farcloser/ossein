//go:build linux

package guestagent_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	"github.com/farcloser/ossein/guest/internal/guestagent"
	pb "github.com/farcloser/ossein/internal/sandbox"
	"github.com/farcloser/ossein/internal/sandbox/sandboxconnect"
)

var errPlain = errors.New("open /x: no such file or directory")

// failingService answers Getenv with whatever error the test scripted.
type failingService struct {
	sandboxconnect.UnimplementedSandboxContextHandler

	err error
}

func (f failingService) Getenv(context.Context, *pb.GetenvRequest) (*pb.GetenvResponse, error) {
	return nil, f.err
}

func callGetenv(t *testing.T, err error) error {
	t.Helper()

	server := connect.NewServer(guestagent.WireErrorText)
	sandboxconnect.RegisterSandboxContextHandler(server, failingService{err: err})

	client := sandboxconnect.NewSandboxContextClient(connect.NewClient(connectinprocess.New(server)))

	_, got := client.Getenv(t.Context(), &pb.GetenvRequest{Key: "k"})
	if got == nil {
		t.Fatal("the handler's error did not reach the client")
	}

	return got
}

// A plain error from a handler crosses with its text: connect alone would send
// a bare CodeUnknown, and ossein's CLI shows only the message.
func TestWireErrorTextKeepsAPlainErrorsMessage(t *testing.T) {
	t.Parallel()

	got := callGetenv(t, errPlain)

	if code := connect.CodeOf(got); code != connect.CodeUnknown {
		t.Fatalf("code: got %v, want %v", code, connect.CodeUnknown)
	}

	var coded *connect.Error
	if !errors.As(got, &coded) || coded.Message() != errPlain.Error() {
		t.Fatalf("message: got %q, want %q", got, errPlain)
	}
}

// A coded error is passed through untouched, code and message.
func TestWireErrorTextPassesACodedErrorThrough(t *testing.T) {
	t.Parallel()

	got := callGetenv(t, connect.NewError(connect.CodeNotFound, "nope"))

	var coded *connect.Error
	if !errors.As(got, &coded) || coded.Code() != connect.CodeNotFound || coded.Message() != "nope" {
		t.Fatalf("got %v, want CodeNotFound \"nope\"", got)
	}
}

// A context error, even wrapped, gets its own code rather than Unknown.
func TestWireErrorTextCodesAContextError(t *testing.T) {
	t.Parallel()

	for want, err := range map[connect.Code]error{
		connect.CodeCanceled:         context.Canceled,
		connect.CodeDeadlineExceeded: context.DeadlineExceeded,
	} {
		if got := connect.CodeOf(callGetenv(t, errors.Join(errors.New("awaiting stage2"), err))); got != want {
			t.Errorf("%v: got %v, want %v", err, got, want)
		}
	}
}
