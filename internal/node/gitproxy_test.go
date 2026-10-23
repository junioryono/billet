package node

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// githubBasic is the header a job's checkout would carry for the private
// repository the fake upstream serves.
const githubBasic = "basic eC1hY2Nlc3MtdG9rZW46c2VjcmV0"

// gitGuard is how long a test waits for git work whose bound under test is a
// few seconds. It is a failure deadline, never a condition: what it waits for
// is held until the test ends, so bounded work returns long before it however
// loaded the machine, and unbounded work never returns at all.
const gitGuard = 2 * time.Minute

// gitWithin runs call and fails the test if it has not returned by gitGuard.
func gitWithin[T any](t *testing.T, what string, call func() T) T {
	t.Helper()

	done := make(chan T, 1)
	go func() { done <- call() }()

	select {
	case v := <-done:
		return v
	case <-time.After(gitGuard):
		t.Fatalf("%s had not returned after %s; nothing bounded it", what, gitGuard)

		var zero T

		return zero
	}
}

// gitUpstream is github.com: git's own smart-HTTP server over private
// repositories, refusing any request without githubBasic, counting fetches.
// Like github.com it serves any object a repository holds, whether or not a ref
// reaches it, and serves filters (measured, 2026-10-21, ADR-018).
type gitUpstream struct {
	*httptest.Server
	root    string
	repo    string
	fetches atomic.Int64
	// mirrorFetches counts the fetches the node's own git makes, which speak
	// protocol v2 where a guest's, answered by the node's v0 advertisement,
	// do not.
	mirrorFetches atomic.Int64
	// refuse makes it answer every request as it would a header it no longer
	// accepts.
	refuse atomic.Bool
	// hold, when set, holds the node's own fetches until it is closed.
	hold atomic.Pointer[chan struct{}]
	// onMirrorFetch, when set, runs as each of the node's own fetches arrives.
	onMirrorFetch atomic.Pointer[func()]
	// ids is the id each repository name is, as GitHub's REST API answers
	// `GET /repos/{owner}/{repo}` under /api; apiDown makes it answer 503.
	ids      sync.Map
	nextID   atomic.Int64
	apiDown  atomic.Bool
	apiCalls atomic.Int64
	// mirrorRequests is the body of every request the node's own git made,
	// ls-refs and fetch alike, in order.
	mirrorMu       sync.Mutex
	mirrorRequests []string
}

func (u *gitUpstream) recordMirrorRequest(body []byte) {
	u.mirrorMu.Lock()
	defer u.mirrorMu.Unlock()
	u.mirrorRequests = append(u.mirrorRequests, string(body))
}

// takeMirrorRequests is what the node's own git asked since the last take.
func (u *gitUpstream) takeMirrorRequests() []string {
	u.mirrorMu.Lock()
	defer u.mirrorMu.Unlock()
	taken := u.mirrorRequests
	u.mirrorRequests = nil

	return taken
}

// repositoryID is the id GitHub's API gives a name, a new one the first time
// the name is asked about.
func (u *gitUpstream) repositoryID(name string) int64 {
	id, _ := u.ids.LoadOrStore(name, u.nextID.Add(1))
	repository, ok := id.(int64)
	if !ok {
		panic(fmt.Sprintf("the stand-in's id for %s is a %T", name, id))
	}

	return repository
}

// recreate deletes acme/api and makes another repository of that name, with a
// history of its own and a new id, as an owner can once the first is renamed
// or deleted.
func (u *gitUpstream) recreate(t *testing.T) string {
	t.Helper()

	work := filepath.Join(t.TempDir(), "recreated")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "init", "-q", "-b", "main")
	runIn(t, work, "commit", "-q", "--allow-empty", "-m", "another repository")
	if err := os.RemoveAll(u.repo); err != nil {
		t.Fatal(err)
	}
	runIn(t, u.root, "clone", "-q", "--bare", work, u.repo)
	githubLike(t, u.repo)
	u.ids.Store("api", u.nextID.Add(1))

	return runIn(t, u.repo, "rev-parse", "main")
}

// holdMirrorFetches holds every fetch the node's own git makes until release
// is called, which the test's end does too.
func (u *gitUpstream) holdMirrorFetches(t *testing.T) (release func()) {
	t.Helper()

	gate := make(chan struct{})
	u.hold.Store(&gate)
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)

	return release
}

// addRepository makes another private repository beside acme/api, with one
// commit of its own, and returns that commit.
func (u *gitUpstream) addRepository(t *testing.T, name string) string {
	t.Helper()

	work := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "init", "-q", "-b", "main")
	runIn(t, work, "commit", "-q", "--allow-empty", "-m", "only in "+name)
	bare := filepath.Join(u.root, "acme", name+".git")
	runIn(t, u.root, "clone", "-q", "--bare", work, bare)
	githubLike(t, bare)

	return runIn(t, bare, "rev-parse", "main")
}

// githubLike configures a bare repository to answer as github.com does.
func githubLike(t *testing.T, bare string) {
	t.Helper()

	runIn(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	runIn(t, bare, "config", "uploadpack.allowFilter", "true")
}

// commitTo commits files to the upstream repository's branch and returns the
// commit.
func (u *gitUpstream) commitTo(t *testing.T, branch string, files map[string]string) string {
	t.Helper()

	work := filepath.Join(t.TempDir(), "w")
	runIn(t, filepath.Dir(work), "clone", "-q", u.repo, work)
	runIn(t, work, "checkout", "-q", "-B", branch)
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runIn(t, work, "add", "-A")
	runIn(t, work, "commit", "-q", "--allow-empty", "-m", "commit to "+branch)
	runIn(t, work, "push", "-q", "-f", "origin", branch)

	return runIn(t, work, "rev-parse", "HEAD")
}

func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(isolatedGitEnviron(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+dir,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}

	return strings.TrimSpace(string(output))
}

func newGitUpstream(t *testing.T) *gitUpstream {
	t.Helper()

	probe := exec.CommandContext(t.Context(), "git", "--exec-path")
	probe.Env = append(isolatedGitEnviron(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	execPath, err := probe.Output()
	if err != nil {
		t.Skip("no git on this host")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "init", "-q", "-b", "main")
	runIn(t, work, "commit", "-q", "--allow-empty", "-m", "one")
	repo := filepath.Join(root, "acme", "api.git")
	runIn(t, root, "clone", "-q", "--bare", work, repo)
	githubLike(t, repo)

	upstream := &gitUpstream{root: root, repo: repo}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A RENAMED REPOSITORY IS A REDIRECT, as github.com answers one; a
		// transferred one names another owner.
		for _, from := range []string{"/acme/old.git/", "/other/moved.git/"} {
			if old, ok := strings.CutPrefix(r.URL.Path, from); ok {
				http.Redirect(w, r, upstream.URL+"/acme/api.git/"+old+"?"+r.URL.RawQuery,
					http.StatusMovedPermanently)

				return
			}
		}
		// THE REST API ANSWERS A NAME IT CANNOT SHOW THIS HEADER 404, as
		// api.github.com does for a private repository (measured, 2026-10-21).
		if name, ok := strings.CutPrefix(r.URL.Path, "/api/repos/acme/"); ok {
			upstream.apiCalls.Add(1)
			switch {
			case upstream.apiDown.Load():
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			case r.Header.Get("Authorization") != githubBasic || upstream.refuse.Load():
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			default:
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":%d,"name":%q}`, upstream.repositoryID(name), name)
			}

			return
		}
		if r.Header.Get("Authorization") != githubBasic || upstream.refuse.Load() {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)

			return
		}
		// A FETCH IS A NEGOTIATION FOR OBJECTS: under v2 a clone also POSTs an
		// ls-refs, which moves nothing.
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "unreadable", http.StatusBadRequest)

				return
			}
			plain := body
			if r.Header.Get("Content-Encoding") == "gzip" {
				if reader, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
					if plain, err = io.ReadAll(reader); err != nil {
						http.Error(w, "unreadable", http.StatusBadRequest)

						return
					}
				}
			}
			if bytes.Contains(plain, []byte("command=")) {
				upstream.recordMirrorRequest(plain)
			}
			if bytes.Contains(plain, []byte("command=fetch")) || bytes.Contains(plain, []byte("want ")) {
				upstream.fetches.Add(1)
			}
			if bytes.Contains(plain, []byte("command=fetch")) {
				upstream.mirrorFetches.Add(1)
				if hook := upstream.onMirrorFetch.Load(); hook != nil {
					(*hook)()
				}
				if gate := upstream.hold.Load(); gate != nil {
					select {
					case <-*gate:
					case <-r.Context().Done():
						return
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)

	return upstream
}

func gitCache() *config.CacheSpec {
	enabled := true
	spec := config.Tier{Cache: &config.TierCache{Git: &config.CacheToggle{Enabled: &enabled}}}.EffectiveCache()

	return &spec
}

// gitClient is a guest: its git rewrites github.com to the node and answers the
// node's challenge with the session bearer and the job's header, as billet's
// credential helper does.
func gitClient(t *testing.T, node *httptest.Server, token, header string) func(args ...string) (string, error) {
	t.Helper()

	home := t.TempDir()
	helper := filepath.Join(home, "helper")
	if err := forkSafeWriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] || exit 0\n"+
		"echo username="+token+"\necho password="+header+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitconfig := "[url \"" + node.URL + "/v1/git/github.com/\"]\n\tinsteadOf = https://github.com/\n" +
		"[credential \"" + node.URL + "\"]\n\thelper = " + helper + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	return func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = home
		cmd.Env = append(isolatedGitEnviron(), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "HOME="+home, "GIT_TERMINAL_PROMPT=0")
		output, err := cmd.CombinedOutput()

		return string(output), err
	}
}

// isolatedGitEnviron is this process's environment with nothing git reads
// from it: no GIT_ variable, tracing and configuration included, no
// XDG_CONFIG_HOME, and not the live tests' token. A caller names the
// configuration its git reads, each a trace setting could otherwise come from
// and print the credentials git sends.
//
// THE VARIABLES ARE LEFT OUT, NOT SET EMPTY: git turns curl's verbose trace on
// when GIT_CURL_VERBOSE is set at all, empty included (measured, git 2.43,
// 2026-10-22).
func isolatedGitEnviron() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "GIT_") && name != "XDG_CONFIG_HOME" && name != "BILLET_LIVE_GIT_TOKEN" {
			env = append(env, entry)
		}
	}

	return env
}

func gitNode(t *testing.T, upstream *gitUpstream) (*CacheService, *httptest.Server, string) {
	t.Helper()

	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.api = upstream.URL, upstream.URL+"/api"
	service.git.fullFraction = 1
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	return service, node, token
}

// A SECOND CLONE IS SERVED FROM THE NODE'S MIRROR, fetching nothing from
// github.com, and has what github.com has.
func TestAGitCloneIsServedFromTheMirror(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	git := gitClient(t, node, token, githubBasic)
	want := runIn(t, upstream.repo, "rev-parse", "main")

	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "first"); err != nil {
		t.Fatalf("the first clone: %v\n%s", err, output)
	}
	mirrored := upstream.fetches.Load()
	if mirrored != 1 {
		t.Fatalf("github.com served %d fetches for the first clone, want the mirror's one", mirrored)
	}
	if output, err := git("clone", "-q", "https://github.com/acme/api", "second"); err != nil {
		t.Fatalf("the second clone: %v\n%s", err, output)
	}
	if upstream.fetches.Load() != mirrored {
		t.Fatalf("the second clone fetched from github.com (%d fetches)", upstream.fetches.Load())
	}
	if got, err := git("-C", "second", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(got) != want {
		t.Fatalf("the second clone is at %q (%v), want %q", got, err, want)
	}
}

// THE JOB THAT MADE A MIRROR REPORTS THE GIT CACHE COLD, and the next job it
// serves reports it warm.
func TestTheGitCacheIsReportedColdThenWarm(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, first := gitNode(t, upstream)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	second, err := service.PrepareScoped(provider.InstanceName("second-lease"), CacheSessionScope{
		Trust: provider.TrustUntrusted, LeaseID: "second-lease", Epoch: 1, Cache: gitCache(),
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	for i, token := range []string{first, second.Token} {
		if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
			"https://github.com/acme/api.git", "c"); err != nil {
			t.Fatalf("clone %d: %v\n%s", i, err, output)
		}
	}
	for instance, want := range map[string]alloc.BuildCache{
		provider.InstanceName(scopedLease):    alloc.BuildCacheCold,
		provider.InstanceName("second-lease"): alloc.BuildCacheWarm,
	} {
		if err := service.Close(t.Context(), instance); err != nil {
			t.Fatalf("Close %s: %v", instance, err)
		}
		calls := observer.recorded()
		if got := calls[len(calls)-1].obs.Git; got != want {
			t.Errorf("%s reported the git cache %q, want %q", instance, got, want)
		}
	}
}

// A JOB GITHUB DOES NOT AUTHORISE IS SERVED NOTHING FROM THE MIRROR, even one
// another job filled: GitHub's refusal is what it gets.
func TestAGitFetchGitHubRefusesIsNotServedFromTheMirror(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "filled"); err != nil {
		t.Fatalf("the filling clone: %v\n%s", err, output)
	}
	for name, header := range map[string]string{"no header": "-", "another header": "basic d3Jvbmc="} {
		if output, err := gitClient(t, node, token, header)("clone", "-q",
			"https://github.com/acme/api.git", "refused"); err == nil {
			t.Errorf("%s: a clone GitHub refuses was served:\n%s", name, output)
		}
	}
	if output, err := gitClient(t, node, "not-a-session", githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "refused"); err == nil {
		t.Errorf("a clone with no session was served:\n%s", output)
	}

	// A NEGOTIATION SENT STRAIGHT TO THE FETCH, with no advertisement GitHub
	// authorised first, is GitHub's to answer too.
	if body := negotiate(t, node, token, "basic d3Jvbmc=", runIn(t, upstream.repo, "rev-parse", "main")); bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("an unauthorised negotiation was answered from the mirror: %q", body[:min(len(body), 80)])
	}
}

// AN OBJECT NO REF REACHES IS SERVED FROM THE MIRROR, as github.com serves it:
// a deleted branch's commit, tree and blob alike (ADR-018). The stand-in serves
// them too, so the mirror is checked to be what answered.
func TestAnObjectNoRefReachesIsServedFromTheMirror(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	secret := upstream.commitTo(t, "secret", map[string]string{"credential": "hunter2\n"})
	objects := map[string]string{
		"commit": secret,
		"tree":   runIn(t, upstream.repo, "rev-parse", secret+"^{tree}"),
		"blob":   runIn(t, upstream.repo, "rev-parse", secret+":credential"),
	}
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "--single-branch", "--branch", "main",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	runIn(t, upstream.repo, "branch", "-q", "-D", "secret")
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	if output := runIn(t, upstream.repo, "branch", "--list", "secret"); output != "" {
		t.Fatalf("the branch was not deleted upstream: %q", output)
	}
	for kind, object := range objects {
		before := upstream.fetches.Load()
		if body := negotiate(t, node, token, githubBasic, object); !bytes.Contains(body, []byte("PACK")) {
			t.Errorf("a %s no ref reaches was not served: %q", kind, body[:min(len(body), 80)])
		}
		if got := upstream.fetches.Load() - before; got != 0 {
			t.Errorf("a %s no ref reaches went to github.com (%d fetches), want the mirror", kind, got)
		}
	}
}

// A PULL REQUEST'S SUPERSEDED MERGE COMMIT IS FETCHED BY ITS NAME, as
// actions/checkout fetches one when a job of an earlier push runs after the
// pull request moved; the mirror holds it and no ref reaches it.
func TestASupersededMergeCommitIsFetchedByName(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	merge := upstream.commitTo(t, "topic", map[string]string{"a": "1\n"})
	runIn(t, upstream.repo, "update-ref", "refs/pull/1/merge", merge)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	// THE PULL REQUEST MOVES: GitHub's merge ref names a new commit, and the
	// mirror, refreshed to it, keeps the old one with no ref reaching it.
	moved := upstream.commitTo(t, "topic", map[string]string{"a": "2\n"})
	runIn(t, upstream.repo, "update-ref", "refs/pull/1/merge", moved)
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}

	if output, err := git("init", "-q", "w"); err != nil {
		t.Fatalf("init: %v\n%s", err, output)
	}
	if output, err := git("-C", "w", "remote", "add", "origin", "https://github.com/acme/api"); err != nil {
		t.Fatalf("remote add: %v\n%s", err, output)
	}
	before := upstream.fetches.Load()
	if output, err := git("-C", "w", "fetch", "--no-tags", "--prune", "--no-recurse-submodules", "--depth=1",
		"origin", "+"+merge+":refs/remotes/pull/1/merge"); err != nil {
		t.Fatalf("the fetch of a superseded merge commit failed: %v\n%s", err, output)
	}
	if got := upstream.fetches.Load() - before; got != 0 {
		t.Fatalf("the superseded merge commit was fetched from github.com (%d fetches), want the mirror", got)
	}
	if output, err := git("-C", "w", "rev-parse", "refs/remotes/pull/1/merge"); err != nil || strings.TrimSpace(output) != merge {
		t.Fatalf("the fetched ref is %q (%v), want %s", output, err, merge)
	}
}

// A BLOBLESS CLONE'S CHECKOUT IS SERVED FROM THE MIRROR: the lazy fetch of every
// file it names, which a full-history job makes, is answered by the node.
func TestABloblessCheckoutIsServedFromTheMirror(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	upstream.commitTo(t, "main", map[string]string{"README": "hello\n", "src/main.go": "package main\n"})
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "--filter=blob:none", "--no-checkout",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("the blobless clone: %v\n%s", err, output)
	}
	// THE CLONE HOLDS NO FILE, so the checkout has to fetch every one of them.
	// --missing=print lists what the clone lacks without fetching it.
	blob := runIn(t, upstream.repo, "rev-parse", "main:src/main.go")
	if output, err := git("-C", "c", "rev-list", "--objects", "--missing=print", "main"); err != nil ||
		!strings.Contains(output, "?"+blob) {
		t.Fatalf("the blobless clone already holds the file, so its checkout fetches nothing (%v):\n%s", err, output)
	}
	before := upstream.fetches.Load()
	if output, err := git("-C", "c", "checkout", "-q", "main"); err != nil {
		t.Fatalf("the checkout: %v\n%s", err, output)
	}
	if got := upstream.fetches.Load() - before; got != 0 {
		t.Fatalf("the checkout's lazy fetch went to github.com (%d fetches), want the mirror", got)
	}
	if output, err := git("-C", "c", "diff", "--quiet", "HEAD"); err != nil {
		t.Fatalf("the checkout does not match its commit: %v\n%s", err, output)
	}
	if output, err := git("-C", "c", "cat-file", "-p", "HEAD:src/main.go"); err != nil || output != "package main\n" {
		t.Fatalf("the checkout has %q (%v)", output, err)
	}
}

// AN OBJECT OF ANOTHER REPOSITORY IS NEVER SERVED FROM THIS ONE'S MIRROR, though
// the node mirrors both: each repository's mirror holds only what GitHub gave
// for that repository, which is what makes serving any object it holds safe.
func TestAnotherRepositorysObjectIsNotServed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	other := upstream.addRepository(t, "other")
	git := gitClient(t, node, token, githubBasic)
	for _, repo := range []string{"api", "other"} {
		if output, err := git("clone", "-q", "https://github.com/acme/"+repo+".git", repo); err != nil {
			t.Fatalf("clone %s: %v\n%s", repo, err, output)
		}
	}
	before := upstream.fetches.Load()
	if body := negotiate(t, node, token, githubBasic, other); bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("acme/api served an object only acme/other holds: %q", body[:min(len(body), 80)])
	}
	if upstream.fetches.Load() == before {
		t.Fatal("a want acme/api's mirror lacks was not github.com's to answer")
	}
}

// A FETCH WHOSE MIRROR WAS NOT REFRESHED IS STILL CORRECT: what the stale mirror
// holds it serves, and a commit pushed since, which it lacks, GitHub serves.
func TestAFetchAStaleMirrorLacksGoesToGitHub(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	service.git.fullFraction = 0
	pushed := upstream.commitTo(t, "main", map[string]string{"new": "1\n"})
	before := upstream.fetches.Load()
	if output, err := git("-C", "c", "pull", "-q", "--ff-only"); err != nil {
		t.Fatalf("a pull past a stale mirror: %v\n%s", err, output)
	}
	if output, err := git("-C", "c", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(output) != pushed {
		t.Fatalf("the pull is at %q (%v), want %s", output, err, pushed)
	}
	if upstream.fetches.Load() == before {
		t.Fatal("a commit the stale mirror lacks was not fetched from github.com")
	}
}

// THE JOB'S HEADER NEVER REACHES git's ARGUMENTS, which every process on the
// node can read.
func TestTheJobsHeaderIsNeverAnArgument(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	record := filepath.Join(t.TempDir(), "argv")
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := forkSafeWriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >>"+record+"\nexec git \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service.git.binary = wrapper
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	argv, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(argv), "fetch") {
		t.Fatalf("the node's git was not run through the wrapper: %v", err)
	}
	if strings.Contains(string(argv), strings.TrimPrefix(githubBasic, "basic ")) {
		t.Fatalf("the job's header reached git's arguments:\n%s", argv)
	}
}

// A REPOSITORY LARGER THAN THE TIER'S GIT CACHE IS NOT KEPT: its clone is
// GitHub's, and so is the next one, rather than a mirror fetched and removed
// for every job.
func TestARepositoryAboveTheCeilingIsForwarded(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	spec := gitCache()
	spec.Git.MaxSize = 1
	service, _, token, _ := casService(t, provider.TrustUntrusted, spec, &fakeCacheStore{})
	service.git.upstream, service.git.api, service.git.fullFraction = upstream.URL, upstream.URL+"/api", 1
	// A MIRROR ATTEMPT IS COUNTED AT THE NODE'S git, BY ITS init: the watchdog
	// stops a fetch past a one-byte ceiling at its first tick, which under load
	// comes before that fetch has asked GitHub for anything or even started,
	// but the init runs before the watchdog does, and a mirror removed as too
	// large has to be made again.
	record := filepath.Join(t.TempDir(), "subcommands")
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := forkSafeWriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >>"+record+"\nexec git \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service.git.binary = wrapper
	mirrorsMade := func() int {
		subcommands, err := os.ReadFile(record)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		n := 0
		for line := range strings.Lines(string(subcommands)) {
			if strings.TrimSpace(line) == "init" {
				n++
			}
		}
		return n
	}
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)
	git := gitClient(t, node, token, githubBasic)

	// A clone whose fetch slot was not admitted within gitFetchWait is GitHub's
	// and makes no mirror, which a stalled machine can cause; so clone until
	// one has, a bounded number of times.
	for attempt := 0; mirrorsMade() == 0; attempt++ {
		if attempt == 3 {
			t.Fatal("no clone made a mirror in three attempts")
		}
		if output, err := git("clone", "-q", "https://github.com/acme/api.git", fmt.Sprintf("first%d", attempt)); err != nil {
			t.Fatalf("clone first: %v\n%s", err, output)
		}
	}
	if got := mirrorsMade(); got != 1 {
		t.Fatalf("the first clone made %d mirrors, want 1", got)
	}
	// THE MIRROR IS GONE AND THE REPOSITORY MARKED, before the second clone:
	// a mirror left behind by a fetch that failed for another reason would let
	// the second clone fetch into it without making it again.
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the mirror above the tier's ceiling was not removed: %v", err)
	}
	service.git.mu.Lock()
	marked := service.git.tooLarge["untrusted/acme/api"]
	service.git.mu.Unlock()
	if !marked.After(time.Now()) {
		t.Fatalf("the repository is not marked too large (until %v)", marked)
	}
	before := upstream.fetches.Load()
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "second"); err != nil {
		t.Fatalf("clone second: %v\n%s", err, output)
	}
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); err == nil {
		t.Fatal("a mirror above the tier's ceiling was kept")
	}
	if got := mirrorsMade(); got != 1 {
		t.Fatalf("the second clone made a mirror again (%d in all), want it forwarded", got)
	}
	// AT LEAST the second client's own fetch: the first mirror fetch, stopped,
	// can still be counted late by a handler the kill did not join.
	if got := upstream.fetches.Load() - before; got < 1 {
		t.Fatalf("github.com served the second clone %d fetches, want its own", got)
	}
}

// A MIRROR NO JOB USED WITHIN ITS RETENTION IS REMOVED; one in use is kept, and
// a full filesystem removes the least recently used first.
func TestUnusedGitMirrorsAreReaped(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.fullFraction = 1
	now := time.Now()
	service.now = func() time.Time { return now }
	mirror := func(key string, used time.Time) string {
		path := service.git.mirrorPath(key)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		stamp := filepath.Join(path, gitUsedStamp)
		if err := os.WriteFile(stamp, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(stamp, used, used); err != nil {
			t.Fatal(err)
		}

		return path
	}
	stale := mirror("untrusted/acme/old", now.Add(-gitMirrorRetention-time.Hour))
	fresh := mirror("untrusted/acme/new", now.Add(-time.Hour))
	older := mirror("trusted/acme/older", now.Add(-2*time.Hour))

	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	for path, want := range map[string]bool{stale: false, fresh: true, older: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", path, err == nil, want)
		}
	}

	service.git.fullFraction = 0
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	for _, path := range []string{fresh, older} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s survived a full filesystem", path)
		}
	}
}

// A TIER WITHOUT THE GIT CACHE IS FORWARDED to github.com untouched.
func TestATierWithoutTheGitCacheIsForwarded(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	legacy := config.Tier{}.EffectiveCache()
	service, _, token, _ := casService(t, provider.TrustUntrusted, &legacy, &fakeCacheStore{})
	service.git.upstream = upstream.URL
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(service.git.root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read the mirror root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a tier without the git cache made a mirror: %v", entries)
	}
	if upstream.fetches.Load() != 1 {
		t.Fatalf("github.com served %d fetches, want the client's own", upstream.fetches.Load())
	}
}

// negotiate posts a v0 fetch negotiation for one object straight to the node,
// as a client that skips its own checks would, and returns the answer.
func negotiate(t *testing.T, node *httptest.Server, token, header, want string) []byte {
	t.Helper()

	line := "want " + want + "\n"
	negotiation := fmt.Sprintf("%04x%s00000009done\n", len(line)+4, line)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		node.URL+"/v1/git/github.com/acme/api.git/git-upload-pack", strings.NewReader(negotiation))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(token, header)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return body
}

// GITHUB'S LATEST ANSWER STANDS: a header refused after it was granted is
// served nothing more from the mirror, even a negotiation sent within the
// minute the grant would have lasted.
func TestARefusalEndsAnEarlierGrant(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	main := runIn(t, upstream.repo, "rev-parse", "main")
	if body := negotiate(t, node, token, githubBasic, main); !bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("a granted negotiation was not served: %q", body)
	}

	upstream.refuse.Store(true)
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err == nil {
		t.Fatalf("a refused header listed the refs:\n%s", output)
	}
	if body := negotiate(t, node, token, githubBasic, main); bytes.Contains(body, []byte("PACK")) {
		t.Fatal("the mirror served a header GitHub had since refused")
	}
}

// AN ANSWER TO AN OLDER QUESTION NEVER REPLACES A NEWER ONE: a grant asked for
// before a refusal, and landing after it, leaves the refusal standing.
func TestAnOlderGrantDoesNotOvertakeANewerRefusal(t *testing.T) {
	t.Parallel()

	g := newGitProxy(t.TempDir())
	request := gitRequest{session: &cacheSession{trust: provider.TrustUntrusted}, owner: "acme",
		repo: "api", auth: "basic eA=="}
	now := time.Now()
	earlier, later := g.ask(), g.ask()
	g.remember(request, now, later, false, 0)
	g.remember(request, now, earlier, true, 7)
	if _, ok := g.granted(request, now); ok {
		t.Fatal("a grant asked for before a refusal overtook it")
	}
	g.remember(request, now, g.ask(), true, 7)
	if repository, ok := g.granted(request, now); !ok || repository != 7 {
		t.Fatal("a grant asked for after the refusal did not stand")
	}
}

// A FETCH IS STOPPED WHILE IT RUNS once its mirror passes the tier's ceiling,
// not only measured after it finishes: a slow upstream here holds the fetch
// open far longer than the watchdog takes.
func TestAFetchIsStoppedWhileItRunsAtTheCeiling(t *testing.T) {
	t.Parallel()

	// THE UPSTREAM NEVER ANSWERS WHILE THE TEST RUNS, so only the watchdog can
	// end the fetch and a fetch without one is still running at the guard.
	ended := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-ended:
		}
		http.Error(w, "slow", http.StatusServiceUnavailable)
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(ended) })
	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.fullFraction = slow.URL, 1
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api"}

	err := gitWithin(t, "a fetch past its ceiling", func() error {
		_, err := service.fetchMirror(t.Context(), service.git.mirrorPath(request.key()), request, 7, 1, gitFetchPlan{})

		return err
	})
	if !errors.Is(err, errMirrorTooLarge) {
		t.Fatalf("fetchMirror = %v, want errMirrorTooLarge", err)
	}
}

// A REPOSITORY GITHUB REDIRECTS IS FOLLOWED THROUGH THE PROXY, so the client's
// next request still carries the credentials the rewrite depends on.
func TestARedirectIsFollowedThroughTheProxy(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/old.git", "c"); err != nil {
		t.Fatalf("a clone of a renamed repository failed: %v\n%s", err, output)
	}
}

// THE REAPER PASSES OVER A MIRROR IN USE, rather than hold the cache loop until
// the transfer ends.
func TestTheReaperPassesOverAMirrorInUse(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.fullFraction = 0
	path := service.git.mirrorPath("untrusted/acme/busy")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, gitUsedStamp), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	held := service.git.mirror("untrusted/acme/busy")
	held.use.RLock()
	defer held.use.RUnlock()

	// THE HOLD OUTLASTS THE GUARD, so a reaper that waited for the mirror
	// rather than passing over it never returns.
	if err := gitWithin(t, "the reaper beside a mirror in use", func() error {
		return service.ReapGitMirrors(t.Context())
	}); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a mirror in use was removed: %v", err)
	}
}

// ADMISSION WAITS ONLY AS LONG AS THE REQUEST MAY, and gives back what it took
// when it cannot take everything.
func TestGitAdmissionIsBoundedAndGivesBack(t *testing.T) {
	t.Parallel()

	free, full := make(chan struct{}, 1), make(chan struct{}, 1)
	full <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := admitGit(ctx, free, full); err == nil {
		t.Fatal("admission past a full bound succeeded")
	}
	if len(free) != 0 {
		t.Fatal("a failed admission kept the slot it had taken")
	}
	release, err := admitGit(t.Context(), free)
	if err != nil || len(free) != 1 {
		t.Fatalf("admission = %v with %d held", err, len(free))
	}
	release()
	if len(free) != 0 {
		t.Fatal("release gave nothing back")
	}
}

// A SHALLOW NEGOTIATION IS SERVED FROM THE MIRROR when it holds every object the
// negotiation names, its shallow boundary included, and is GitHub's otherwise.
func TestAShallowNegotiationIsServedWhenTheMirrorHoldsItsBoundary(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	main := runIn(t, upstream.repo, "rev-parse", "main")
	shallowNegotiation := func(boundary string) string {
		shallow := "shallow " + boundary + "\n"
		want := "want " + main + "\n"
		deepen := "deepen 2147483647\n"

		return fmt.Sprintf("%04x%s%04x%s%04x%s00000009done\n", len(want)+4, want,
			len(shallow)+4, shallow, len(deepen)+4, deepen)
	}
	post := func(negotiation string) []byte {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			node.URL+"/v1/git/github.com/acme/api.git/git-upload-pack", strings.NewReader(negotiation))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(token, githubBasic)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		return body
	}

	before := upstream.fetches.Load()
	if body := post(shallowNegotiation(main)); !bytes.Contains(body, []byte("PACK")) || bytes.Contains(body, []byte("ERR ")) {
		t.Fatalf("a shallow negotiation the mirror holds was not answered with a pack: %q", body[:min(len(body), 120)])
	}
	if got := upstream.fetches.Load() - before; got != 0 {
		t.Fatalf("a shallow negotiation the mirror can answer went to github.com (%d fetches)", got)
	}
	unknown := strings.Repeat("ab", 20)
	before = upstream.fetches.Load()
	post(shallowNegotiation(unknown))
	if upstream.fetches.Load() == before {
		t.Fatal("a shallow boundary the mirror lacks was answered from the mirror")
	}
}

// THE NEGOTIATION'S OBJECTS ARE EVERY WANT AND SHALLOW BEFORE ITS FIRST FLUSH, and
// only full object names: anything else is not a negotiation the proxy judges,
// because cat-file would read it as a revision.
func TestNegotiatedObjects(t *testing.T) {
	t.Parallel()

	a, b := strings.Repeat("a", 40), strings.Repeat("b", 64)
	line := func(payload string) string { return fmt.Sprintf("%04x%s", len(payload)+4, payload) }
	for _, c := range []struct {
		name        string
		negotiation string
		want        []string
		ok          bool
	}{
		{"wants and shallows", line("want "+a+" multi_ack side-band-64k\n") + line("want "+b+"\n") +
			line("shallow "+a+"\n") + line("deepen 1\n") + "0000" + line("have "+b+"\n") + "0009done\n",
			[]string{a, b, a}, true},
		{"no wants", "0000", nil, true},
		{"a revision, not a name", line("want HEAD~1\n") + "0000", nil, false},
		{"an abbreviated name", line("want "+a[:12]+"\n") + "0000", nil, false},
		{"an uppercase name", line("want "+strings.ToUpper(a)+"\n") + "0000", nil, false},
		{"a v2 command", line("command=fetch\n") + "0001" + line("want "+a+"\n") + "0000", nil, false},
		{"no flush", line("want " + a + "\n"), nil, false},
		{"a truncated line", "00ffwant " + a, nil, false},
		{"not a pkt-line", "zzzz", nil, false},
	} {
		got, ok := negotiatedObjects([]byte(c.negotiation))
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: negotiatedObjects = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// A REFUSAL OUTLIVES ANSWERS ABOUT OTHER HEADERS: an unrelated question
// recorded after it must not clear it, or an older grant for the refused
// header would land as though nothing newer had been said.
func TestARefusalOutlivesUnrelatedAnswers(t *testing.T) {
	t.Parallel()

	g := newGitProxy(t.TempDir())
	session := &cacheSession{trust: provider.TrustUntrusted}
	a := gitRequest{session: session, owner: "acme", repo: "api", auth: "basic YQ=="}
	b := gitRequest{session: session, owner: "acme", repo: "api", auth: "basic Yg=="}
	now := time.Now()
	older, refusal, unrelated := g.ask(), g.ask(), g.ask()
	g.remember(a, now, refusal, false, 0)
	g.remember(b, now.Add(gitAuthorisationAge), unrelated, true, 7)
	g.remember(a, now.Add(gitAuthorisationAge), older, true, 7)
	if _, ok := g.granted(a, now.Add(gitAuthorisationAge)); ok {
		t.Fatal("an unrelated answer cleared a refusal and let an older grant stand")
	}
}

// ONLY THE SCOPE OWNER'S REPOSITORIES ARE MIRRORED for a scoped session; any
// other repository is GitHub's to serve, so a job cannot fill the node with
// mirrors of whatever it names.
func TestOnlyTheScopeOwnersRepositoriesAreMirrored(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	spec := gitCache()
	spec.Publish, spec.Owner, spec.Repository = config.CachePublishDefaultBranch, "other", "repo"
	service, _, token, _ := casService(t, provider.TrustUntrusted, spec, &fakeCacheStore{})
	service.git.upstream, service.git.api, service.git.fullFraction = upstream.URL, upstream.URL+"/api", 1
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); err == nil {
		t.Fatal("a repository outside the session's owner was mirrored")
	}

	// NOR ONE THAT ONLY REDIRECTS FROM THE OWNER: a repository transferred out
	// of the scope is judged where it now lives.
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/other/moved.git", "d"); err != nil {
		t.Fatalf("clone of a transferred repository: %v\n%s", err, output)
	}
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); err == nil {
		t.Fatal("a repository transferred out of the session's owner was mirrored")
	}
}

// AN ADVERTISEMENT ALWAYS ASKS ABOUT THE NAME IT WAS GIVEN, so a rename is
// believed only while GitHub is giving it, and only to the header it was given
// to.
func TestARenameIsFollowedOnlyForTheFetchItWasGivenTo(t *testing.T) {
	t.Parallel()

	g := newGitProxy(t.TempDir())
	session := &cacheSession{trust: provider.TrustUntrusted}
	old := gitRequest{session: session, owner: "acme", repo: "old", auth: "basic YQ=="}
	resp := &gitAnswer{StatusCode: http.StatusMovedPermanently, Header: http.Header{
		"Location": {g.upstream + "/acme/api.git/info/refs?service=git-upload-pack"}}}
	now := time.Now()
	if _, ok := g.learnRename(old, resp, now, g.ask()); !ok {
		t.Fatal("the redirect was not learned")
	}
	if moved := g.follow(old, now); moved.repo != "api" {
		t.Fatalf("the same header's fetch went to %q", moved.repo)
	}
	other := old
	other.auth = "basic Yg=="
	if moved := g.follow(other, now); moved.repo != "old" {
		t.Fatal("another header followed a rename it was never given")
	}
	if moved := g.follow(old, now.Add(gitRenameAge)); moved.repo != "old" {
		t.Fatal("a rename was followed after its grant expired")
	}
}

// A BUSY FETCH SLOT SENDS THE JOB TO GITHUB PROMPTLY, rather than holding its
// checkout behind another job's fetch.
func TestABusyFetchSlotFallsBackPromptly(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	// THE SLOTS ARE NEVER GIVEN BACK, so a refresh that waited for one rather
	// than for gitFetchWait never answers.
	for range gitFetches {
		service.git.fetches <- struct{}{}
	}
	git := gitClient(t, node, token, githubBasic)
	type cloned struct {
		output string
		err    error
	}
	result := gitWithin(t, "a clone behind busy fetch slots", func() cloned {
		output, err := git("clone", "-q", "https://github.com/acme/api.git", "c")

		return cloned{output, err}
	})
	if result.err != nil {
		t.Fatalf("clone while every slot is busy: %v\n%s", result.err, result.output)
	}
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); err == nil {
		t.Fatal("a mirror was fetched while every slot was held")
	}
}

// A CANCELLED GIT TAKES ITS CHILDREN WITH IT: fetch runs index-pack, which
// would go on writing after the slot that bounded it was released.
func TestACancelledCommandEndsItsChildren(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & echo $! >"+pidFile+"; wait")
	killGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for range 100 {
		if raw, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			if child, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
				t.Fatalf("the child's pid %q: %v", raw, err)
			}

			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the child never started")
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("a cancelled command exited cleanly")
	}
	for range 100 {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Errorf("kill the surviving child: %v", err)
	}
	t.Fatal("the child outlived its cancelled parent")
}

// A RENAME ENDS WITH THE NEXT ANSWER THAT IS NOT ONE: a refusal, or could not
// tell, about the old name must not leave its fetch following the rename to a
// repository whose grant is still held.
func TestARenameIsWithdrawnByALaterAnswer(t *testing.T) {
	t.Parallel()

	g := newGitProxy(t.TempDir())
	session := &cacheSession{trust: provider.TrustUntrusted}
	old := gitRequest{session: session, owner: "acme", repo: "old", auth: "basic YQ=="}
	redirect := &gitAnswer{StatusCode: http.StatusMovedPermanently, Header: http.Header{
		"Location": {g.upstream + "/acme/api.git/info/refs?service=git-upload-pack"}}}
	refusal := &gitAnswer{StatusCode: http.StatusNotFound, Header: http.Header{}}
	now := time.Now()

	older, learned, refused := g.ask(), g.ask(), g.ask()
	g.learnRename(old, redirect, now, learned)
	if moved, _ := g.learnRename(old, refusal, now, refused); moved.repo != "old" {
		t.Fatalf("a refusal answered with %q", moved.repo)
	}
	if moved := g.follow(old, now); moved.repo != "old" {
		t.Fatal("a fetch followed a rename GitHub had since stopped giving")
	}
	if _, ok := g.learnRename(old, redirect, now, older); ok {
		t.Fatal("an older redirect overtook the withdrawal")
	}
	if moved := g.follow(old, now); moved.repo != "old" {
		t.Fatal("an older redirect was followed after the withdrawal")
	}
	unknown := g.ask()
	g.learnRename(old, redirect, now, g.ask())
	g.learnRename(old, nil, now, unknown)
	if moved := g.follow(old, now); moved.repo != "api" {
		t.Fatal("an older could-not-tell withdrew a newer rename")
	}
	g.learnRename(old, nil, now, g.ask())
	if moved := g.follow(old, now); moved.repo != "old" {
		t.Fatal("could not tell left the rename standing")
	}
}

// RENAMES ARE FORGOTTEN once no question they could order is still in flight,
// so headers that come and go do not grow the node's memory.
func TestRenamesAreForgotten(t *testing.T) {
	t.Parallel()

	g := newGitProxy(t.TempDir())
	session := &cacheSession{trust: provider.TrustUntrusted}
	redirect := &gitAnswer{StatusCode: http.StatusMovedPermanently, Header: http.Header{
		"Location": {g.upstream + "/acme/api.git/info/refs?service=git-upload-pack"}}}
	now := time.Now()
	for i := range 50 {
		request := gitRequest{session: session, owner: "acme", repo: "old", auth: gitAuth(fmt.Sprint("basic ", i))}
		g.learnRename(request, redirect, now, g.ask())
	}
	last := gitRequest{session: session, owner: "acme", repo: "old", auth: "basic last"}
	g.learnRename(last, redirect, now.Add(gitGrantMemory+time.Second), g.ask())
	g.mu.Lock()
	held := len(g.renamed)
	g.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d renames held after their memory, want only the newest", held)
	}
}

// scriptedGitHub answers info/refs per repository from a table a test changes
// as it goes: a redirect to another repository, a status, or a gate that holds
// the answer until the test releases it.
type scriptedGitHub struct {
	*httptest.Server
	mu      sync.Mutex
	answers map[string]scriptedAnswer
}

type scriptedAnswer struct {
	redirect string
	status   int
	arrived  chan struct{}
	gate     chan struct{}
}

func newScriptedGitHub(t *testing.T) *scriptedGitHub {
	t.Helper()

	github := &scriptedGitHub{answers: map[string]scriptedAnswer{}}
	github.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/acme/"), ".git/")
		github.mu.Lock()
		answer := github.answers[repo]
		github.mu.Unlock()
		if answer.arrived != nil {
			close(answer.arrived)
		}
		if answer.gate != nil {
			<-answer.gate
			github.mu.Lock()
			answer = github.answers[repo]
			github.mu.Unlock()
		}
		switch {
		case answer.redirect != "":
			http.Redirect(w, r, github.URL+"/acme/"+answer.redirect+".git/info/refs?"+r.URL.RawQuery,
				http.StatusMovedPermanently)
		case answer.status != 0:
			http.Error(w, "scripted", answer.status)
		default:
			if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
				return
			}
		}
	}))
	t.Cleanup(github.Close)

	return github
}

func (g *scriptedGitHub) answer(repo string, answer scriptedAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answers[repo] = answer
}

// advertise asks the node for repo's advertisement as the job would.
func advertise(t *testing.T, node *httptest.Server, token, repo string) {
	t.Helper()

	if err := advertiseErr(t.Context(), node, token, repo); err != nil {
		t.Fatal(err)
	}
}

// advertiseErr is advertise for a goroutine other than the test's.
func advertiseErr(ctx context.Context, node *httptest.Server, token, repo string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		node.URL+"/v1/git/github.com/acme/"+repo+".git/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return err
	}
	req.SetBasicAuth(token, githubBasic)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, resp.Body)

	return errors.Join(err, resp.Body.Close())
}

func scriptedNode(t *testing.T) (*CacheService, *scriptedGitHub, *httptest.Server, string) {
	t.Helper()

	github := newScriptedGitHub(t)
	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.api, service.git.fullFraction = github.URL, github.URL+"/api", 1
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	return service, github, node, token
}

// A FOLLOWED NAME'S REFUSAL WITHDRAWS WHAT WAS SAID ABOUT IT BEFORE: once b
// redirected to c, an advertisement of a that GitHub sends on to b, which b
// now refuses, must leave b's fetch no rename to follow into c's mirror.
func TestARefusalReachedThroughARedirectWithdrawsTheFollowedNamesRename(t *testing.T) {
	t.Parallel()

	service, github, node, token := scriptedNode(t)
	github.answer("b", scriptedAnswer{redirect: "c"})
	advertise(t, node, token, "b")
	b := gitRequest{session: service.byToken[token], owner: "acme", repo: "b", auth: gitAuth(githubBasic)}
	if moved := service.git.follow(b, service.now()); moved.repo != "c" {
		t.Fatalf("the first redirect was not learned: b goes to %q", moved.repo)
	}

	github.answer("a", scriptedAnswer{redirect: "b"})
	github.answer("b", scriptedAnswer{status: http.StatusNotFound})
	advertise(t, node, token, "a")
	if moved := service.git.follow(b, service.now()); moved.repo != "b" {
		t.Fatalf("after GitHub refused b, b's fetch still follows the rename to %q", moved.repo)
	}
}

// A FOLLOWED NAME'S ANSWER IS ORDERED WHEN IT IS ASKED: a redirect asked about
// before a grant of b, whose follow-up b refuses after that grant, ends it.
func TestAFollowUpRefusalOutranksAnEarlierGrant(t *testing.T) {
	t.Parallel()

	service, github, node, token := scriptedNode(t)
	arrived, gate := make(chan struct{}), make(chan struct{})
	github.answer("a", scriptedAnswer{redirect: "b", arrived: arrived, gate: gate})
	done := make(chan error, 1)
	// RELEASED AND JOINED HOWEVER THE TEST ENDS, before either server closes: a
	// held answer would otherwise block the upstream's Close forever.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(func() {
		release()
		if err := <-done; err != nil {
			t.Errorf("the advertisement of a: %v", err)
		}
	})
	go func() { done <- advertiseErr(t.Context(), node, token, "a") }()
	<-arrived
	advertise(t, node, token, "b")
	b := gitRequest{session: service.byToken[token], owner: "acme", repo: "b", auth: gitAuth(githubBasic)}
	service.git.mu.Lock()
	granted := service.git.authorised[service.git.authorisationKey(b)].until
	service.git.mu.Unlock()
	if !service.now().Before(granted) {
		t.Fatal("the direct advertisement of b was not granted")
	}

	github.answer("b", scriptedAnswer{status: http.StatusNotFound})
	release()
	if err := <-done; err != nil {
		t.Fatalf("the advertisement of a: %v", err)
	}
	done <- nil
	service.git.mu.Lock()
	held := service.git.authorised[service.git.authorisationKey(b)]
	service.git.mu.Unlock()
	if service.now().Before(held.until) {
		t.Fatal("b's grant survived GitHub's later refusal of b")
	}
}
