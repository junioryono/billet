package node

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingGitObserver keeps what the Git proxy told it.
type recordingGitObserver struct {
	mu        sync.Mutex
	requests  []string
	took      []time.Duration
	refreshes []string
	lookups   []string
	// maintenances is each maintenance pass as "kind outcome".
	maintenances []string
}

func (o *recordingGitObserver) GitRequest(route, reason, status string, took time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, route+" "+reason+" "+status)
	o.took = append(o.took, took)
}

func (o *recordingGitObserver) GitRefresh(outcome string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refreshes = append(o.refreshes, outcome)
}

func (o *recordingGitObserver) GitLookup(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lookups = append(o.lookups, outcome)
}

func (o *recordingGitObserver) GitMaintenance(kind, outcome string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.maintenances = append(o.maintenances, kind+" "+outcome)
}

// maintained is the maintenance passes the observer was told of.
func (o *recordingGitObserver) maintained() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.maintenances)
}

func (o *recordingGitObserver) seen() ([]string, []string, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.requests), slices.Clone(o.refreshes), slices.Clone(o.lookups)
}

// THE PROXY TELLS ITS OBSERVER WHAT IT DID, with the labels the metrics were
// registered with: a clone's advertisement and fetch served from the mirror,
// the refresh that made it, and the lookups that tied the name to its
// repository; and a fetch GitHub answered instead, under the reason why.
func TestTheGitProxyReportsWhatItDid(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	upstream.apiDown.Store(true)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "d"); err != nil {
		t.Fatalf("clone while the API is down: %v\n%s", err, output)
	}

	requests, refreshes, lookups := observer.seen()
	for _, want := range []string{"mirror served 2xx", "github no_repository_id 2xx"} {
		if !slices.Contains(requests, want) {
			t.Errorf("the observer was not told %q; it was told %v", want, requests)
		}
	}
	if !slices.Contains(refreshes, gitRefreshOK) {
		t.Errorf("the observer was not told of the refresh that made the mirror: %v", refreshes)
	}
	for _, want := range []string{gitLookupOK, gitLookupFailed} {
		if !slices.Contains(lookups, want) {
			t.Errorf("the observer was not told of a %s lookup: %v", want, lookups)
		}
	}
	for _, request := range requests {
		route, reason, status := splitThree(request)
		if !slices.Contains(GitRoutes(), route) || !slices.Contains(GitReasons(), reason) ||
			!slices.Contains(GitStatusClasses(), status) {
			t.Errorf("the observer was told %q, outside the labels the metrics were registered with", request)
		}
	}
}

// EACH REQUEST IS COUNTED EXACTLY ONCE, under its own labels: one served
// advertisement and one served fetch are two counts and no more, and a request
// without credentials, the first of every fetch, is counted once as refused.
func TestEachGitRequestIsCountedOnce(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)

	advertise(t, node, token, "api")
	if body := negotiate(t, node, token, githubBasic, runIn(t, upstream.repo, "rev-parse", "main")); !strings.Contains(string(body), "PACK") {
		t.Fatalf("the fetch was not served: %q", body[:min(len(body), 80)])
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		node.URL+"/v1/git/github.com/acme/api.git/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A REQUEST IS COUNTED AS ITS HANDLER ENDS, which can be just after the
	// client has its answer, so the count is awaited before it is judged.
	const want = 3
	requests := gitWithin(t, "three counted requests", func() []string {
		for t.Context().Err() == nil {
			if requests, _, _ := observer.seen(); len(requests) >= want {
				return requests
			}
			time.Sleep(10 * time.Millisecond)
		}

		return nil
	})
	slices.Sort(requests)
	if expected := []string{"mirror served 2xx", "mirror served 2xx", "refused credentials_required 4xx"}; !slices.Equal(requests, expected) {
		t.Fatalf("the three requests were counted as %v, want %v", requests, expected)
	}
	// A REFUSAL IS TIMED like any other request, from the handler's start.
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for i, request := range observer.requests {
		if observer.took[i] <= 0 {
			t.Errorf("%q was counted as taking %s", request, observer.took[i])
		}
	}
}

// A GRANT THAT ENDS WHILE THE MIRROR IS REFRESHED is counted as ended, not as
// a mirror that could not answer: the mirror worked, and GitHub answers the
// advertisement.
func TestAnAdvertisementWhoseGrantEndedIsCountedAsSuch(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	withdraw := func() {
		service.git.mu.Lock()
		defer service.git.mu.Unlock()
		for key, held := range service.git.authorised {
			service.git.authorised[key] = gitAuthorisation{sequence: held.sequence + 1, at: held.at}
		}
	}
	upstream.onMirrorFetch.Store(&withdraw)

	advertise(t, node, token, "api")
	requests := gitWithin(t, "the counted advertisement", func() []string {
		for t.Context().Err() == nil {
			if requests, _, _ := observer.seen(); len(requests) > 0 {
				return requests
			}
			time.Sleep(10 * time.Millisecond)
		}

		return nil
	})
	if want := []string{"github grant_ended 2xx"}; !slices.Equal(requests, want) {
		t.Fatalf("the advertisement was counted as %v, want %v", requests, want)
	}
	if _, refreshes, _ := observer.seen(); !slices.Equal(refreshes, []string{gitRefreshOK}) {
		t.Fatalf("the refresh was counted as %v, want one ok: the mirror worked", refreshes)
	}
}

// A REFRESH CUT BY CLOSE IS COUNTED AS CLOSED, not as whatever its killed fetch
// looked like: the proxy closes while the mirror's fetch is held at GitHub.
func TestARefreshCutByCloseIsCountedAsClosed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	upstream.holdMirrorFetches(t)
	fetching := make(chan struct{}, 1)
	signal := func() {
		select {
		case fetching <- struct{}{}:
		default:
		}
	}
	upstream.onMirrorFetch.Store(&signal)

	advertised := make(chan error, 1)
	go func() { advertised <- advertiseErr(t.Context(), node, token, "api") }()
	gitWithin(t, "the mirror's fetch reaching GitHub", func() struct{} {
		<-fetching

		return struct{}{}
	})
	gitWithin(t, "close", func() struct{} {
		service.git.close()

		return struct{}{}
	})
	if err := gitWithin(t, "the advertisement", func() error { return <-advertised }); err != nil {
		t.Fatalf("the advertisement beside a closed refresh: %v", err)
	}
	if _, refreshes, _ := observer.seen(); !slices.Equal(refreshes, []string{gitRefreshClosed}) {
		t.Fatalf("the refresh close cut was counted as %v, want one closed", refreshes)
	}
}

// A REQUEST IS COUNTED UNDER ITS FINAL STATUS: an interim 1xx a forward relays
// before GitHub's answer is not what the request answered, and a body written
// with no header is a 200.
func TestAnExchangeRecordsItsFinalStatus(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		write func(*gitExchange) error
		want  int
	}{
		"interim then final": {func(e *gitExchange) error {
			e.WriteHeader(http.StatusEarlyHints)
			e.WriteHeader(http.StatusForbidden)

			return nil
		}, http.StatusForbidden},
		"body alone": {func(e *gitExchange) error {
			_, err := e.Write([]byte("0000"))

			return err
		}, http.StatusOK},
		"first final kept": {func(e *gitExchange) error {
			e.WriteHeader(http.StatusBadGateway)
			e.WriteHeader(http.StatusOK)

			return nil
		}, http.StatusBadGateway},
	} {
		exchange := &gitExchange{ResponseWriter: httptest.NewRecorder(), moved: &atomic.Int64{}}
		if err := c.write(exchange); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if exchange.status != c.want {
			t.Errorf("%s: the exchange recorded %d, want %d", name, exchange.status, c.want)
		}
	}
}

// splitThree is a recorded request's route, reason and status.
func splitThree(request string) (string, string, string) {
	var parts [3]string
	i := 0
	for _, r := range request {
		if r == ' ' && i < 2 {
			i++

			continue
		}
		parts[i] += string(r)
	}

	return parts[0], parts[1], parts[2]
}

// A REFRESH IS COUNTED BY WHAT ENDED IT, read from the error's sentinel, never
// its text, and a lookup by GitHub's status and its rate-limit header.
func TestGitOutcomesAreReadFromWhatEndedThem(t *testing.T) {
	t.Parallel()

	for err, want := range map[error]string{
		nil: gitRefreshOK,
		// A FETCH CLOSE CUT is counted as closed whatever it looked like.
		errors.Join(errGitClosed, errFetchNotVerified): gitRefreshClosed,
		errFetchNotVerified:                            gitRefreshUnverified,
		errMirrorTooLarge:                              gitRefreshTooLarge,
		errFetchSlotsBusy:                              gitRefreshBusy,
		errGitDiskFull:                                 gitRefreshFull,
		errGitClosed:                                   gitRefreshClosed,
		errNotOurs:                                     gitRefreshFailed,
	} {
		if got := gitRefreshOutcome(err); got != want {
			t.Errorf("gitRefreshOutcome(%v) = %q, want %q", err, got, want)
		}
	}
	for _, c := range []struct {
		status    int
		remaining string
		want      string
	}{
		{200, "4999", gitLookupOK},
		{404, "4999", gitLookupRefused},
		{403, "12", gitLookupRefused},
		{403, "0", gitLookupLimited},
		{429, "", gitLookupLimited},
		{503, "", gitLookupFailed},
	} {
		if got := gitLookupOutcome(c.status, c.remaining); got != c.want {
			t.Errorf("gitLookupOutcome(%d, %q) = %q, want %q", c.status, c.remaining, got, c.want)
		}
	}
}

// errNotOurs is an error no sentinel names.
var errNotOurs = errorString("a fetch failed for a reason of its own")

type errorString string

func (e errorString) Error() string { return string(e) }
