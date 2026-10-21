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
	if !service.git.isBroken("untrusted/acme/api") {
		t.Fatal("the mirror whose serve failed mid-answer was not marked broken")
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
	if service.git.isBroken("untrusted/acme/api") {
		t.Fatal("the broken mark outlived the mirror it was on")
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

	reader, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	go func() {
		// HALF A NEGOTIATION, then nothing until the test ends.
		if _, err := io.WriteString(writer, "0032want "+strings.Repeat("a", 40)+"\n"); err != nil {
			writer.CloseWithError(err)
		}
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		node.URL+"/v1/git/github.com/acme/api.git/git-upload-pack", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(token, githubBasic)
	gitWithin(t, "a request whose guest stopped sending", func() struct{} {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}

		return struct{}{}
	})
	gitWithin(t, "the stalled request's slots", func() struct{} {
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
