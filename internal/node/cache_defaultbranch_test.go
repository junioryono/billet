package node

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
	storecontract "github.com/junioryono/billet/internal/store"
)

const scopedLease = "a1"

// fakeAuthority answers a re-check with whatever the test set, and counts.
type fakeAuthority struct {
	authority server.CacheAuthority
	err       error
	asked     int
}

func (f *fakeAuthority) CacheAuthority(context.Context, string) (server.CacheAuthority, error) {
	f.asked++

	return f.authority, f.err
}

func publishingAuthority() server.CacheAuthority {
	return server.CacheAuthority{LeaseID: scopedLease, JobID: "job-1", RunID: 31, Owner: "acme",
		Repository: "api", Event: "push", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Proven: true, WriteOwnRef: true, PublishDefault: true}
}

func defaultBranchCache() *config.CacheSpec {
	spec := config.Tier{Cache: &config.TierCache{Publish: config.CachePublishDefaultBranch},
		CacheScope: &config.CacheScope{Owner: "acme", Repository: "api"}}.EffectiveCache()

	return &spec
}

// scopedService is a cache service with one session scoped by spec, under a
// lease its instance is named after, asking authority about its job.
func scopedService(
	t *testing.T, trust provider.TrustClass, spec *config.CacheSpec, authority *fakeAuthority,
	storage storecontract.Store,
) (*CacheService, string, string) {
	t.Helper()

	service, token, instance := scopedServiceIn(t, t.TempDir(), trust, spec, storage)
	if authority != nil {
		service.SetAuthorityReader(authority)
	}

	return service, token, instance
}

// scopedServiceIn is scopedService with its state in dir, so a test can start
// another process on the same state.
func scopedServiceIn(
	t *testing.T, dir string, trust provider.TrustClass, spec *config.CacheSpec,
	storage storecontract.Store,
) (*CacheService, string, string) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", dir,
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	instance := provider.InstanceName(scopedLease)
	credentials, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: trust, LeaseID: scopedLease, Epoch: 1, Cache: spec,
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, credentials.Token, instance
}

// attachAndCommit attaches one sticky disk and commits it with a clean proof.
func attachAndCommit(t *testing.T, service *CacheService, token string) map[string]any {
	t.Helper()

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "npm", "size_bytes": int64(10 << 30), "architecture": "amd64",
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	committed := cacheRequest(t, service, token, "/v1/volumes/0/commit", map[string]any{
		"filesystem": map[string]any{"type": "ext4", "uuid": "fs-1", "clean": true},
	})
	if committed.Code != http.StatusOK {
		t.Fatalf("commit status = %d: %s", committed.Code, committed.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(committed.Body.Bytes(), &body); err != nil {
		t.Fatalf("commit body: %v", err)
	}

	return body
}

// endSession settles the completion, closes the session as a proved teardown
// does, and runs the cache loop's pass.
func endSession(t *testing.T, service *CacheService, instance string, authority server.CacheAuthority) {
	t.Helper()

	if err := service.SettleCompleted(t.Context(), instance, true, authority); err != nil {
		t.Fatalf("SettleCompleted: %v", err)
	}
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
}

// A DEFAULT-BRANCH NAMESPACE IS NOT ONE A GUEST CAN NAME. Every key it uses
// sits under the namespace's own root, scoped by trust, repository and
// architecture, and no guest key's spelling reaches it.
func TestADefaultBranchSessionUsesItsOwnNamespace(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, token, _ := scopedService(t, provider.TrustUntrusted, defaultBranchCache(), nil, storage)
	attachAndCommit(t, service, token)

	want := "test-deployment.scoped/untrusted/acme/api/amd64/sticky/npm"
	if len(storage.keys) == 0 || storage.keys[0] != want {
		t.Fatalf("keys = %v, want the first to be %q", storage.keys, want)
	}
	for _, key := range storage.keys {
		if strings.HasPrefix(key, "test-deployment/") {
			t.Errorf("a default-branch session touched the guest-named namespace: %q", key)
		}
	}
}

// A DEFAULT-BRANCH COMMIT WAITS FOR ITS JOB'S COMPLETION, and an authorised
// completion publishes it after the compute is gone. The clone is consumed by
// the snapshot, so nothing is discarded.
func TestAnAuthorisedDefaultBranchWritePublishesAfterTheComputeIsGone(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, token, instance := scopedService(t, provider.TrustUntrusted, defaultBranchCache(),
		nil, storage)

	body := attachAndCommit(t, service, token)
	if body["pending"] != true || body["published"] != false {
		t.Fatalf("commit answered %v, want pending and unpublished", body)
	}
	if storage.snapshots != 0 || storage.published != 0 || storage.discarded != 0 {
		t.Fatalf("a commit touched storage before the job completed: %d/%d/%d",
			storage.snapshots, storage.published, storage.discarded)
	}

	endSession(t, service, instance, publishingAuthority())

	if storage.published != 1 || storage.discarded != 0 {
		t.Fatalf("publish/discard = %d/%d, want 1/0", storage.published, storage.discarded)
	}
}

// killSwitch blocks every cache it is asked about, and counts.
type killSwitch struct{ asked int }

func (k *killSwitch) CacheAllowed(context.Context, config.CacheKind, string, string) (bool, error) {
	k.asked++

	return false, nil
}

// EVERY WAY A COMPLETION CAN FAIL TO AUTHORISE LEAVES THE WRITE UNPUBLISHED,
// and the clone discarded with the session; so does a kill switch set after
// the completion authorised it.
func TestAnUnauthorisedDefaultBranchWriteIsDiscarded(t *testing.T) {
	t.Parallel()

	another := publishingAuthority()
	another.LeaseID = "a2"
	otherRepository := publishingAuthority()
	otherRepository.Repository = "web"
	pullRequest := publishingAuthority()
	pullRequest.PublishDefault = false

	for name, tc := range map[string]struct {
		completion server.CacheAuthority
		blocked    bool
	}{
		"a pull request":                          {completion: pullRequest},
		"an authority for another lease":          {completion: another},
		"an authority for another repository":     {completion: otherRepository},
		"the zero authority an older plane sends": {},
		"a kill switch set after the completion":  {completion: publishingAuthority(), blocked: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			service, token, instance := scopedService(t, provider.TrustUntrusted,
				defaultBranchCache(), nil, storage)
			attachAndCommit(t, service, token)
			if err := service.SettleCompleted(t.Context(), instance, true, tc.completion); err != nil {
				t.Fatalf("SettleCompleted: %v", err)
			}
			blocker := &killSwitch{}
			if tc.blocked {
				service.SetCachePolicy(blocker)
			}
			if err := service.Close(t.Context(), instance); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := service.RetryClosed(t.Context()); err != nil {
				t.Fatalf("RetryClosed: %v", err)
			}

			if storage.published != 0 || storage.snapshots != 0 || storage.discarded != 1 {
				t.Fatalf("published %d, snapshotted %d, discarded %d; want only the discard",
					storage.published, storage.snapshots, storage.discarded)
			}
			if tc.blocked && blocker.asked == 0 {
				t.Error("the kill switch was never asked")
			}
		})
	}
}

// A PUBLICATION A CRASH INTERRUPTED IS NEVER SNAPSHOTTED TWICE, reconciled by
// a fresh process from the journal on disk: a journal that says the snapshot
// may have run is abandoned, because which candidate exists cannot be told;
// one that says the pointer may have moved checks the pointer before
// publishing again.
func TestAnInterruptedPublicationIsReconciledFromItsJournal(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		journal       publishJournal
		current       string
		wantPublished int
		wantDiscarded int
	}{
		"interrupted inside the snapshot": {
			journal:       publishJournal{Phase: phaseSnapshotting},
			wantDiscarded: 1,
		},
		"interrupted after the pointer moved": {
			journal: publishJournal{Phase: phasePublishing, Consumed: true,
				Candidate: &storecontract.Candidate{Key: "k", Generation: "next"}},
			current: "next",
		},
		"interrupted before the pointer moved": {
			journal: publishJournal{Phase: phasePublishing, Consumed: true,
				Candidate: &storecontract.Candidate{Key: "k", Generation: "next"}},
			current:       "old",
			wantPublished: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			first := &fakeCacheStore{current: tc.current}
			service, token, instance := scopedServiceIn(t, dir, provider.TrustUntrusted,
				defaultBranchCache(), first)
			attachAndCommit(t, service, token)
			if err := service.SettleCompleted(t.Context(), instance, true, publishingAuthority()); err != nil {
				t.Fatalf("SettleCompleted: %v", err)
			}
			if err := service.Close(t.Context(), instance); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// THE CRASH: the journal the dead process left, written as it would
			// have been, and a new process reading it from disk.
			session := service.sessionOf(instance)
			session.mu.Lock()
			session.slots[0].Journal = &tc.journal
			session.slots[0].Volume.Generation = "old"
			if err := service.persistSession(session); err != nil {
				t.Fatalf("persist: %v", err)
			}
			session.mu.Unlock()

			after := &fakeCacheStore{current: tc.current}
			restarted, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", dir,
				after, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("restart: %v", err)
			}
			if err := restarted.RetryClosed(t.Context()); err != nil {
				t.Fatalf("RetryClosed: %v", err)
			}

			if after.snapshots != 0 || after.published != tc.wantPublished ||
				after.discarded != tc.wantDiscarded {
				t.Fatalf("snapshots/publications/discards = %d/%d/%d, want 0/%d/%d",
					after.snapshots, after.published, after.discarded, tc.wantPublished,
					tc.wantDiscarded)
			}
			if restarted.sessionOf(instance) != nil {
				t.Error("the session outlived a publication that had nothing left to do")
			}
		})
	}
}

// A REDELIVERED COMPLETION NEVER WAITS BEHIND STORAGE. A session already
// closing may be held for as long as storage takes, and the completion that
// arrives meanwhile records nothing and returns.
func TestASettlementForAClosingSessionDoesNotWait(t *testing.T) {
	t.Parallel()

	service, _, instance := scopedService(t, provider.TrustUntrusted, defaultBranchCache(),
		&fakeAuthority{authority: publishingAuthority()}, &fakeCacheStore{})
	session := service.sessionOf(instance)
	session.closing.Store(true)
	session.mu.Lock()
	defer session.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.SettleCompleted(ctx, instance, true, publishingAuthority()); err != nil {
		t.Fatalf("SettleCompleted on a closing session = %v, want nil at once", err)
	}
}

// THE RUNNER HANDS A COMPLETION'S AUTHORITY TO THE CACHE, through the path a
// completed destroy takes, and a grant naming another lease publishes nothing.
// Driven through Runner.DestroyCompleted, so a runner that stopped passing the
// authority on fails here rather than in a publication that silently never
// happens.
func TestTheRunnerSettlesACompletionWithItsAuthority(t *testing.T) {
	for name, publishes := range map[string]bool{"this lease": true, "another lease": false} {
		t.Run(name, func(t *testing.T) {
			p := &fakeProvider{kind: config.ProviderDocker}
			storage := &fakeCacheStore{}
			service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment",
				t.TempDir(), storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewCacheService: %v", err)
			}
			a, host := newAllocatorWithHost(t)
			runner := New(a, host, &fakeJIT{setID: 7}, p, nil, WithCacheService(service))
			lease := assignedLease(t, a)
			spec := dockerSpec()
			spec.Cache = defaultBranchCache()
			if err := runner.Launch(t.Context(), lease, spec, Job{RequestID: 11}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			authority := publishingAuthority()
			authority.LeaseID = lease.ID
			if !publishes {
				authority.LeaseID = "another"
			}
			recheck := publishingAuthority()
			recheck.LeaseID = lease.ID
			service.SetAuthorityReader(&fakeAuthority{authority: recheck})

			attachAndCommit(t, service, p.launched[0].CacheToken)
			if err := runner.DestroyCompleted(t.Context(), 11, "succeeded", authority); err != nil {
				t.Fatalf("DestroyCompleted: %v", err)
			}
			if err := service.RetryClosed(t.Context()); err != nil {
				t.Fatalf("RetryClosed: %v", err)
			}

			if got := storage.published == 1; got != publishes {
				t.Fatalf("published %d, want publication %t", storage.published, publishes)
			}
		})
	}
}

// sizedStore reports every clone as the given size.
type sizedStore struct {
	*fakeCacheStore
	size int64
}

func (s sizedStore) SizeOf(context.Context, storecontract.Volume) (int64, error) {
	return s.size, nil
}

// A GENERATION LARGER THAN THE TIER ALLOWS IS NOT HANDED OUT: the clone is
// discarded and the job starts cold at the size it asked for.
func TestAnOversizedBaselineStartsCold(t *testing.T) {
	t.Parallel()

	spec := defaultBranchCache()
	spec.StickyDisks.MaxSize = 20 * config.GiB
	storage := &fakeCacheStore{current: "big"}
	service, token, _ := scopedService(t, provider.TrustUntrusted, spec, nil,
		sizedStore{fakeCacheStore: storage, size: 50 << 30})

	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "npm", "size_bytes": int64(40 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}
	if storage.cloned != 1 || storage.discarded != 1 || storage.created != 1 {
		t.Fatalf("clone/discard/create = %d/%d/%d, want the oversized clone replaced",
			storage.cloned, storage.discarded, storage.created)
	}
	if len(storage.createdSizes) != 1 || storage.createdSizes[0] != 20<<30 {
		t.Errorf("created %v, want the request clamped to the tier's 20GiB", storage.createdSizes)
	}
}

// PUBLICATION OFF PUBLISHES NOTHING, even from a trusted pool, and a disabled
// cache attaches nothing.
func TestOffAndDisabledCachesPublishAndAttachNothing(t *testing.T) {
	t.Parallel()

	off := config.Tier{Cache: &config.TierCache{Publish: config.CachePublishOff}}.EffectiveCache()
	storage := &fakeCacheStore{}
	service, token, _ := scopedService(t, provider.TrustTrusted, &off, nil, storage)
	body := attachAndCommit(t, service, token)
	if storage.published != 0 || storage.discarded != 1 || body["reason"] != "publication is off" {
		t.Fatalf("off: published %d, discarded %d, answered %v", storage.published,
			storage.discarded, body)
	}

	disabled := false
	spec := config.Tier{Cache: &config.TierCache{
		StickyDisks: &config.CacheToggle{Enabled: &disabled}}}.EffectiveCache()
	storage = &fakeCacheStore{}
	service, token, _ = scopedService(t, provider.TrustTrusted, &spec, nil, storage)
	attached := cacheRequest(t, service, token, "/v1/volumes", map[string]any{
		"key": "npm", "size_bytes": int64(1 << 30),
	})
	if attached.Code != http.StatusForbidden || len(storage.keys) != 0 {
		t.Fatalf("disabled sticky disks: status %d, keys %v", attached.Code, storage.keys)
	}
}
