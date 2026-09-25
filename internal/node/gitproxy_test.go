package node

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		if r.Header.Get("Authorization") != githubBasic {
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
					plain, _ = io.ReadAll(reader)
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
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] || exit 0\n"+
		"echo username="+token+"\necho password="+header+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[url \"" + node.URL + "/v1/git/github.com/\"]\n\tinsteadOf = https://github.com/\n" +
		"[credential \"" + node.URL + "\"]\n\thelper = " + helper + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o600); err != nil {
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
	if got, _ := git("-C", "second", "rev-parse", "HEAD"); strings.TrimSpace(got) != want {
		t.Fatalf("the second clone is at %q, want %q", got, want)
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
	runIn(t, filepath.Join(work, "w"), "commit", "-q", "--allow-empty", "-m", "secret")
	runIn(t, filepath.Join(work, "w"), "push", "-q", "origin", "secret")
	secret := runIn(t, filepath.Join(work, "w"), "rev-parse", "HEAD")

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
	if body := negotiate(t, node, token, githubBasic, secret); bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("an object only a deleted branch reached was served: %q", body[:min(len(body), 80)])
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
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >>"+record+"\nexec git \"$@\"\n"), 0o700); err != nil {
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
	if entries, _ := os.ReadDir(service.git.root); len(entries) != 0 {
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
