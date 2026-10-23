package cache

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/cli"

	"github.com/junioryono/billet/internal/gocacheprog"
)

// The guest's view of the node cache. The runner's environment carries both;
// the bearer is never written to a file billet installs. The image's sudoers
// names the two, without their values, so they cross sudo
// (build-guest-image.sh).
const (
	envCacheEndpoint = "BILLET_CACHE_ENDPOINT"
	envCacheToken    = "BILLET_CACHE_TOKEN"
	envGoCacheDir    = "BILLET_GOCACHE_DIR"
)

// cmdCacheGoCacheProg is the GOCACHEPROG a guest image names: it speaks the go
// command's protocol on stdin and stdout until the go command closes it.
func cmdCacheGoCacheProg(ctx context.Context, env cli.Env, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: billet cache gocacheprog (run by the go command as GOCACHEPROG)")
	}
	dir := env.Getenv(envGoCacheDir)
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("no %s and no user cache directory: %w", envGoCacheDir, err)
		}
		dir = filepath.Join(base, "billet", "go-build")
	}

	return gocacheprog.Run(ctx, gocacheprog.Config{
		Dir:      dir,
		Endpoint: env.Getenv(envCacheEndpoint),
		Token:    env.Getenv(envCacheToken),
		Log:      env.Stderr,
	}, env.Stdin, env.Stdout)
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
func cmdCacheCredentialHelper(_ context.Context, env cli.Env, args []string) error {
	if len(args) != 1 || args[0] != "get" {
		return errors.New("usage: billet cache credential-helper get (run by bazel)")
	}
	answer, err := cacheCredential(env.Stdin, env.Getenv(envCacheEndpoint), env.Getenv(envCacheToken))
	if err != nil {
		return err
	}

	return json.NewEncoder(env.Stdout).Encode(answer)
}

// cmdCacheGitCredential is the git credential helper the guest's gitconfig
// names for the node, which a github.com fetch reaches through url.insteadOf.
// The rewrite drops the header actions/checkout scopes to github.com, so this
// hands it to the node as the password, beside the session bearer; and the
// rewrite hides git's own github.com helpers, so without that header it hands
// the user's own github.com credential. Under sudo it reads the same
// environment: the image's sudoers keeps the two variables across it.
func cmdCacheGitCredential(ctx context.Context, env cli.Env, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: billet cache git-credential get (run by git)")
	}
	if args[0] != "get" {
		return nil
	}
	headers, err := checkoutHeaders(ctx)
	if err != nil {
		return err
	}
	answer, err := gitCredential(env.Stdin, env.Getenv(envCacheEndpoint), env.Getenv(envCacheToken), headers,
		func(path string) string { return githubCredential(ctx, "git", path) })
	if err != nil {
		return err
	}
	_, err = io.WriteString(env.Stdout, answer)

	return err
}

// gitProxyPath is where the node serves github.com's repositories: a path git
// hands this helper is the repository's github.com path behind it.
const gitProxyPath = "/v1/git/github.com/"

// githubCredentialWait bounds asking git for the user's own github.com
// credential: a helper that hangs must not hold the fetch.
const githubCredentialWait = 5 * time.Second

// githubCredential is the user's own github.com credential for the repository
// at path (owner/repo.git, or "" when git did not say) as an Authorization
// header, from whatever helpers git has for github.com (a credential store,
// `gh auth setup-git`), or "" when there is none or it cannot be had. git
// applies the user's own github.com policy to the path, so a helper configured
// for one repository is the one asked. The image's gitconfig names this helper
// only for the node, so git never asks it here, and nothing prompts: the job
// has no one to answer.
func githubCredential(ctx context.Context, git, path string) string {
	ctx, cancel := context.WithTimeout(ctx, githubCredentialWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "credential", "fill")
	// THE WHOLE GROUP ENDS WITH IT: a helper git started that hangs would
	// otherwise outlive the fill, holding the environment it inherited.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	request := "protocol=https\nhost=github.com\n"
	if path != "" && !strings.ContainsAny(path, "\r\n\x00") {
		request += "path=" + path + "\n"
	}
	cmd.Stdin = strings.NewReader(request + "\n")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=",
		"GCM_INTERACTIVE=never")
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	answer := map[string]string{}
	for line := range strings.Lines(string(output)) {
		if key, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
			answer[key] = value
		}
	}
	user, password := answer["username"], answer["password"]
	if user == "" || password == "" || strings.ContainsAny(user+password, "\r\n") {
		return ""
	}

	return "basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// checkoutHeaders is every header actions/checkout configured for github.com in
// the repository git is fetching into (measured: git runs a helper with the
// worktree as its directory). git config exits 1 for a key that is not set,
// which is none; any other failure is the helper's, not an absent header.
func checkoutHeaders(ctx context.Context) ([]string, error) {
	output, err := exec.CommandContext(ctx, "git", "config", "--get-all",
		"http.https://github.com/.extraheader").Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkout's github.com header: %w", err)
	}

	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

// hostOf is the scheme and host a URL names, or two empty strings for one that
// does not parse, which names no host at all.
func hostOf(raw string) (string, string) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}

	return parsed.Scheme, parsed.Host
}

// gitCredential answers git's request for the node's host: the session bearer
// as the user name, and as the password the last Authorization header checkout
// configured for github.com, else the user's own github.com credential own
// finds, else "-". A request for any other host is answered with nothing.
//
// CHECKOUT'S HEADER FIRST, so a fetch in the workspace keeps using the token
// the workflow gave checkout; the user's own credential is for what that
// token does not reach, another private repository a job clones with its own.
// own is handed the github.com path of the repository git is fetching, which
// the image's gitconfig has git send.
func gitCredential(in io.Reader, endpoint, token string, headers []string, own func(path string) string) (string, error) {
	scheme, host := hostOf(endpoint)
	if host == "" || token == "" {
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
	if asked["protocol"] != scheme || asked["host"] != host {
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
	if password == "-" && own != nil {
		path, _ := strings.CutPrefix(asked["path"], strings.TrimPrefix(gitProxyPath, "/"))
		if path == asked["path"] {
			path = ""
		}
		if header := own(path); header != "" {
			password = header
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
	_, node := hostOf(endpoint)
	if _, asked := hostOf(request.URI); asked == "" || asked != node {
		return answer, nil
	}
	answer.Headers["Authorization"] = []string{"Bearer " + token}

	return answer, nil
}
