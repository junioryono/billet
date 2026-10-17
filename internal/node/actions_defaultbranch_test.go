package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// presentMatcher answers a lookup only for keys that were published, so a
// test can see which scope a lookup found its entry in.
type presentMatcher struct {
	*fakeCacheStore
	present map[string]string
	asked   []string
}

func (m *presentMatcher) Match(
	_ context.Context, exact string, restore []string,
) (string, string, error) {
	m.asked = append(m.asked, exact)
	if generation, ok := m.present[exact]; ok {
		return exact, generation, nil
	}
	for _, prefix := range restore {
		for key, generation := range m.present {
			if strings.HasPrefix(key, prefix) {
				return key, generation, nil
			}
		}
	}

	return "", "", storecontract.ErrMiss
}

// refPrefix is the key prefix of a v1 archive under ref.
func refPrefix(ref string) string {
	refDigest := sha256.Sum256([]byte(ref))
	versionDigest := sha256.Sum256([]byte("v1"))

	return "test-deployment.scoped/untrusted/acme/api/any/actions/" +
		hex.EncodeToString(refDigest[:]) + "/" + hex.EncodeToString(versionDigest[:]) + "/"
}

// defaultBranchActions is an untrusted default-branch session with the Actions
// cache, asking authority about its job.
func defaultBranchActions(
	t *testing.T, authority *fakeAuthority, storage storecontract.Store,
) (*CacheService, *cacheSession) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetAuthorityReader(authority)
	spec := defaultBranchCache()
	spec.Actions.Enabled = true
	credentials, err := service.PrepareScoped(provider.InstanceName(scopedLease), CacheSessionScope{
		Trust: provider.TrustUntrusted, Intercept: true, LeaseID: scopedLease, Epoch: 1, Cache: spec,
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, service.byToken[credentials.Token]
}

// forkAuthority is the plane's answer about a job it has bound and cannot
// prove: a fork's pull request, named by its job.
func forkAuthority() dispatch.CacheAuthority {
	return dispatch.CacheAuthority{LeaseID: scopedLease, JobID: "job-1", RunID: 31, Owner: "acme",
		Repository: "api", Event: "pull_request"}
}

// unboundAnswer is the plane's answer about a lease whose job it has not yet
// learned.
func unboundAnswer() dispatch.CacheAuthority {
	return dispatch.CacheAuthority{LeaseID: scopedLease}
}

func pullRequestAuthority() dispatch.CacheAuthority {
	return dispatch.CacheAuthority{LeaseID: scopedLease, JobID: "job-1", RunID: 31, Owner: "acme",
		Repository: "api", Event: "pull_request", Ref: "refs/pull/7/merge",
		BaseRef: "refs/heads/release", DefaultRef: "refs/heads/main", Proven: true, WriteOwnRef: true}
}

func twirp(t *testing.T, path, body string) *http.Request {
	t.Helper()

	return actionsRequestForTest(t, http.MethodPost, "https://"+actionsResultsHost+path, body)
}

// A JOB THAT CANNOT BE PROVED IS NOT ASKED ABOUT ON EVERY CALL: its answer
// stands for actionsUnprovenFor, because each ask costs the control plane
// GitHub requests and such a job (a fork's pull request) makes many calls.
func TestAnUnprovenJobIsAskedAboutAtMostEveryInterval(t *testing.T) {
	t.Parallel()

	authority := &fakeAuthority{authority: forkAuthority()}
	service, session := defaultBranchActions(t, authority, &fakeCacheStore{})
	now := time.Now()
	service.now = func() time.Time { return now }

	for range 5 {
		if _, ok := service.sessionActionsAuthority(t.Context(), session); ok {
			t.Fatal("an unproven job was given an authority")
		}
	}
	if authority.asked != 1 {
		t.Fatalf("asked %d times within the interval, want once", authority.asked)
	}
	now = now.Add(actionsUnprovenFor)
	authority.authority = pullRequestAuthority()
	if _, ok := service.sessionActionsAuthority(t.Context(), session); !ok || authority.asked != 2 {
		t.Fatalf("after the interval: proven %v, asked %d, want a fresh ask proving it", ok, authority.asked)
	}
}

// A PULL REQUEST SAVES UNDER ITS OWN MERGE REF AND NOWHERE ELSE, on an
// untrusted pool, served locally because GitHub proved which ref it runs for.
func TestAPullRequestSavesUnderItsOwnRef(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, session := defaultBranchActions(t,
		&fakeAuthority{authority: pullRequestAuthority()}, storage)

	response, handled, err := service.actionsResponse(
		twirp(t, actionsCreatePath, `{"key":"npm","version":"v1"}`), session)
	if err != nil || !handled {
		t.Fatalf("create handled=%t err=%v, want it served locally", handled, err)
	}
	response.Body.Close()
	want := refPrefix("refs/pull/7/merge") + "npm"
	if len(storage.keys) == 0 || storage.keys[len(storage.keys)-1] != want {
		t.Fatalf("reserved %v, want %q", storage.keys, want)
	}
}

// A LOOKUP RESTORES FROM THE JOB'S OWN REF, THEN ITS BASE, THEN THE DEFAULT
// BRANCH, which is GitHub's own order, and stops at the first that has it.
func TestALookupRestoresInGitHubsOrder(t *testing.T) {
	t.Parallel()

	matcher := &presentMatcher{fakeCacheStore: &fakeCacheStore{current: "g"},
		present: map[string]string{refPrefix("refs/heads/main") + "npm": "g"}}
	service, session := defaultBranchActions(t,
		&fakeAuthority{authority: pullRequestAuthority()}, matcher)

	response, handled, err := service.actionsResponse(
		twirp(t, actionsDownloadPath, `{"key":"npm","restore_keys":[],"version":"v1"}`), session)
	if err != nil || !handled {
		t.Fatalf("lookup handled=%t err=%v", handled, err)
	}
	found := responseJSON(t, response)
	if found["ok"] != true || found["matched_key"] != "npm" {
		t.Fatalf("lookup = %v, want the default branch's entry", found)
	}
	want := []string{
		refPrefix("refs/pull/7/merge") + "npm",
		refPrefix("refs/heads/release") + "npm",
		refPrefix("refs/heads/main") + "npm",
	}
	if strings.Join(matcher.asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("asked\n%s\nwant\n%s", strings.Join(matcher.asked, "\n"), strings.Join(want, "\n"))
	}
}

// A JOB THAT MAY READ AND NOT WRITE HAS ITS SAVE GO TO GITHUB, which applies
// the same rule; nothing is reserved locally.
func TestAReadOnlyJobsSaveGoesToGitHub(t *testing.T) {
	t.Parallel()

	target := pullRequestAuthority()
	target.Event, target.Ref, target.BaseRef, target.WriteOwnRef = "pull_request_target",
		"refs/heads/main", "", false
	storage := &fakeCacheStore{}
	service, session := defaultBranchActions(t, &fakeAuthority{authority: target}, storage)

	response, handled, err := service.actionsResponse(
		twirp(t, actionsCreatePath, `{"key":"npm","version":"v1"}`), session)
	if response != nil {
		response.Body.Close()
	}
	if handled || err != nil || len(storage.keys) != 0 {
		t.Fatalf("create handled=%t err=%v keys=%v, want it spliced", handled, err, storage.keys)
	}
}

// AN UNPROVEN JOB GOES WHOLLY TO GITHUB. An unproven answer stands for
// actionsUnprovenFor (TestAnUnprovenJobIsAskedAboutAtMostEveryInterval); a plane
// that could not be reached is asked again on the next call, because nothing
// was decided.
func TestAnUnprovenJobGoesToGitHubAndIsAskedAgain(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		authority *fakeAuthority
		asks      int
	}{
		"unproven":   {&fakeAuthority{authority: forkAuthority()}, 1},
		"unreadable": {&fakeAuthority{err: errors.New("the plane is unreachable")}, 2},
	} {
		authority := tc.authority
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			service, session := defaultBranchActions(t, authority, storage)
			for range 2 {
				response, handled, err := service.actionsResponse(
					twirp(t, actionsDownloadPath, `{"key":"npm","restore_keys":[],"version":"v1"}`),
					session)
				if response != nil {
					response.Body.Close()
				}
				if handled || err != nil {
					t.Fatalf("lookup handled=%t err=%v, want it spliced", handled, err)
				}
			}
			if authority.asked != tc.asks || len(storage.keys) != 0 {
				t.Fatalf("asked %d times, keys %v; want %d asks and no storage", authority.asked,
					storage.keys, tc.asks)
			}
		})
	}
}

// bindingAuthority is a plane that has not bound the job for its first
// unboundAsks questions and names it afterwards; a negative count never binds
// it.
type bindingAuthority struct {
	unboundAsks int
	job         dispatch.CacheAuthority
	asked       int
}

func (b *bindingAuthority) CacheAuthority(context.Context, string) (dispatch.CacheAuthority, error) {
	b.asked++
	if b.unboundAsks < 0 || b.asked <= b.unboundAsks {
		return unboundAnswer(), nil
	}

	return b.job, nil
}

// lookup is one restore of key "npm", and whether it was served here rather
// than spliced to GitHub.
func lookup(t *testing.T, service *CacheService, session *cacheSession) bool {
	t.Helper()

	response, handled, err := service.actionsResponse(
		twirp(t, actionsDownloadPath, `{"key":"npm","restore_keys":[],"version":"v1"}`), session)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if response != nil {
		response.Body.Close()
	}

	return handled
}

// A JOB THE PLANE HAS NOT BOUND YET IS WAITED FOR, AND SERVED HERE. Its first
// cache call, a restore, came before the plane had GitHub's JobStarted; spliced,
// it would have fetched the whole archive from GitHub.
func TestACallBeforeTheBindingWaitsForIt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		plane := &bindingAuthority{unboundAsks: 3, job: pullRequestAuthority()}
		service, session := defaultBranchActions(t, nil, &fakeCacheStore{})
		service.SetAuthorityReader(plane)
		start := time.Now()

		if !lookup(t, service, session) {
			t.Fatal("the restore was spliced to GitHub, want it served once the plane bound the job")
		}
		if plane.asked != 4 {
			t.Fatalf("asked %d times, want 3 unbound answers and the bound one", plane.asked)
		}
		if waited := time.Since(start); waited != 3*actionsBindingPoll {
			t.Fatalf("waited %v, want %v: three polls, and no longer once the job was named",
				waited, 3*actionsBindingPoll)
		}
	})
}

// A JOB THE PLANE NEVER BINDS IS WAITED FOR ONCE, for actionsBindingWait, and
// every call after it goes to GitHub at once as any unproven job's does: a
// job's many calls must not each stand still for a minute.
func TestAJobNeverBoundIsWaitedForOnce(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		plane := &bindingAuthority{unboundAsks: -1}
		service, session := defaultBranchActions(t, nil, &fakeCacheStore{})
		service.SetAuthorityReader(plane)
		start := time.Now()

		if lookup(t, service, session) {
			t.Fatal("a job that was never bound was served here")
		}
		if waited := time.Since(start); waited != actionsBindingWait {
			t.Fatalf("the first call waited %v, want %v", waited, actionsBindingWait)
		}

		time.Sleep(actionsUnprovenFor)
		asked, again := plane.asked, time.Now()
		if lookup(t, service, session) {
			t.Fatal("a job that was never bound was served here")
		}
		if plane.asked != asked+1 || time.Since(again) != 0 {
			t.Fatalf("the next call asked %d times over %v, want one ask and no wait",
				plane.asked-asked, time.Since(again))
		}
	})
}

// A JOB THE PLANE KNOWS AND CANNOT PROVE IS NOT WAITED FOR: a fork's pull
// request is named by its job, and waiting would only delay its splice.
func TestAnUnprovableJobIsNotWaitedFor(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		plane := &fakeAuthority{authority: forkAuthority()}
		service, session := defaultBranchActions(t, plane, &fakeCacheStore{})
		start := time.Now()

		if lookup(t, service, session) {
			t.Fatal("an unprovable job was served here")
		}
		if plane.asked != 1 || time.Since(start) != 0 {
			t.Fatalf("asked %d times over %v, want one ask and no wait", plane.asked, time.Since(start))
		}
	})
}

// A CALLER THAT GIVES UP WHILE WAITING LEAVES THE WAIT TO ITS RETRY: nothing
// was decided, so the retry is neither held off as unproven nor denied the
// wait, and it is proven once the plane names the job.
func TestARetryAfterAnAbandonedWaitWaitsAgain(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		plane := &bindingAuthority{unboundAsks: 10, job: pullRequestAuthority()}
		service, session := defaultBranchActions(t, nil, &fakeCacheStore{})
		service.SetAuthorityReader(plane)

		ctx, cancel := context.WithTimeout(t.Context(), 5*actionsBindingPoll+actionsBindingPoll/2)
		defer cancel()
		if _, ok := service.sessionActionsAuthority(ctx, session); ok {
			t.Fatal("an abandoned wait proved the job")
		}

		if _, ok := service.sessionActionsAuthority(t.Context(), session); !ok {
			t.Fatal("the retry was not proven once the plane named the job")
		}
		if plane.asked != 11 {
			t.Fatalf("asked %d times, want 11: the retry waited on until the job was named", plane.asked)
		}
	})
}
