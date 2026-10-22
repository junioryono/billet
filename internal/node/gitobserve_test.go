package node

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// recordingGitObserver keeps what the Git proxy told it.
type recordingGitObserver struct {
	mu        sync.Mutex
	requests  []string
	refreshes []string
	lookups   []string
}

func (o *recordingGitObserver) GitRequest(route, reason, status string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, route+" "+reason+" "+status)
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
		nil:                 gitRefreshOK,
		errFetchNotVerified: gitRefreshUnverified,
		errMirrorTooLarge:   gitRefreshTooLarge,
		errFetchSlotsBusy:   gitRefreshBusy,
		errGitDiskFull:      gitRefreshFull,
		errGitClosed:        gitRefreshClosed,
		errNotOurs:          gitRefreshFailed,
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
