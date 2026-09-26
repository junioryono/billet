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
	"time"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
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

func pullRequestAuthority() server.CacheAuthority {
	return server.CacheAuthority{LeaseID: scopedLease, JobID: "job-1", RunID: 31, Owner: "acme",
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

	authority := &fakeAuthority{authority: server.CacheAuthority{LeaseID: scopedLease}}
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
		"unproven":   {&fakeAuthority{authority: server.CacheAuthority{LeaseID: scopedLease}}, 1},
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
