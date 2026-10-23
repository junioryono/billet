package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// The Git proxy against the real github.com (#609 step 4). Every claim ADR-018
// makes about GitHub was measured against GitHub once and then tested against
// git's own git-http-backend configured to answer as GitHub does; these run the
// proxy's real code against GitHub itself, through a real git, on a private
// repository the run is free to push to, force-push, open a pull request in
// and delete branches from.
//
// IT CANNOT BE SUBSTITUTED WITH A FAKE: a fake answers what billet's author
// expects, which is the thing being measured.
//
// It is opt-in and needs a private scratch repository and a token that can push
// to it, whose user GitHub authorises for git and for the REST lookup the proxy
// makes. The token is read from the environment, reaches git through the
// environment and the test's own temporary files, never an argument, and is
// redacted from anything a failure prints:
//
//	BILLET_LIVE_GIT_REPOSITORY=owner/name \
//	BILLET_LIVE_GIT_TOKEN=<token> \
//	go test ./internal/node/ -run '^TestLiveGitProxy' -v -count=1
//
// BILLET_LIVE_GIT_LOAD=1 adds the load run, BILLET_LIVE_GIT_LOAD_CHECKOUTS its
// count of checkouts at once (50). Each advertisement costs the token one REST
// call and each refresh another, well inside 5,000 an hour.
//
// IT RAN ON 2026-10-22 against billet-testbed/b1t2-git-proxy, seeded with
// billet's own history (37.5 MiB packed); the findings are in ADR-018.

// liveGit is the live run's repository and the header a checkout would write
// for its token.
type liveGit struct {
	repository, owner, name string
	token, header, url      string
}

// liveGitTarget is the run's repository, checked before anything is pushed:
// a name of two segments, and private under exactly that name, so no redirect
// to a renamed repository and no public one is ever written to.
func liveGitTarget(t *testing.T) liveGit {
	t.Helper()

	repository := os.Getenv("BILLET_LIVE_GIT_REPOSITORY")
	if repository == "" {
		t.Skip("BILLET_LIVE_GIT_REPOSITORY is unset; this test needs a real private repository on " +
			"github.com and is opt-in")
	}
	token := os.Getenv("BILLET_LIVE_GIT_TOKEN")
	if token == "" {
		t.Fatal("BILLET_LIVE_GIT_TOKEN is required once BILLET_LIVE_GIT_REPOSITORY is set")
	}
	// NO GIT TRACING ANYWHERE, OR NO RUN: a trace writes the credentials git
	// sends, or with GIT_TRACE2_ENV_VARS the token itself. Every git the tests
	// start leaves these out and the node's own git builds a closed environment,
	// so this refuses what neither covers: a git added later that inherits. Go's
	// own HTTP/2 debugging prints the REST calls' Authorization header the same
	// way, before anything here could redact it.
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_TRACE") || name == "GIT_CURL_VERBOSE" {
			t.Fatalf("%s is set; the live tests run with no git tracing in their environment", name)
		}
	}
	if strings.Contains(os.Getenv("GODEBUG"), "http2debug") {
		t.Fatal("GODEBUG asks for HTTP/2 debugging, which prints the token's header; the live tests run without it")
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || !gitNameSegment(owner) || !gitNameSegment(name) {
		t.Fatalf("BILLET_LIVE_GIT_REPOSITORY %q is not owner/name", repository)
	}
	live := liveGit{
		repository: repository, owner: owner, name: name, token: token,
		header: "basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
		url:    "https://github.com/" + repository + ".git",
	}
	var answer struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	}
	if err := live.call(t, http.MethodGet, "/repos/"+repository, nil, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.FullName != repository || !answer.Private {
		t.Fatalf("%s is %q, private %v: the run writes only to a private repository under the name it was given",
			repository, answer.FullName, answer.Private)
	}
	// ACTIONS OFF, OR NOTHING IS PUSHED: a repository seeded with a project's
	// history carries its workflows, and every branch and pull request the run
	// makes would queue them on whatever runners the organization has. It did
	// on 2026-10-22, before this check: billet's CI, queued on billet's own
	// labels in the shared test organization, and cancelled.
	var actions struct {
		Enabled *bool `json:"enabled"`
	}
	if err := live.call(t, http.MethodGet, "/repos/"+repository+"/actions/permissions", nil, &actions); err != nil {
		t.Fatal(err)
	}
	if actions.Enabled == nil || *actions.Enabled {
		t.Fatalf("Actions is on for %s, so every push would start its workflows; turn it off first "+
			"(PUT /repos/%s/actions/permissions with enabled false)", repository, repository)
	}

	return live
}

// redact is s with the token and the header taken out.
func (l liveGit) redact(s string) string {
	for _, secret := range []string{l.header, strings.TrimPrefix(l.header, "basic "), l.token} {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}

	return s
}

// env is git's environment straight to GitHub: the header as configuration
// through the environment, never in an argument, and no tracing that would
// print it.
func (l liveGit) env(home string) []string {
	return append(isolatedGitEnviron(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: "+l.header,
		"GIT_AUTHOR_NAME=billet live", "GIT_AUTHOR_EMAIL=live@billet.invalid",
		"GIT_COMMITTER_NAME=billet live", "GIT_COMMITTER_EMAIL=live@billet.invalid")
}

// direct runs git against github.com itself in dir.
func (l liveGit) direct(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = l.env(t.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, l.redact(string(output)))
	}

	return strings.TrimSpace(string(output))
}

// call calls GitHub's REST API with the live token, decoding the answer into
// out. It has a minute of its own, not the test's context: a cleanup that
// removes what the test made runs after that context has ended.
func (l liveGit) call(t *testing.T, method, path string, body, out any) error {
	t.Helper()

	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		in = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New(l.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, l.redact(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}

	return nil
}

// cleanUp closes every open pull request and deletes every branch whose name
// starts with prefix, and fails the test for any it cannot. Registered before
// the first is made, so one GitHub accepted before an error the test saw is
// still removed.
func (l liveGit) cleanUp(t *testing.T, prefix string) {
	t.Helper()

	var pulls []struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := l.call(t, http.MethodGet, "/repos/"+l.repository+"/pulls?state=open&per_page=100", nil, &pulls); err != nil {
		t.Errorf("list the pull requests to close: %v", err)
	}
	for _, pull := range pulls {
		if !strings.HasPrefix(pull.Head.Ref, prefix) {
			continue
		}
		if err := l.call(t, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", l.repository, pull.Number),
			map[string]string{"state": "closed"}, nil); err != nil {
			t.Errorf("close pull request %d: %v", pull.Number, err)
		}
	}
	var refs []struct {
		Ref string `json:"ref"`
	}
	err := l.call(t, http.MethodGet, "/repos/"+l.repository+"/git/matching-refs/heads/"+url.PathEscape(prefix), nil, &refs)
	if err != nil {
		t.Errorf("list the branches to delete: %v", err)
	}
	for _, ref := range refs {
		if err := l.call(t, http.MethodDelete, "/repos/"+l.repository+"/git/"+ref.Ref, nil, nil); err != nil {
			t.Errorf("delete %s: %v", ref.Ref, err)
		}
	}
}

// liveGitNode is the proxy pointed at github.com, as a node runs it.
func liveGitNode(t *testing.T) (*CacheService, *httptest.Server, string) {
	t.Helper()

	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.api, service.git.fullFraction = "https://github.com", "https://api.github.com", 1
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	return service, node, token
}

// liveCounts is the requests an observer was told of, by route, reason and
// status, and how many went to GitHub.
func liveCounts(observer *recordingGitObserver) (map[string]int, int) {
	requests, _, _ := observer.seen()
	counts := map[string]int{}
	github := 0
	for _, request := range requests {
		counts[request]++
		if strings.HasPrefix(request, "github ") {
			github++
		}
	}

	return counts, github
}

// THE PROXY AGAINST GITHUB: a clone, a moved branch, a rebased branch, a
// deleted branch, a superseded pull-request merge commit fetched by id, a
// blobless and a shallow checkout, a daily repack and a prune, and a forgotten
// mirror, each through a real git, each served from the mirror where it should
// be and by GitHub only where it should be.
func TestLiveGitProxyAgainstGitHub(t *testing.T) {
	live := liveGitTarget(t)
	prefix := "b1t2-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { live.cleanUp(t, prefix) })
	service, node, session := liveGitNode(t)
	git := gitClient(t, node, session, live.header)
	key := "untrusted/" + strings.ToLower(live.owner) + "/" + strings.ToLower(live.name)
	work := t.TempDir()
	live.direct(t, work, "clone", "-q", live.url, "w")
	repo := filepath.Join(work, "w")
	main := live.direct(t, repo, "rev-parse", "HEAD")

	push := func(t *testing.T, branch string) string {
		t.Helper()

		live.direct(t, repo, "commit", "-q", "--allow-empty", "-m", prefix+" "+branch)
		live.direct(t, repo, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)

		return live.direct(t, repo, "rev-parse", "HEAD")
	}
	through := func(t *testing.T, args ...string) string {
		t.Helper()

		output, err := git(args...)
		if err != nil {
			t.Fatalf("git %s through the proxy: %v\n%s", strings.Join(args, " "), err, live.redact(output))
		}

		return strings.TrimSpace(output)
	}
	observe := func() *recordingGitObserver {
		observer := &recordingGitObserver{}
		service.SetGitObserver(observer)

		return observer
	}
	// fromMirror is that every request since observe was the mirror's, at least
	// served of them, the credentials challenge aside.
	fromMirror := func(t *testing.T, observer *recordingGitObserver, at int) {
		t.Helper()

		served(t, observer, at)
		counts, github := liveCounts(observer)
		t.Logf("requests %v, refreshes %v", counts, refreshesOf(observer))
		if github > 0 || counts["mirror served 2xx"] < at {
			t.Errorf("not every request was served from the mirror: %v", counts)
		}
	}

	t.Run("a clone is served from a new mirror", func(t *testing.T) {
		observer := observe()
		started := time.Now()
		through(t, "clone", "-q", live.url, "c")
		t.Logf("clone through the proxy, cold: %s", time.Since(started).Round(time.Millisecond))
		if head := through(t, "-C", "c", "rev-parse", "HEAD"); head != main {
			t.Fatalf("the clone is at %s, want %s", head, main)
		}
		fromMirror(t, observer, 2)
		awaitIdleMirror(t, service, key)
	})

	t.Run("a moved branch is refreshed and served", func(t *testing.T) {
		// THE BRANCH IS THE MIRROR'S FIRST, so what is timed is a fast-forward of a
		// ref the mirror holds, not a new one.
		live.direct(t, repo, "checkout", "-q", "-b", prefix+"-moved", main)
		first := push(t, prefix+"-moved")
		through(t, "-C", "c", "fetch", "-q", "origin", prefix+"-moved")
		awaitIdleMirror(t, service, key)
		if refs := mirrorRefsOf(t, service.git.mirrorPath(key)); refs["refs/heads/"+prefix+"-moved"] != first {
			t.Fatalf("the mirror's branch is %q before the move, want %s", refs["refs/heads/"+prefix+"-moved"], first)
		}
		pushed := push(t, prefix+"-moved")
		observer := observe()
		started := time.Now()
		through(t, "-C", "c", "fetch", "-q", "origin", prefix+"-moved")
		t.Logf("fetch of one moved branch: %s", time.Since(started).Round(time.Millisecond))
		if got := through(t, "-C", "c", "rev-parse", "FETCH_HEAD"); got != pushed {
			t.Fatalf("fetched %s, want %s", got, pushed)
		}
		fromMirror(t, observer, 2)
		if !slices.Contains(refreshesOf(observer), gitRefreshOK) {
			t.Error("no refresh brought the moved branch")
		}
		if refs := mirrorRefsOf(t, service.git.mirrorPath(key)); refs["refs/heads/"+prefix+"-moved"] != pushed {
			t.Errorf("the mirror's branch is %q, want %s", refs["refs/heads/"+prefix+"-moved"], pushed)
		}
		awaitIdleMirror(t, service, key)
	})

	t.Run("a rebased branch is refreshed and served", func(t *testing.T) {
		live.direct(t, repo, "checkout", "-q", "-B", prefix+"-moved", main+"~3")
		rebased := push(t, prefix+"-moved")
		observer := observe()
		through(t, "-C", "c", "fetch", "-q", "origin", "+"+prefix+"-moved")
		if got := through(t, "-C", "c", "rev-parse", "FETCH_HEAD"); got != rebased {
			t.Fatalf("fetched %s, want %s", got, rebased)
		}
		fromMirror(t, observer, 2)
		awaitIdleMirror(t, service, key)
	})

	t.Run("a deleted branch leaves the mirror", func(t *testing.T) {
		live.direct(t, repo, "push", "-q", "origin", ":refs/heads/"+prefix+"-moved")
		observer := observe()
		if listed := through(t, "ls-remote", live.url, prefix+"-moved"); listed != "" {
			t.Fatalf("the advertisement still lists the deleted branch: %q", listed)
		}
		fromMirror(t, observer, 1)
		if _, ok := mirrorRefsOf(t, service.git.mirrorPath(key))["refs/heads/"+prefix+"-moved"]; ok {
			t.Error("the mirror still holds the deleted branch")
		}
		awaitIdleMirror(t, service, key)
	})

	var superseded string
	t.Run("a superseded merge commit is fetched by id from the mirror", func(t *testing.T) {
		live.direct(t, repo, "checkout", "-q", "-B", prefix+"-pr", main)
		push(t, prefix+"-pr")
		var pr struct {
			Number int `json:"number"`
		}
		if err := live.call(t, http.MethodPost, "/repos/"+live.repository+"/pulls", map[string]string{
			"title": prefix, "head": prefix + "-pr", "base": "main",
			"body": "Opened by billet's live Git proxy test; closed when it ends.",
		}, &pr); err != nil {
			t.Fatal(err)
		}
		mergeRef := fmt.Sprintf("refs/pull/%d/merge", pr.Number)
		// GITHUB COMPUTES THE MERGE COMMIT WHEN ASKED, so the pull request is
		// asked until its merge ref names one other than not.
		mergeOf := func(t *testing.T, not string) string {
			t.Helper()

			deadline := time.Now().Add(3 * time.Minute)
			var answered, listed string
			for time.Now().Before(deadline) {
				var got struct {
					MergeCommitSHA string `json:"merge_commit_sha"`
				}
				if err := live.call(t, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", live.repository, pr.Number),
					nil, &got); err != nil {
					t.Fatal(err)
				}
				answered, listed = got.MergeCommitSHA, live.direct(t, repo, "ls-remote", "origin", mergeRef)
				if answered != "" && answered != not && strings.HasPrefix(listed, answered) {
					return answered
				}
				time.Sleep(2 * time.Second)
			}
			t.Fatalf("GitHub named no merge commit other than %q for %s in three minutes: the pull request "+
				"last said %q and the ref was %q", not, mergeRef, answered, listed)

			return ""
		}
		first := mergeOf(t, "")
		through(t, "-C", "c", "fetch", "-q", "origin", mergeRef)
		awaitIdleMirror(t, service, key)
		push(t, prefix+"-pr")
		second := mergeOf(t, first)
		t.Logf("merge commit %s superseded by %s", first, second)
		through(t, "ls-remote", live.url)
		awaitIdleMirror(t, service, key)

		observer := observe()
		through(t, "init", "-q", "byid")
		through(t, "-C", "byid", "fetch", "-q", live.url, first)
		fromMirror(t, observer, 2)
		superseded = first
	})

	t.Run("blobless and shallow checkouts are served", func(t *testing.T) {
		observer := observe()
		through(t, "clone", "-q", "--filter=blob:none", live.url, "blobless")
		// THE FILTER WAS HONOURED: an older commit's blobs are missing until a
		// checkout asks for them.
		missing := 0
		for line := range strings.Lines(through(t, "-C", "blobless", "rev-list", "--objects", "--missing=print",
			main+"~20")) {
			if strings.HasPrefix(line, "?") {
				missing++
			}
		}
		if missing == 0 {
			t.Fatal("the blobless clone holds every blob, so the filter was not honoured")
		}
		fromMirror(t, observer, 2)
		lazy := observe()
		through(t, "-C", "blobless", "checkout", "-q", main+"~20")
		fromMirror(t, lazy, 1)

		shallow := observe()
		through(t, "clone", "-q", "--depth=1", live.url, "shallow")
		if got := through(t, "-C", "shallow", "rev-parse", "--is-shallow-repository"); got != "true" {
			t.Fatalf("the shallow clone is not shallow: %q", got)
		}
		through(t, "-C", "shallow", "fetch", "-q", "--deepen=5")
		fromMirror(t, shallow, 4)
	})

	t.Run("a daily repack keeps and a prune removes, and GitHub answers what was pruned", func(t *testing.T) {
		if superseded == "" {
			t.Skip("no superseded merge commit to follow")
		}
		request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
			owner: live.owner, repo: live.name, auth: gitAuth(live.header)}
		var repository struct {
			ID int64 `json:"id"`
		}
		if err := live.call(t, http.MethodGet, "/repos/"+live.repository, nil, &repository); err != nil {
			t.Fatal(err)
		}
		path := service.git.mirrorPath(key)
		maintained := observe()
		started := time.Now()
		service.maintain(t.Context(), request, repository.ID, service.git.mirror(key), gitMaintainFull)
		t.Logf("daily repack of the mirror (%d packs after): %s", len(packFiles(t, path, "*.pack")),
			time.Since(started).Round(time.Millisecond))
		if got := maintained.maintained(); !slices.Equal(got, []string{"full ok"}) {
			t.Fatalf("the daily repack was counted %v", got)
		}
		if !hasObject(t, path, superseded) {
			t.Fatal("the daily repack removed the superseded merge commit")
		}
		later := time.Now().Add(gitPruneAfter + 24*time.Hour)
		service.now = func() time.Time { return later }
		service.maintain(t.Context(), request, repository.ID, service.git.mirror(key), gitMaintainPrune)
		service.now = time.Now
		if got := maintained.maintained(); !slices.Equal(got, []string{"full ok", "prune ok"}) {
			t.Fatalf("the prune was counted %v", got)
		}
		if hasObject(t, path, superseded) {
			t.Fatal("a prune past the window kept the superseded merge commit")
		}
		if !hasObject(t, path, main) {
			t.Fatal("the prune removed main's commit")
		}
		observer := observe()
		through(t, "init", "-q", "pruned")
		through(t, "-C", "pruned", "fetch", "-q", live.url, superseded)
		served(t, observer, 3)
		counts, _ := liveCounts(observer)
		t.Logf("requests %v", counts)
		if counts["github mirror_lacks_object 2xx"] != 1 {
			t.Errorf("a fetch of a pruned commit was not sent to GitHub once: %v", counts)
		}
	})

	t.Run("a forgotten mirror is fetched anew", func(t *testing.T) {
		if _, err := ForgetGitMirrors(filepath.Dir(service.git.root), live.owner, live.name); err != nil {
			t.Fatal(err)
		}
		observer := observe()
		through(t, "-C", "c", "fetch", "-q", "origin")
		fromMirror(t, observer, 2)
		if !slices.Contains(refreshesOf(observer), gitRefreshOK) {
			t.Error("no new mirror was fetched in the forgotten one's place")
		}
	})
}

// refreshesOf is the refreshes the observer was told of.
func refreshesOf(observer *recordingGitObserver) []string {
	_, refreshes, _ := observer.seen()

	return refreshes
}

// served awaits n counted requests.
func served(t *testing.T, observer *recordingGitObserver, n int) {
	t.Helper()

	gitWithin(t, "the counted requests", func() struct{} {
		for t.Context().Err() == nil {
			if requests, _, _ := observer.seen(); len(requests) >= n {
				return struct{}{}
			}
			time.Sleep(20 * time.Millisecond)
		}

		return struct{}{}
	})
}

// THE LOAD RUN: many checkouts at once through the proxy, each under a session
// of its own as many jobs would be, against as many straight to GitHub; the
// advertisement alone, shallow as actions/checkout fetches by default, and
// whole; and advertisements just after a push, while a refresh runs. What it
// decides is in #609: whether pack-objects is the bottleneck, and whether
// advertisements wait on refreshes.
func TestLiveGitProxyLoad(t *testing.T) {
	live := liveGitTarget(t)
	if os.Getenv("BILLET_LIVE_GIT_LOAD") != "1" {
		t.Skip("BILLET_LIVE_GIT_LOAD is not 1; the load run is opt-in on top of the live tests")
	}
	checkouts := 50
	if raw := os.Getenv("BILLET_LIVE_GIT_LOAD_CHECKOUTS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 2 {
			t.Fatalf("BILLET_LIVE_GIT_LOAD_CHECKOUTS %q is not a count of two or more", raw)
		}
		checkouts = n
	}
	prefix := "b1t2-load-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { live.cleanUp(t, prefix) })
	service, node, first := liveGitNode(t)
	sessions := []string{first}
	for i := 1; i < checkouts; i++ {
		credentials, err := service.PrepareScoped(provider.InstanceName(fmt.Sprintf("load-%d", i)), CacheSessionScope{
			Trust: provider.TrustUntrusted, LeaseID: fmt.Sprintf("load-%d", i), Epoch: 1, Cache: gitCache(),
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, credentials.Token)
	}
	// THE MIRROR IS MADE FIRST, as a node's first job of the day makes it.
	if output, err := gitClient(t, node, first, live.header)("clone", "-q", "--depth=1", live.url, "warm"); err != nil {
		t.Fatalf("warm the mirror: %v\n%s", err, live.redact(output))
	}
	key := "untrusted/" + strings.ToLower(live.owner) + "/" + strings.ToLower(live.name)
	awaitIdleMirror(t, service, key)
	work := t.TempDir()
	live.direct(t, work, "clone", "-q", "--depth=1", live.url, "w")
	repo := filepath.Join(work, "w")
	main := live.direct(t, repo, "rev-parse", "HEAD")

	// cpu is the CPU every child of this process has used and been waited for:
	// the clients' git on both sides, and through the proxy the node's
	// upload-packs and their pack-objects too. The proxy's own Go is not in it,
	// and the clients differ between the sides (loopback and HTTPS), so the
	// difference is the node's git and that, not the node's git alone.
	cpu := func() time.Duration {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &usage); err != nil {
			t.Fatal(err)
		}

		return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	}
	// run is one phase: every checkout at once, through the proxy or straight to
	// GitHub, each answer holding want; it returns, through
	// the proxy, the requests and the refreshes the node reported.
	run := func(name string, args []string, through bool, want string) (map[string]int, []string) {
		t.Helper()

		observer := &recordingGitObserver{}
		service.SetGitObserver(observer)
		// EVERY CLIENT IS MADE BEFORE THE CLOCK STARTS, its home, credential helper
		// and gitconfig included, so both sides time git alone.
		commands := make([]func() (string, error), checkouts)
		clones := make([]string, checkouts)
		for i := range checkouts {
			dir := t.TempDir()
			command := append(slices.Clone(args), live.url)
			if args[0] == "clone" {
				clones[i] = filepath.Join(dir, "c")
				command = append(command, clones[i])
			}
			if through {
				client := gitClient(t, node, sessions[i], live.header)
				commands[i] = func() (string, error) { return client(command...) }
			} else {
				env := live.env(dir)
				commands[i] = func() (string, error) {
					cmd := exec.CommandContext(t.Context(), "git", command...)
					cmd.Dir = dir
					cmd.Env = env
					output, err := cmd.CombinedOutput()

					return string(output), err
				}
			}
		}
		var wg sync.WaitGroup
		took := make([]time.Duration, checkouts)
		failed := make([]error, checkouts)
		used := cpu()
		started := time.Now()
		for i := range checkouts {
			wg.Go(func() {
				begin := time.Now()
				output, err := commands[i]()
				took[i] = time.Since(begin)
				switch {
				case err != nil:
					failed[i] = fmt.Errorf("%w: %s", err, live.redact(output))
				case !strings.Contains(output, want):
					failed[i] = fmt.Errorf("the answer lacks %q: %s", want, live.redact(output))
				}
			})
		}
		wg.Wait()
		wall := time.Since(started)
		slices.Sort(took)
		failures := 0
		for _, err := range failed {
			if err != nil {
				failures++
				t.Logf("%s: one failed: %v", name, err)
			}
		}
		t.Logf("%s: %d at once, wall %s, median %s, p90 %s, slowest %s, child CPU %s, %d failed", name, checkouts,
			wall.Round(time.Millisecond), took[checkouts/2].Round(time.Millisecond),
			took[checkouts*9/10].Round(time.Millisecond), took[checkouts-1].Round(time.Millisecond),
			(cpu() - used).Round(time.Millisecond), failures)
		if failures > 0 {
			t.Errorf("%s: %d of %d failed", name, failures, checkouts)
		}
		// EVERY CLONE IS WHAT IT CLAIMS, checked once the clock has stopped: main's
		// commit, and shallow exactly when it was asked to be.
		for i, clone := range clones {
			if clone == "" || failed[i] != nil {
				continue
			}
			if head := runIn(t, clone, "rev-parse", "HEAD"); head != main {
				t.Errorf("%s: a clone is at %s, want %s", name, head, main)
			}
			shallow := runIn(t, clone, "rev-parse", "--is-shallow-repository") == "true"
			if want := slices.Contains(args, "--depth=1"); shallow != want {
				t.Errorf("%s: a clone is shallow %v, want %v", name, shallow, want)
			}
		}
		counts, _ := liveCounts(observer)
		refreshes := refreshesOf(observer)
		if through {
			t.Logf("%s: requests %v, refreshes %v", name, counts, refreshes)
		}

		return counts, refreshes
	}
	// fromMirrorAlone holds a phase through the proxy to what it claims: at least
	// each served requests a checkout from the mirror (an advertisement, and a
	// clone's fetch), nothing sent to GitHub, and a refresh only where the phase
	// moved GitHub's refs.
	fromMirrorAlone := func(name string, counts map[string]int, refreshes []string, each int, refreshed bool) {
		t.Helper()

		for request := range counts {
			if strings.HasPrefix(request, "github ") {
				t.Errorf("%s: a request went to GitHub: %v", name, counts)
			}
		}
		if served := counts["mirror served 2xx"]; served < each*checkouts {
			t.Errorf("%s: %d requests served from the mirror for %d checkouts: %v", name, served, checkouts, counts)
		}
		switch {
		case refreshed && !slices.Contains(refreshes, gitRefreshOK):
			t.Errorf("%s: no refresh brought the push: %v", name, refreshes)
		case !refreshed && len(refreshes) > 0:
			t.Errorf("%s: refreshed with GitHub's refs unchanged: %v", name, refreshes)
		}
	}

	listed := main + "\trefs/heads/main"
	counts, refreshes := run("advertisement, through the proxy", []string{"ls-remote", "-q"}, true, listed)
	fromMirrorAlone("advertisement", counts, refreshes, 1, false)
	run("advertisement, straight to GitHub", []string{"ls-remote", "-q"}, false, listed)
	counts, refreshes = run("shallow, through the proxy", []string{"clone", "-q", "--depth=1"}, true, "")
	fromMirrorAlone("shallow", counts, refreshes, 2, false)
	run("shallow, straight to GitHub", []string{"clone", "-q", "--depth=1"}, false, "")
	counts, refreshes = run("whole, through the proxy", []string{"clone", "-q"}, true, "")
	fromMirrorAlone("whole", counts, refreshes, 2, false)
	run("whole, straight to GitHub", []string{"clone", "-q"}, false, "")

	// JUST AFTER A PUSH every advertisement finds the mirror's refs behind
	// GitHub's: one starts the refresh and the rest wait for it, which is what
	// protocol v2 to the guest would spare them. The time is measured; that every
	// answer names the pushed commit, from the mirror, is asserted.
	for round := 1; round <= 3; round++ {
		live.direct(t, repo, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("%s %d", prefix, round))
		live.direct(t, repo, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+prefix)
		pushed := live.direct(t, repo, "rev-parse", "HEAD") + "\trefs/heads/" + prefix
		name := fmt.Sprintf("advertisement after a push, through the proxy, round %d", round)
		counts, refreshes := run(name, []string{"ls-remote", "-q"}, true, pushed)
		fromMirrorAlone(name, counts, refreshes, 1, true)
		awaitIdleMirror(t, service, key)
	}
}
