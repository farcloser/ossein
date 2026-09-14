//go:build darwin && arm64

package main

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/buildkit/session/auth"
	"google.golang.org/grpc"
)

// dockerHubHost is the host buildkitd asks credentials for when a Dockerfile
// pulls from Docker Hub; docker's credential store files those under the
// index.docker.io key, which go-containerregistry's keychain knows as the
// default registry.
const dockerHubHost = "registry-1.docker.io"

// keychainAuth is the registry-credentials half of the build session: when
// buildkitd resolves a FROM line it asks the client for the host's
// credentials, and this answers from docker's own credential store — the same
// keychain `ossein pull` consults, so a `docker login` covers both. It
// implements Credentials only; the token-authority calls buildkit added for
// cross-session token sharing answer Unimplemented, which buildkitd treats as
// "fetch the token yourself with these credentials". (buildkit ships an
// equivalent provider, authprovider, that would drag docker/cli and an
// MPL-licensed HTTP helper into the binary for the same four lines.)
type keychainAuth struct {
	auth.UnimplementedAuthServer
}

// Register makes keychainAuth a session.Attachable.
func (a keychainAuth) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, a)
}

// Credentials answers buildkitd's credential request for host. An anonymous
// host gets an empty response, which buildkitd reads as "no credentials".
func (keychainAuth) Credentials(_ context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	host := req.GetHost()
	if host == dockerHubHost {
		host = name.DefaultRegistry
	}

	registry, err := name.NewRegistry(host)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", req.GetHost(), err)
	}

	authenticator, err := authn.DefaultKeychain.Resolve(registry)
	if err != nil {
		return nil, fmt.Errorf("credentials for %s: %w", registry, err)
	}

	cfg, err := authenticator.Authorization()
	if err != nil {
		return nil, fmt.Errorf("credentials for %s: %w", registry, err)
	}

	// docker's convention, which buildkitd expects: an identity (refresh)
	// token travels as the secret with no username.
	if cfg.IdentityToken != "" {
		return &auth.CredentialsResponse{Secret: cfg.IdentityToken}, nil
	}

	return &auth.CredentialsResponse{Username: cfg.Username, Secret: cfg.Password}, nil
}
