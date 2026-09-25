package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/gocacheprog"
)

// The guest's view of the node cache. The runner's environment carries both;
// neither is ever written to a file billet installs.
const (
	envCacheEndpoint = "BILLET_CACHE_ENDPOINT"
	envCacheToken    = "BILLET_CACHE_TOKEN"
	envGoCacheDir    = "BILLET_GOCACHE_DIR"
)

// cmdCacheGoCacheProg is the GOCACHEPROG a guest image names: it speaks the go
// command's protocol on stdin and stdout until the go command closes it.
func cmdCacheGoCacheProg(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: billet cache gocacheprog (run by the go command as GOCACHEPROG)")
	}
	dir := os.Getenv(envGoCacheDir)
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("no %s and no user cache directory: %w", envGoCacheDir, err)
		}
		dir = filepath.Join(base, "billet", "go-build")
	}

	return gocacheprog.Run(ctx, gocacheprog.Config{
		Dir:      dir,
		Endpoint: os.Getenv(envCacheEndpoint),
		Token:    os.Getenv(envCacheToken),
		Log:      os.Stderr,
	}, os.Stdin, os.Stdout)
}

// credentialRequest and credentialResponse are Bazel's credential helper
// protocol: `<helper> get` reads the URI on stdin and answers its headers.
type credentialRequest struct {
	URI string `json:"uri"`
}

type credentialResponse struct {
	Headers map[string][]string `json:"headers"`
}

// cmdCacheCredentialHelper is the credential helper the guest's bazelrc names
// for the node, so the bearer lives in the runner's environment and never in
// a file a job can read after the fact or a build can log.
func cmdCacheCredentialHelper(_ context.Context, args []string) error {
	if len(args) != 1 || args[0] != "get" {
		return errors.New("usage: billet cache credential-helper get (run by bazel)")
	}
	answer, err := cacheCredential(os.Stdin, os.Getenv(envCacheEndpoint), os.Getenv(envCacheToken))
	if err != nil {
		return err
	}

	return json.NewEncoder(os.Stdout).Encode(answer)
}

// cacheCredential answers the bearer only for a URI on the node's own endpoint:
// a helper that answered every host would hand the token to whichever remote a
// workflow's bazelrc names.
func cacheCredential(in io.Reader, endpoint, token string) (credentialResponse, error) {
	var request credentialRequest
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&request); err != nil {
		return credentialResponse{}, fmt.Errorf("read the credential request: %w", err)
	}
	answer := credentialResponse{Headers: map[string][]string{}}
	if endpoint == "" || token == "" {
		return answer, nil
	}
	node, err := url.Parse(endpoint)
	if err != nil {
		return answer, nil
	}
	asked, err := url.Parse(request.URI)
	if err != nil || asked.Host != node.Host || asked.Host == "" {
		return answer, nil
	}
	answer.Headers["Authorization"] = []string{"Bearer " + token}

	return answer, nil
}
