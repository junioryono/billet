package node

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// mirrorHas reports whether the mirror at path holds object, asked of git
// directly rather than through the proxy.
func mirrorHas(t *testing.T, path, object string) bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", object)
	cmd.Dir = path
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+path)

	return cmd.Run() == nil
}

// A REPOSITORY GIVEN A NAME ANOTHER ONE HAD IS NEVER SERVED THE FIRST ONE'S
// OBJECTS: the mirror is stamped with the id GitHub gave the repository it was
// fetched from, the advertisement replaces a mirror of another id, and a fetch
// is served only from a mirror of the id its grant names.
func TestARecreatedRepositorysObjectsAreNeverServed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	// THE CLAIM BESIDE A MIRROR IN USE gives up within this, below.
	service.git.refreshWait = 2 * time.Second
	secret := upstream.commitTo(t, "secret", map[string]string{"credential": "hunter2\n"})
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	path := service.git.mirrorPath("untrusted/acme/api")
	first := upstream.repositoryID("api")
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != first {
		t.Fatalf("the mirror is stamped %d (%v), want the repository's id %d", stamped, err, first)
	}
	if !mirrorHas(t, path, secret) {
		t.Fatal("the mirror never held the first repository's commit, so its absence below proves nothing")
	}

	// THE NAME NOW BELONGS TO ANOTHER REPOSITORY, whose readers may not be the
	// first one's.
	recreated := upstream.recreate(t)
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	second := upstream.repositoryID("api")
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != second {
		t.Fatalf("the mirror is stamped %d (%v) after the name changed hands, want %d", stamped, err, second)
	}
	if mirrorHas(t, path, secret) {
		t.Fatal("the mirror of the second repository still holds the first one's commit")
	}
	if body := negotiate(t, node, token, githubBasic, secret); bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("the first repository's commit was served to the second's job: %q", body[:min(len(body), 80)])
	}

	// A MIRROR THE CLAIM COULD NOT REPLACE, one in use when the name changed
	// hands, is still never served: the fetch is GitHub's.
	if err := os.WriteFile(filepath.Join(path, gitIdentityStamp), []byte(strconv.FormatInt(first, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirror := service.git.mirror("untrusted/acme/api")
	mirror.use.RLock()
	output, err := git("ls-remote", "https://github.com/acme/api.git")
	mirror.use.RUnlock()
	if err != nil {
		t.Fatalf("ls-remote beside a mirror in use: %v\n%s", err, output)
	}
	before := upstream.guestFetches()
	if body := negotiate(t, node, token, githubBasic, recreated); !bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("github.com did not answer the fetch the mirror may not: %q", body[:min(len(body), 80)])
	}
	if upstream.guestFetches() == before {
		t.Fatal("a mirror stamped with another repository's id served the fetch")
	}
}

// A NAME GITHUB'S API WILL NOT TIE TO A REPOSITORY IS GITHUB'S TO ANSWER, and no
// mirror is made for it; once the API answers, the next job is mirrored.
func TestARepositoryGitHubCannotNameIsGitHubsToAnswer(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	upstream.apiDown.Store(true)
	git := gitClient(t, node, token, githubBasic)
	before := upstream.guestFetches()
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("a clone while the API cannot say which repository it is: %v\n%s", err, output)
	}
	if upstream.apiCalls.Load() == 0 {
		t.Fatal("the proxy never asked the API, so this proves nothing about what it does without an answer")
	}
	if upstream.guestFetches() == before {
		t.Fatal("the clone was answered from a mirror though GitHub never said which repository the name is")
	}
	path := service.git.mirrorPath("untrusted/acme/api")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a mirror was made for a repository GitHub never named")
	}

	upstream.apiDown.Store(false)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "d"); err != nil {
		t.Fatalf("the clone once the API answers: %v\n%s", err, output)
	}
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != upstream.repositoryID("api") {
		t.Fatalf("the mirror is stamped %d (%v) once the API answered", stamped, err)
	}
}

// A MIRROR WHOSE SERVE FAILS AFTER ITS ANSWER BEGAN IS REPLACED: that one fetch
// fails, as nothing can be taken back once sent, and the next advertisement
// removes the mirror so the jobs after it are answered.
func TestAMirrorThatFailsMidAnswerIsReplaced(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	var log gitLog
	service.log = slog.New(slog.NewTextHandler(&log, nil))
	mode := gitWrapper(t, service)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if err := os.WriteFile(mode, []byte("nakfail"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream.commitTo(t, "main", map[string]string{"f": "1\n"})
	if output, err := git("-C", "c", "pull", "-q", "--ff-only"); err == nil {
		t.Fatalf("a pull whose serve failed mid-answer succeeded, so the failure was never staged:\n%s", output)
	}
	path := service.git.mirrorPath("untrusted/acme/api")
	if _, _, err := mirrorRepository(path); !errors.Is(err, errMirrorBroken) {
		t.Fatalf("the mirror whose serve failed mid-answer reads %v, want it marked broken on disk", err)
	}

	if err := os.WriteFile(mode, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	pushed := upstream.commitTo(t, "main", map[string]string{"g": "1\n"})
	if output, err := git("-C", "c", "pull", "-q", "--ff-only"); err != nil {
		t.Fatalf("the pull after a broken mirror: %v\n%s", err, output)
	}
	if output, err := git("-C", "c", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(output) != pushed {
		t.Fatalf("the pull is at %q (%v), want %s", output, err, pushed)
	}
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != upstream.repositoryID("api") {
		t.Fatalf("the mirror after the broken one reads %d, %v; want a verified mirror of the repository", stamped, err)
	}
	if !strings.Contains(log.String(), "removed a git mirror it could not serve") {
		t.Fatalf("the broken mirror's removal was not logged:\n%s", log.String())
	}
}

// A GUEST THAT STOPS SENDING ITS BODY IS ENDED AT THE IDLE LIMIT, though a read
// blocked on the connection is not woken by a context: the connection's own
// deadline is moved, and the request's slots are given back.
func TestAStalledGuestEndsAtTheIdleLimit(t *testing.T) {
	t.Parallel()

	const idle = 300 * time.Millisecond
	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.fullFraction, service.git.idleLimit = 1, idle
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	// HALF A NEGOTIATION, then nothing until the test ends. The client is not
	// what is watched: Go's client waits for its request body to finish before
	// it returns, so the node's slot is.
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		node.URL+"/v1/git/github.com/acme/api.git/git-upload-pack", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(token, githubBasic)
	var client sync.WaitGroup
	t.Cleanup(func() {
		writer.Close()
		client.Wait()
	})
	client.Go(func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	if _, err := io.WriteString(writer, "0032want "+strings.Repeat("a", 40)+"\n"); err != nil {
		t.Fatal(err)
	}
	gitWithin(t, "the stalled request to be admitted", func() struct{} {
		for len(service.git.work) == 0 && t.Context().Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})
	gitWithin(t, "the stalled request's slot to be given back", func() struct{} {
		for len(service.git.work) != 0 && t.Context().Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})
}

// NO REFRESH STARTS ONCE THE PROXY HAS BEGUN TO CLOSE, so none outlives the
// state directory close was called to protect.
func TestNoRefreshStartsOnceTheProxyCloses(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.close()
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)), owner: "acme", repo: "api"}
	refresh := service.startRefresh(t.Context(), request, 7, service.git.mirror(request.key()), "")
	select {
	case <-refresh.done:
	default:
		t.Fatal("a refresh started after the proxy closed")
	}
	if !errors.Is(refresh.err, errGitClosed) {
		t.Fatalf("a refresh after close ended %v, want errGitClosed", refresh.err)
	}
	if _, err := os.Stat(service.git.mirrorPath(request.key())); err == nil {
		t.Fatal("a refresh after close made a mirror")
	}
}

// A MIRROR LEFT UNVERIFIED WHILE A FETCH WAITS FOR ITS FIRST LINE IS NOT
// SERVED: the last check before an answer commits reads the mirror again, as a
// refresh sharing the request's hold can mark it while the request waits.
func TestAMirrorUnverifiedMidRequestIsNotServed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	mode := gitWrapper(t, service)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	pushed := upstream.commitTo(t, "main", map[string]string{"f": "1\n"})
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	if err := os.WriteFile(mode, []byte("slow"), 0o600); err != nil {
		t.Fatal(err)
	}
	// UPLOAD-PACK WAITS FOR THIS, so the mark below lands before it answers
	// however the test is scheduled; the test's end releases it too.
	release := func() error { return os.WriteFile(mode+".release", nil, 0o600) }
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	})
	path := service.git.mirrorPath("untrusted/acme/api")
	before := upstream.guestFetches()
	type pulled struct {
		output string
		err    error
	}
	done := make(chan pulled, 1)
	go func() {
		output, err := git("-C", "c", "pull", "-q", "--ff-only")
		done <- pulled{output, err}
	}()
	gitWithin(t, "the mirror's upload-pack to start", func() struct{} {
		for t.Context().Err() == nil {
			if _, err := os.Stat(mode + ".started"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})
	if err := markUnverified(path); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	result := gitWithin(t, "the pull", func() pulled { return <-done })
	if result.err != nil {
		t.Fatalf("the pull: %v\n%s", result.err, result.output)
	}
	if output, err := git("-C", "c", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(output) != pushed {
		t.Fatalf("the pull is at %q (%v), want %s", output, err, pushed)
	}
	if upstream.guestFetches() == before {
		t.Fatal("a mirror marked unverified while the fetch waited was served")
	}
}

// A BROKEN MIRROR THE DISK WILL NOT MARK IS STILL NEVER SERVED AGAIN: it is held
// out of service in the process, and its stamp is taken away so a restart that
// forgets the hold finds a mirror of no repository; the hold ends with the
// mirror, however it is removed.
func TestABrokenMirrorTheDiskWillNotMarkIsHeldAcrossARestart(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.fullFraction = 1
	path := service.git.mirrorPath("untrusted/acme/api")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{gitIdentityStamp: "7\n", gitUsedStamp: ""} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != 7 {
		t.Fatalf("the mirror reads %d, %v before it broke", stamped, err)
	}

	if err := service.git.holdBroken(path, errors.New("no space left on device")); err == nil ||
		!strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("holdBroken = %v, want the mark's failure reported", err)
	}
	if !service.git.quarantined(path) {
		t.Fatal("the broken mirror is not held out of service in the process")
	}
	if _, exists, err := mirrorRepository(path); !exists || err == nil {
		t.Fatalf("after a restart the broken mirror would read exists=%v, %v; want no repository it can serve", exists, err)
	}

	// THE HOLD ENDS WITH THE MIRROR, here the reaper's removal of one unused
	// for longer than its retention, so the mirror made after it is served.
	now := time.Now()
	service.now = func() time.Time { return now.Add(gitMirrorRetention + time.Hour) }
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the reaper kept a mirror past its retention")
	}
	if service.git.quarantined(path) {
		t.Fatal("the hold outlived the mirror it was on")
	}
}

// A MIRROR MARKED WHILE ITS ADVERTISEMENT IS READ IS NOT ADVERTISED: refs a
// refresh brought meanwhile are never sent from a mirror GitHub has not
// vouched for; GitHub's own advertisement is.
func TestAMirrorMarkedWhileAdvertisedIsNotAdvertised(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	var log gitLog
	service.log = slog.New(slog.NewTextHandler(&log, nil))
	mode := gitWrapper(t, service)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if err := os.WriteFile(mode, []byte("slowadvert"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := func() error { return os.WriteFile(mode+".release", nil, 0o600) }
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	})
	type listed struct {
		output string
		err    error
	}
	done := make(chan listed, 1)
	go func() {
		output, err := git("ls-remote", "https://github.com/acme/api.git")
		done <- listed{output, err}
	}()
	gitWithin(t, "the mirror's advertisement to start", func() struct{} {
		for t.Context().Err() == nil {
			if _, err := os.Stat(mode + ".started"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})
	if err := markUnverified(service.git.mirrorPath("untrusted/acme/api")); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	result := gitWithin(t, "the ls-remote", func() listed { return <-done })
	if result.err != nil {
		t.Fatalf("ls-remote: %v\n%s", result.err, result.output)
	}
	if !strings.Contains(log.String(), "the mirror was marked while it was advertised") {
		t.Fatalf("a mirror marked while it was advertised was advertised:\n%s", log.String())
	}
}

// A FETCH IS VERIFIED ONLY BY GITHUB NAMING THE SAME REPOSITORY AFTER IT: a name
// that passed to another repository while the node fetched it, or an API that
// could not say, leaves the mirror unverified on disk, served to no one, and
// the next advertisement replaces it.
func TestAFetchGitHubCannotVouchForLeavesTheMirrorUnverified(t *testing.T) {
	t.Parallel()

	for name, during := range map[string]func(*gitUpstream){
		"the name changed hands": func(u *gitUpstream) { u.ids.Store("api", u.nextID.Add(1)) },
		"the API could not say":  func(u *gitUpstream) { u.apiDown.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			upstream := newGitUpstream(t)
			service, node, token := gitNode(t, upstream)
			git := gitClient(t, node, token, githubBasic)
			if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
				t.Fatalf("clone: %v\n%s", err, output)
			}
			path := service.git.mirrorPath("untrusted/acme/api")
			if _, _, err := mirrorRepository(path); err != nil {
				t.Fatalf("the first mirror is not verified, so what follows proves nothing: %v", err)
			}

			hook := func() { during(upstream) }
			upstream.onMirrorFetch.Store(&hook)
			pushed := upstream.commitTo(t, "main", map[string]string{"f": "1\n"})
			before := upstream.guestFetches()
			if output, err := git("-C", "c", "pull", "-q", "--ff-only"); err != nil {
				t.Fatalf("a pull beside an unverified fetch: %v\n%s", err, output)
			}
			if output, err := git("-C", "c", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(output) != pushed {
				t.Fatalf("the pull is at %q (%v), want %s", output, err, pushed)
			}
			if _, _, err := mirrorRepository(path); !errors.Is(err, errMirrorUnverified) {
				t.Fatalf("the mirror after a fetch GitHub did not vouch for reads %v, want unverified", err)
			}
			if upstream.guestFetches() == before {
				t.Fatal("the pull was served from a mirror GitHub had not vouched for")
			}

			upstream.onMirrorFetch.Store(nil)
			upstream.apiDown.Store(false)
			if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
				t.Fatalf("ls-remote: %v\n%s", err, output)
			}
			if stamped, _, err := mirrorRepository(path); err != nil || stamped != upstream.repositoryID("api") {
				t.Fatalf("the unverified mirror was not replaced by a verified one: %d, %v", stamped, err)
			}
		})
	}
}

// THE REAPER TAKES ONLY WHAT A CLAIM RENAMED ASIDE, under a directory no
// repository's mirror can be named, and only once the claim has had its time
// to delete it: a repository whose name looks like a discard is a mirror.
func TestTheReaperTakesOnlyDiscardedMirrors(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.fullFraction = 1
	now := time.Now()
	service.now = func() time.Time { return now }
	live := service.git.mirrorPath("untrusted/acme/api.git.discard-demo")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, gitUsedStamp), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// THE LIVE MIRROR'S DIRECTORY IS AS OLD AS A LEFTOVER DISCARD, so only its
	// name keeps it, not its age; its stamp says it was used now.
	if err := os.Chtimes(live, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	discards := filepath.Join(service.git.root, gitDiscardDirectory)
	old, young := filepath.Join(discards, "mirror-old"), filepath.Join(discards, "mirror-young")
	for _, dir := range []string{old, young} {
		if err := os.MkdirAll(filepath.Join(dir, "mirror.git", "objects"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatalf("ReapGitMirrors: %v", err)
	}
	for path, want := range map[string]bool{live: true, old: false, young: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", path, err == nil, want)
		}
	}
}
