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

// gitUpstream is github.com: git's own smart-HTTP server over one private
// repository, refusing any request without githubBasic, counting fetches.
type gitUpstream struct {
	*httptest.Server
	repo    string
	fetches atomic.Int64
	// refuse makes it answer every request as it would a header it no longer
	// accepts.
	refuse atomic.Bool
}

func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}

	return strings.TrimSpace(string(output))
}

func newGitUpstream(t *testing.T) *gitUpstream {
	t.Helper()

	execPath, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
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

	upstream := &gitUpstream{repo: repo}
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
			if bytes.Contains(plain, []byte("command=fetch")) || bytes.Contains(plain, []byte("want ")) {
				upstream.fetches.Add(1)
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
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+home, "GIT_TERMINAL_PROMPT=0")
		output, err := cmd.CombinedOutput()

		return string(output), err
	}
}

func gitNode(t *testing.T, upstream *gitUpstream) (*CacheService, *httptest.Server, string) {
	t.Helper()

	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream = upstream.URL
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

// AN OBJECT ONLY A DELETED BRANCH REACHED IS NOT SERVED, though the mirror still
// holds it: the mirror is pruned to github.com's refs and served under v0.
func TestAnObjectOnlyADeletedBranchReachedIsNotServed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	work := t.TempDir()
	runIn(t, work, "clone", "-q", upstream.repo, "w")
	runIn(t, filepath.Join(work, "w"), "checkout", "-q", "-b", "secret")
	if err := os.WriteFile(filepath.Join(work, "w", "credential"), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIn(t, filepath.Join(work, "w"), "add", "credential")
	runIn(t, filepath.Join(work, "w"), "commit", "-q", "-m", "secret")
	runIn(t, filepath.Join(work, "w"), "push", "-q", "origin", "secret")
	secret := runIn(t, filepath.Join(work, "w"), "rev-parse", "HEAD")
	// THE COMMIT, ITS TREE AND ITS FILE: a reachability check that walks
	// commits alone passes a want for a tree or a blob.
	secretObjects := map[string]string{
		"commit": secret,
		"tree":   runIn(t, filepath.Join(work, "w"), "rev-parse", "HEAD^{tree}"),
		"blob":   runIn(t, filepath.Join(work, "w"), "rev-parse", "HEAD:credential"),
	}

	git := gitClient(t, node, token, githubBasic)
	// THE MIRROR TAKES EVERY REF, the secret branch's object included; the
	// client takes main alone, so it has to ask for the object to have it.
	if output, err := git("clone", "-q", "--single-branch", "--branch", "main",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if _, err := git("-C", "c", "cat-file", "-e", secret); err == nil {
		t.Fatal("the client already holds the object, so asking for it proves nothing")
	}
	// WHILE ITS BRANCH EXISTS the same request is served, so the refusal below
	// is the deletion's doing and not a fetch by object that never works.
	if output, err := git("clone", "-q", "--single-branch", "--branch", "main",
		"https://github.com/acme/api.git", "control"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if output, err := git("-C", "control", "fetch", "-q", "origin", secret); err != nil {
		t.Fatalf("while its branch exists the object is not served: %v\n%s", err, output)
	}
	runIn(t, upstream.repo, "branch", "-q", "-D", "secret")
	// SENT RAW, because a client of its own accord will not ask for an object
	// the advertisement lacks, and a hostile one will.
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	// THE MIRROR ANSWERS NONE OF THEM. The commit it refuses itself, as
	// upload-pack judges a commit; a tree or a blob it sends to GitHub, whose
	// answer is GitHub's own business (the stand-in here is git's http-backend,
	// which serves one as upload-pack does).
	if body := negotiate(t, node, token, githubBasic, secretObjects["commit"]); bytes.Contains(body, []byte("PACK")) {
		t.Errorf("a commit only a deleted branch reached was served: %q", body[:min(len(body), 80)])
	}
	for _, kind := range []string{"tree", "blob"} {
		before := upstream.fetches.Load()
		negotiate(t, node, token, githubBasic, secretObjects[kind])
		if upstream.fetches.Load() == before {
			t.Errorf("a %s only a deleted branch reached was answered from the mirror", kind)
		}
	}
	if body := negotiate(t, node, token, githubBasic, runIn(t, upstream.repo, "rev-parse", "main")); !bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("an advertised object was not served by the same negotiation: %q", body)
	}

	// A MIRROR THAT COULD NOT BE REFRESHED IS NOT SERVED: its refs may be ones
	// GitHub no longer has. The fetch goes to GitHub, which refuses it.
	runIn(t, filepath.Join(work, "w"), "push", "-q", "origin", secret+":refs/heads/secret")
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	runIn(t, upstream.repo, "branch", "-q", "-D", "secret")
	service.git.fullFraction = 0
	fetches := upstream.fetches.Load()
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	if body := negotiate(t, node, token, githubBasic, secret); bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("a stale mirror served an object GitHub no longer advertises: %q", body[:min(len(body), 80)])
	}
	if upstream.fetches.Load() == fetches {
		t.Fatal("the fetch after a failed refresh was not GitHub's to answer")
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
	service.git.upstream, service.git.fullFraction = upstream.URL, 1
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)
	git := gitClient(t, node, token, githubBasic)

	for _, dir := range []string{"first", "second"} {
		if output, err := git("clone", "-q", "https://github.com/acme/api.git", dir); err != nil {
			t.Fatalf("clone %s: %v\n%s", dir, err, output)
		}
	}
	if _, err := os.Stat(service.git.mirrorPath("untrusted/acme/api")); err == nil {
		t.Fatal("a mirror above the tier's ceiling was kept")
	}
	// The first clone's mirror fetch and the client's own, then the second
	// client's own: no second mirror fetch.
	if got := upstream.fetches.Load(); got != 3 {
		t.Fatalf("github.com served %d fetches, want 3", got)
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
	g.remember(request, now, later, false, false)
	g.remember(request, now, earlier, true, true)
	if g.servesFromMirror(request, now) {
		t.Fatal("a grant asked for before a refusal overtook it")
	}
	g.remember(request, now, g.ask(), true, true)
	if !g.servesFromMirror(request, now) {
		t.Fatal("a grant asked for after the refusal did not stand")
	}
}

// A FETCH IS STOPPED WHILE IT RUNS once its mirror passes the tier's ceiling,
// not only measured after it finishes: a slow upstream here holds the fetch
// open far longer than the watchdog takes.
func TestAFetchIsStoppedWhileItRunsAtTheCeiling(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		http.Error(w, "slow", http.StatusServiceUnavailable)
	}))
	t.Cleanup(slow.Close)
	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.fullFraction = slow.URL, 1
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api"}

	start := time.Now()
	err := service.fetchMirror(t.Context(), service.git.mirrorPath(request.key()), request, 1)
	if !errors.Is(err, errMirrorTooLarge) {
		t.Fatalf("fetchMirror = %v, want errMirrorTooLarge", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the fetch ran %s past its ceiling", took)
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

	start := time.Now()
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("the reaper waited %s on a mirror in use", waited)
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

// A SHALLOW NEGOTIATION IS GITHUB'S TO ANSWER: a client's `shallow` line makes
// upload-pack send that commit's parents without judging them.
func TestAShallowNegotiationGoesToGitHub(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	_, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	main := runIn(t, upstream.repo, "rev-parse", "main")
	shallow := "shallow " + main + "\n"
	want := "want " + main + "\n"
	deepen := "deepen 2147483647\n"
	negotiation := fmt.Sprintf("%04x%s%04x%s%04x%s00000009done\n", len(want)+4, want,
		len(shallow)+4, shallow, len(deepen)+4, deepen)
	before := upstream.fetches.Load()
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
	resp.Body.Close()
	if upstream.fetches.Load() == before {
		t.Fatal("a shallow negotiation was answered from the mirror")
	}
	if wants, shallow := negotiatedWants([]byte(negotiation)); !shallow || len(wants) != 1 {
		t.Fatalf("negotiatedWants = %v, %v", wants, shallow)
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
	g.remember(a, now, refusal, false, false)
	g.remember(b, now.Add(gitAuthorisationAge), unrelated, true, true)
	g.remember(a, now.Add(gitAuthorisationAge), older, true, true)
	if g.servesFromMirror(a, now.Add(gitAuthorisationAge)) {
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
	service.git.upstream, service.git.fullFraction = upstream.URL, 1
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
	for range gitFetches {
		service.git.fetches <- struct{}{}
	}
	start := time.Now()
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone while every slot is busy: %v\n%s", err, output)
	}
	if took := time.Since(start); took > 3*gitFetchWait+5*time.Second {
		t.Fatalf("the clone waited %s behind busy fetch slots", took)
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
	service.git.upstream, service.git.fullFraction = github.URL, 1
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
