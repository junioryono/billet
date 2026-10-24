package node

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// gitLog is the node's log, which its handlers and refreshes write at once.
type gitLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *gitLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *gitLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

// guestFetches is how many fetches github.com answered for guests, the node's
// own mirror fetches aside.
func (u *gitUpstream) guestFetches() int64 { return u.fetches.Load() - u.mirrorFetches.Load() }

// gitWrapper puts a git in front of the node's that answers every fetch served
// from the mirror as mode's file says: "refuse" with an ERR line, "silent" with
// nothing at all, "hang" with nothing for ten minutes, "nakfail" with a NAK and
// then a failure, "nakcrash" with a NAK and then a crash on SIGSEGV, "slow" as git would once mode.release exists, after touching
// mode.started, anything else as git would; "slowadvert" holds the mirror's
// advertisement the same way.
func gitWrapper(t *testing.T, service *CacheService) (mode string) {
	t.Helper()

	mode = filepath.Join(t.TempDir(), "mode")
	if err := os.WriteFile(mode, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"*\" --advertise-refs \"*)\n" +
		"  if [ \"$(cat " + mode + ")\" = slowadvert ]; then\n" +
		"    : >" + mode + ".started; while [ ! -e " + mode + ".release ]; do sleep 0.05; done\n" +
		"  fi\n" +
		"  exec git \"$@\" ;;\n" +
		"*\" upload-pack \"*)\n" +
		"  case \"$(cat " + mode + ")\" in\n" +
		"  refuse) printf '0017ERR forced refusal\\n'; exit 1 ;;\n" +
		"  silent) exit 128 ;;\n" +
		"  hang) sleep 600; exit 128 ;;\n" +
		"  slow) : >" + mode + ".started; while [ ! -e " + mode + ".release ]; do sleep 0.05; done ;;\n" +
		"  nakfail) printf '0008NAK\\n'; exit 128 ;;\n" +
		"  nakcrash) printf '0008NAK\\n'; kill -SEGV $$ ;;\n" +
		"  esac ;;\n" +
		"esac\n" +
		"exec git \"$@\"\n"
	if err := forkSafeWriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	service.git.binary = wrapper

	return mode
}

// A FETCH THE MIRROR'S OWN UPLOAD-PACK REFUSES, OR LEAVES UNANSWERED, IS
// GITHUB'S: nothing reaches the client before upload-pack's first line, so the
// client never sees the mirror's refusal.
func TestTheMirrorsRefusalIsAnsweredByGitHub(t *testing.T) {
	t.Parallel()

	for _, behaviour := range []string{"refuse", "silent", "hang"} {
		t.Run(behaviour, func(t *testing.T) {
			t.Parallel()

			upstream := newGitUpstream(t)
			service, node, token := gitNode(t, upstream)
			var log gitLog
			service.log = slog.New(slog.NewTextHandler(&log, nil))
			mode := gitWrapper(t, service)
			// A HUNG upload-pack IS GIVEN UP ON well inside the request's own
			// life, so the request is still alive for GitHub to answer.
			service.git.firstLineWait = 500 * time.Millisecond
			git := gitClient(t, node, token, githubBasic)
			if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
				t.Fatalf("clone: %v\n%s", err, output)
			}
			if err := os.WriteFile(mode, []byte(behaviour), 0o600); err != nil {
				t.Fatal(err)
			}
			pushed := upstream.commitTo(t, "main", map[string]string{"f": "1\n"})
			before := upstream.guestFetches()
			if output, err := git("-C", "c", "pull", "-q", "--ff-only"); err != nil {
				t.Fatalf("a pull the mirror would not answer: %v\n%s", err, output)
			}
			if output, err := git("-C", "c", "rev-parse", "HEAD"); err != nil || strings.TrimSpace(output) != pushed {
				t.Fatalf("the pull is at %q (%v), want %s", output, err, pushed)
			}
			if got := upstream.guestFetches() - before; got != 1 {
				t.Fatalf("github.com answered %d of the pull's fetches, want the one the mirror would not", got)
			}
			want := map[string]string{"refuse": "the mirror refused: ERR forced refusal",
				"silent": "the mirror's upload-pack said nothing",
				"hang":   "the mirror's upload-pack said nothing within 500ms"}[behaviour]
			if !strings.Contains(log.String(), want) {
				t.Fatalf("the request's line does not say %q:\n%s", want, log.String())
			}
		})
	}
}

// AN ADVERTISEMENT WAITS FOR ITS MIRROR'S REFRESH ONLY AS LONG AS ITS BOUND, and
// every job then gets GitHub's answer; one refresh serves them all, runs on
// after every one of them has gone, and the next job is served from it.
func TestARefreshHoldsNoJobPastItsWait(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	service.git.refreshWait = 300 * time.Millisecond
	release := upstream.holdMirrorFetches(t)
	git := gitClient(t, node, token, githubBasic)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)

	// HELD UNTIL EVERY CLONE HAS RETURNED, so a clone that waited for the
	// refresh rather than for its bound never returns before the guard.
	const jobs = 6
	errs := gitWithin(t, "clones beside a held refresh", func() []error {
		var wg sync.WaitGroup
		errs := make([]error, jobs)
		for i := range jobs {
			wg.Go(func() {
				if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"+string(rune('a'+i))); err != nil {
					errs[i] = errors.New(err.Error() + "\n" + output)
				}
			})
		}
		wg.Wait()

		return errs
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("clone %d beside a held refresh: %v", i, err)
		}
	}

	release()
	want := runIn(t, upstream.repo, "rev-parse", "main")
	path := service.git.mirrorPath("untrusted/acme/api")
	gitWithin(t, "the refresh no job waited for", func() struct{} {
		// ENDS WITH THE TEST, so a refresh that never lands leaves no loop behind.
		for t.Context().Err() == nil {
			refs, err := service.mirrorRefs(t.Context(), path)
			if err == nil && refs["refs/heads/main"] == want {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		return struct{}{}
	})
	// COUNTED ONCE THE MIRROR IS IDLE, not when the clones return: the upstream
	// counts a fetch when the node's git sends it, after its handshake, which
	// under -race came after all six had given up waiting (0 counted, CI,
	// 2026-10-22). The wait is on every refresh's own goroutine, not on the
	// mirror's refreshing mark, which a refresh that failed to join another
	// would overwrite. The refreshes are counted as well as the fetches,
	// because a second refresh behind the mirror's lock finds nothing to fetch.
	gitSettled(t, service, "every refresh the jobs started")
	if got := upstream.mirrorFetches.Load(); got != 1 {
		t.Fatalf("the node made %d mirror fetches for %d jobs, want one refresh they shared", got, jobs)
	}
	if _, refreshes, _ := observer.seen(); !slices.Equal(refreshes, []string{gitRefreshOK}) {
		t.Fatalf("the node refreshed %v for %d jobs, want one refresh they shared", refreshes, jobs)
	}
	before := upstream.guestFetches()
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "after"); err != nil {
		t.Fatalf("the clone after the refresh: %v\n%s", err, output)
	}
	if got := upstream.guestFetches() - before; got != 0 {
		t.Fatalf("the clone after the refresh went to github.com (%d fetches), want the mirror", got)
	}
}

// A REFRESH KILLED MID-FETCH LEAVES LOCK FILES, and the next one clears them
// rather than failing on them for every job after.
func TestAKilledRefreshsLocksDoNotStopTheNext(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	path := service.git.mirrorPath("untrusted/acme/api")
	for _, lock := range []string{"refs/heads/main.lock", "packed-refs.lock", "objects/pack/tmp_pack_left"} {
		if err := os.WriteFile(filepath.Join(path, lock), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pushed := upstream.commitTo(t, "main", map[string]string{"f": "1\n"})
	if output, err := git("ls-remote", "https://github.com/acme/api.git"); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, output)
	}
	if got := runIn(t, path, "rev-parse", "refs/heads/main"); got != pushed {
		t.Fatalf("the mirror's main is %s after a refresh past stale locks, want %s", got, pushed)
	}
	if _, err := os.Stat(filepath.Join(path, "objects", "pack", "tmp_pack_left")); err == nil {
		t.Fatal("a killed fetch's temporary pack was left behind")
	}
}

// A FORWARDED REQUEST'S BODY IS WHOLE BEFORE GITHUB SEES A BYTE, so a guest that
// sends it slowly is never ended by GitHub's idle timeout on request bodies,
// ten seconds there and a fifth of a second here.
func TestAForwardedBodyIsWholeBeforeGitHubSeesIt(t *testing.T) {
	t.Parallel()

	const idle = 200 * time.Millisecond
	var lengths sync.Map
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lengths.Store(r.URL.Path, r.ContentLength)
		buffer := make([]byte, 64)
		last := time.Now()
		for {
			n, err := r.Body.Read(buffer)
			if n > 0 {
				if time.Since(last) > idle {
					w.WriteHeader(http.StatusRequestTimeout)

					return
				}
				last = time.Now()
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}
		}
		if _, err := io.WriteString(w, "0008NAK\n"); err != nil {
			return
		}
	}))
	t.Cleanup(github.Close)

	// HELD IN MEMORY, and past a limit of sixteen bytes on disk; each on a
	// repository of its own, so the two can run at once.
	for _, limit := range []int64{gitRequestLimit, 16} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			t.Parallel()

			forwardSlowBody(t, github.URL, fmt.Sprintf("api%d", limit), limit, idle, &lengths)
		})
	}
}

// forwardSlowBody sends a fetch whose body stops for three times GitHub's idle
// limit halfway through, through a node holding limit bytes in memory, and
// fails unless GitHub answered it whole.
func forwardSlowBody(t *testing.T, github, repo string, limit int64, idle time.Duration, lengths *sync.Map) {
	t.Helper()

	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.fullFraction, service.git.requestLimit = github, 1, limit
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	reader, writer := io.Pipe()
	go func() {
		if _, err := io.WriteString(writer, "0032want "+strings.Repeat("a", 40)+"\n"); err != nil {
			writer.CloseWithError(err)

			return
		}
		time.Sleep(3 * idle)
		_, err := io.WriteString(writer, "00000009done\n")
		writer.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		node.URL+"/v1/git/github.com/acme/"+repo+".git/git-upload-pack", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(token, githubBasic)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a slowly sent fetch was answered %d, want github.com's 200", resp.StatusCode)
	}
	if length, _ := lengths.Load("/acme/" + repo + ".git/git-upload-pack"); length != int64(len("0032want "+strings.Repeat("a", 40)+"\n00000009done\n")) {
		t.Fatalf("github.com was sent a body of length %v, want the whole body's", length)
	}
}

// THE PROXY REACHES GITHUB OVER HTTP/1.1, one connection per request, even
// where GitHub offers HTTP/2: never one connection that every job shares.
func TestTheProxyReachesGitHubOverHTTP1(t *testing.T) {
	t.Parallel()

	var protocols sync.Map
	github := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocols.Store(r.Method+" "+r.URL.Path, r.ProtoMajor)
		http.Error(w, "scripted", http.StatusNotFound)
	}))
	github.EnableHTTP2 = true
	github.StartTLS()
	t.Cleanup(github.Close)
	// THE STAND-IN DOES SPEAK HTTP/2, to a client that asks for it.
	controlRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, github.URL+"/control", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	control, err := github.Client().Do(controlRequest)
	if err != nil {
		t.Fatal(err)
	}
	control.Body.Close()
	if control.ProtoMajor != 2 {
		t.Fatalf("the stand-in answered HTTP/%d, so it cannot tell the proxy's protocol", control.ProtoMajor)
	}

	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.fullFraction = github.URL, 1
	pool := x509.NewCertPool()
	pool.AddCert(github.Certificate())
	transport, ok := service.git.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the proxy's transport is a %T", service.git.client.Transport)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	if err := advertiseErr(t.Context(), node, token, "api"); err != nil {
		t.Fatal(err)
	}
	negotiate(t, node, token, githubBasic, strings.Repeat("a", 40))
	seen := 0
	protocols.Range(func(key, value any) bool {
		if key == "GET /control" {
			return true
		}
		seen++
		if value != 1 {
			t.Errorf("%s reached github.com over HTTP/%v", key, value)
		}

		return true
	})
	if seen != 2 {
		t.Fatalf("github.com saw %d of the proxy's requests, want the advertisement and the forward", seen)
	}
}

// A GIT REQUEST ON WHICH NO BYTE MOVES ENDS AT THE IDLE LIMIT, however long its
// deadline; one whose bytes keep moving runs past that limit to its end.
//
// THE MOVING ONE SENDS A BYTE EVERY 50 ms AGAINST A ONE-SECOND LIMIT, for longer
// than the limit in all. Against a 300 ms limit, a pause of the test process under
// -race in CI ended it after one byte (unexpected EOF, 2026-10-23); the same
// cadence under a limit twenty times its gap leaves room for such a pause.
func TestAStalledGitRequestEndsAndAMovingOneDoesNot(t *testing.T) {
	t.Parallel()

	const idle = time.Second
	// 30 bytes 50 ms apart: the last at 1.45 s and the end at 1.5 s, past the
	// 1 s limit, so the request cannot finish before the limit and prove nothing.
	const movingBytes = 30
	var stallEnded atomic.Bool
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)

			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/acme/stall"):
			if _, err := io.WriteString(w, "0008NAK\n"); err != nil {
				return
			}
			flusher.Flush()
			<-r.Context().Done()
			stallEnded.Store(true)
		case strings.HasPrefix(r.URL.Path, "/acme/moving"):
			for range movingBytes {
				if _, err := io.WriteString(w, "x"); err != nil {
					return
				}
				flusher.Flush()
				time.Sleep(idle / 20)
			}
		}
	}))
	t.Cleanup(github.Close)
	service, _, token, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	service.git.upstream, service.git.fullFraction, service.git.idleLimit = github.URL, 1, idle
	node := httptest.NewServer(service)
	t.Cleanup(node.Close)

	post := func(repo string) ([]byte, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			node.URL+"/v1/git/github.com/acme/"+repo+".git/git-upload-pack", strings.NewReader("0000"))
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(token, githubBasic)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		return io.ReadAll(resp.Body)
	}
	type answer struct {
		body []byte
		err  error
	}
	stalled := gitWithin(t, "a stalled git request", func() answer {
		body, err := post("stall")

		return answer{body, err}
	})
	if stalled.err == nil && len(stalled.body) > len("0008NAK\n") {
		t.Fatalf("the stalled request answered %q", stalled.body)
	}
	gitWithin(t, "github.com's side of the stalled request", func() struct{} {
		for !stallEnded.Load() && t.Context().Err() == nil {
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})

	moving := gitWithin(t, "a moving git request", func() answer {
		body, err := post("moving")

		return answer{body, err}
	})
	if moving.err != nil || string(moving.body) != strings.Repeat("x", movingBytes) {
		t.Fatalf("a request whose bytes kept moving past the idle limit ended with %q (%v)", moving.body, moving.err)
	}
}
