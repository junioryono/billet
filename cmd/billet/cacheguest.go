package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// cmdCacheGitCredential is the git credential helper the guest's gitconfig
// names for the node, which a github.com fetch reaches through url.insteadOf.
// The rewrite drops the header actions/checkout scopes to github.com, so this
// hands it to the node as the password, beside the session bearer.
func cmdCacheGitCredential(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: billet cache git-credential get (run by git)")
	}
	if args[0] != "get" {
		return nil
	}
	// RUN IN THE REPOSITORY GIT IS FETCHING INTO (measured: git runs a helper
	// with the worktree as its directory), where checkout wrote its header.
	output, _ := exec.CommandContext(ctx, "git", "config", "--get-all",
		"http.https://github.com/.extraheader").Output()
	answer, err := gitCredential(os.Stdin, os.Getenv(envCacheEndpoint), os.Getenv(envCacheToken),
		strings.Split(strings.TrimSpace(string(output)), "\n"))
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, answer)

	return err
}

// gitCredential answers git's request for the node's host: the session bearer
// as the user name, and the last Authorization header checkout configured for
// github.com as the password, or "-" when there is none. A request for any
// other host is answered with nothing.
func gitCredential(in io.Reader, endpoint, token string, headers []string) (string, error) {
	node, err := url.Parse(endpoint)
	if err != nil || node.Host == "" || token == "" {
		return "", nil
	}
	asked := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(in, 64<<10))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			asked[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read the credential request: %w", err)
	}
	if asked["protocol"] != node.Scheme || asked["host"] != node.Host {
		return "", nil
	}
	password := "-"
	for _, header := range headers {
		name, value, ok := strings.Cut(header, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "authorization") &&
			strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\r\n") {
			password = strings.TrimSpace(value)
		}
	}

	return "username=" + token + "\npassword=" + password + "\n", nil
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
