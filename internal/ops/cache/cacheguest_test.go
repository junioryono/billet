package cache

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// GIT IS ANSWERED ONLY FOR THE NODE, with the session bearer and checkout's own
// header, the last one when several are configured, or "-" when there is none.
func TestTheGitCredentialHelperHandsTheNodeCheckoutsHeader(t *testing.T) {
	t.Parallel()

	const endpoint, token = "http://172.31.0.1:7718", "session"
	node := "protocol=http\nhost=172.31.0.1:7718\nwwwauth[]=Basic realm=\"billet\"\n\n"
	for name, tc := range map[string]struct {
		request string
		headers []string
		want    string
	}{
		"checkout's header": {node, []string{"AUTHORIZATION: basic eDpzZWNyZXQ="},
			"username=session\npassword=basic eDpzZWNyZXQ=\n"},
		"the last of several": {node, []string{"AUTHORIZATION: basic b2xk", "x-other: y", "authorization: basic bmV3"},
			"username=session\npassword=basic bmV3\n"},
		"no header":    {node, []string{""}, "username=session\npassword=-\n"},
		"another host": {"protocol=https\nhost=github.com\n\n", []string{"AUTHORIZATION: basic eDpzZWNyZXQ="}, ""},
		"another port": {"protocol=http\nhost=172.31.0.1:7719\n\n", []string{"AUTHORIZATION: basic eDpzZWNyZXQ="}, ""},
	} {
		got, err := gitCredential(strings.NewReader(tc.request), endpoint, token, tc.headers, nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: answered %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := gitCredential(strings.NewReader(node), endpoint, "", nil, nil); err != nil || got != "" {
		t.Errorf("with no session the helper answered %q, %v", got, err)
	}
}

// THE USER'S OWN CREDENTIAL COMES AFTER CHECKOUT'S, never before and never for
// another host: it is asked for only when checkout configured no header, so a
// fetch in the workspace keeps the token the workflow gave checkout.
func TestTheGitCredentialHelperFallsBackToTheUsersOwnCredential(t *testing.T) {
	t.Parallel()

	const endpoint, token = "http://172.31.0.1:7718", "session"
	node := "protocol=http\nhost=172.31.0.1:7718\npath=v1/git/github.com/acme/api.git\n\n"
	for name, tc := range map[string]struct {
		request, own string
		headers      []string
		want         string
		asked        bool
	}{
		"checkout's header first": {node, "basic b3du", []string{"AUTHORIZATION: basic Y2hlY2tvdXQ="}, "username=session\npassword=basic Y2hlY2tvdXQ=\n", false},
		"the user's own after":    {node, "basic b3du", nil, "username=session\npassword=basic b3du\n", true},
		"neither":                 {node, "", nil, "username=session\npassword=-\n", true},
		"never for another host":  {"protocol=https\nhost=github.com\n\n", "basic b3du", nil, "", false},
	} {
		asked := false
		got, err := gitCredential(strings.NewReader(tc.request), endpoint, token, tc.headers, func(path string) string {
			asked = true
			if path != "acme/api.git" {
				t.Errorf("%s: the user's own credential was asked for %q, want acme/api.git", name, path)
			}

			return tc.own
		})
		if err != nil || got != tc.want || asked != tc.asked {
			t.Errorf("%s: answered %q, %v, asked for the user's own %v; want %q, asked %v",
				name, got, err, asked, tc.want, tc.asked)
		}
	}
}

// THE USER'S OWN CREDENTIAL IS WHAT GIT'S OTHER HELPERS GIVE FOR github.com,
// as a basic Authorization header; one that fails, hangs or answers half is
// none, and the fetch goes on without it.
func TestTheUsersOwnCredentialIsAskedOfGitWithoutAPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := func(name, body string) string {
		t.Helper()

		path := filepath.Join(dir, name)
		if err := forkSafeWriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}

		return path
	}
	answers := fake("answers", `[ "$1 $2" = "credential fill" ] || exit 2
[ "$GIT_TERMINAL_PROMPT" = 0 ] || exit 3
input=$(cat)
case "$input" in *host=github.com*path=acme/api.git*) ;; *) exit 4 ;; esac
printf 'protocol=https\nhost=github.com\nusername=octocat\npassword=ghp_secret\n'
`)
	want := "basic " + base64.StdEncoding.EncodeToString([]byte("octocat:ghp_secret"))
	if got := githubCredential(t.Context(), answers, "acme/api.git"); got != want {
		t.Errorf("a helper's answer became %q, want %q", got, want)
	}
	for name, git := range map[string]string{
		"git has nothing":   fake("nothing", "cat >/dev/null\necho 'fatal: could not read Username' >&2\nexit 128\n"),
		"a half answer":     fake("half", "cat >/dev/null\nprintf 'username=octocat\\n'\n"),
		"a fill that hangs": fake("hangs", "cat >/dev/null\nexec sleep 60\n"),
		"no git at all":     filepath.Join(dir, "absent"),
	} {
		started := time.Now()
		if got := githubCredential(t.Context(), git, "acme/api.git"); got != "" {
			t.Errorf("%s: became %q, want none", name, got)
		}
		if took := time.Since(started); took > githubCredentialWait+3*time.Second {
			t.Errorf("%s: held the fetch %s", name, took)
		}
	}
}

// A HELPER THAT HANGS ENDS WITH THE FILL: git starts it as a child of its own,
// and one left running when the fill gives up would hold the environment it
// inherited for as long as it liked.
func TestAHangingHelperEndsWithTheFill(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	git := filepath.Join(dir, "git")
	if err := forkSafeWriteFile(git, []byte("#!/bin/sh\ncat >/dev/null\nsleep 60 &\necho $! >"+
		shellQuote(pidFile)+"\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := githubCredential(t.Context(), git, "acme/api.git"); got != "" {
		t.Fatalf("a hanging helper became %q", got)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the helper never started, so this proves nothing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // best effort: the test has failed
			t.Fatal("the helper git started was still running after the fill gave up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shellQuote is s as one sh word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// THE BEARER GOES ONLY TO THE NODE. Bazel asks the helper about every remote a
// build touches, and a workflow chooses those.
func TestTheCredentialHelperAnswersOnlyTheNode(t *testing.T) {
	t.Parallel()

	const endpoint, token = "http://172.31.0.1:7718", "secret"
	for uri, want := range map[string]bool{
		"http://172.31.0.1:7718/v1/cas/bazel/cas/00":  true,
		"grpc://172.31.0.1:7718":                      true,
		"https://cache.example.com/cas/00":            false,
		"http://172.31.0.1:7719/v1/cas/bazel/cas/00":  false,
		"http://172.31.0.1.example.com:7718/whatever": false,
		"not a uri at all":                            false,
	} {
		answer, err := cacheCredential(strings.NewReader(`{"uri":"`+uri+`"}`), endpoint, token)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		got := len(answer.Headers["Authorization"]) == 1 &&
			answer.Headers["Authorization"][0] == "Bearer "+token
		if got != want {
			t.Errorf("%s: bearer given = %v, want %v (%v)", uri, got, want, answer.Headers)
		}
	}

	answer, err := cacheCredential(strings.NewReader(`{"uri":"http://172.31.0.1:7718/x"}`), endpoint, "")
	if err != nil || len(answer.Headers) != 0 {
		t.Errorf("with no token the helper answered %v, %v", answer.Headers, err)
	}
}
