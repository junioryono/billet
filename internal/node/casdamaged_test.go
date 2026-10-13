package node

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// A DAMAGED GENERATION STARTS OVER WITHOUT ANYONE: the first job whose clone of
// it will not mount, and whose store proves the generation itself damaged,
// starts an empty volume in its place, and what that job publishes replaces it.
func TestADamagedGenerationStartsOverAndIsReplaced(t *testing.T) {
	t.Parallel()

	for name, damageErr := range map[string]error{
		"proved":                        nil,
		"proved, then the unmap failed": errors.New("unmap /dev/rbd3: (5) Input/output error"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testADamagedGenerationStartsOver(t, damageErr)
		})
	}
}

func testADamagedGenerationStartsOver(t *testing.T, damageErr error) {
	t.Helper()

	storage := &fakeCacheStore{
		current: "damaged", damagedGenerations: map[string]bool{"damaged": true}, damageErr: damageErr,
	}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	volumes.damaged = map[string]bool{"/dev/rbd8": true}
	session := service.sessionOf(instance)
	session.mu.Lock()
	hv, err := service.casVolume(t.Context(), session, config.CacheGo)
	session.mu.Unlock()
	if err != nil {
		t.Fatalf("casVolume over a damaged generation: %v", err)
	}
	if !slices.Equal(storage.damageChecks, []string{"damaged"}) {
		t.Fatalf("the store was asked about %v, want the generation the clone came from", storage.damageChecks)
	}
	if !slices.Equal(storage.damageKeys, []string{hv.Volume.Key}) || hv.Volume.Key != storage.keys[0] {
		t.Fatalf("the store was asked about keys %v, want the session's %q", storage.damageKeys, storage.keys[0])
	}
	if hv.Supersedes != "damaged" || hv.Volume.Device != "/dev/rbd7" || !volumes.fresh["/dev/rbd7"] {
		t.Fatalf("supersedes %q on %s (fresh %v); want a fresh volume in place of the damaged generation",
			hv.Supersedes, hv.Volume.Device, volumes.fresh["/dev/rbd7"])
	}
	if len(storage.discardedVolumes) != 1 || storage.discardedVolumes[0].Device != "/dev/rbd8" {
		t.Fatalf("discarded %+v, want the clone of the damaged generation", storage.discardedVolumes)
	}
	if code := putObject(t, service, token, "after the damage"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.current != "next" || len(storage.publishExpected) != 1 ||
		storage.publishExpected[0] != "damaged" || storage.cloned != 1 {
		t.Fatalf("current %q published over %v after %d clones; want the fresh volume over the damaged generation",
			storage.current, storage.publishExpected, storage.cloned)
	}
}

// uncheckedStore is a store that offers no generation check.
type uncheckedStore struct{ storecontract.Store }

// A GENERATION IS STARTED OVER ONLY ON THE STORE'S PROOF ABOUT IT: a job's copy
// that will not mount while the generation is sound, a check that could not
// tell, and a store that cannot check all keep the generation and leave the
// clone to the session's cleanup.
func TestAGenerationIsKeptUnlessTheStoreProvesItDamaged(t *testing.T) {
	t.Parallel()

	couldNotTell := errors.New("the check could not run")
	for _, tc := range []struct {
		name      string
		damageErr error
		unchecked bool
	}{
		{name: "a damaged copy of a sound generation"},
		{name: "a check that could not tell", damageErr: couldNotTell},
		{name: "a store that cannot check", unchecked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{current: "published", damageErr: tc.damageErr}
			service, volumes, _, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			volumes.damaged = map[string]bool{"/dev/rbd8": true}
			if tc.unchecked {
				service.store = uncheckedStore{storage}
			}
			session := service.sessionOf(instance)
			session.mu.Lock()
			_, err := service.casVolume(t.Context(), session, config.CacheGo)
			hv := session.hosts[config.CacheGo]
			session.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), "probably corrupted filesystem") {
				t.Fatalf("casVolume = %v, want the mount's failure", err)
			}
			if tc.damageErr != nil && !errors.Is(err, tc.damageErr) {
				t.Errorf("casVolume = %v, which drops what the check could not tell", err)
			}
			if tc.unchecked && !strings.Contains(err.Error(), "cannot check") {
				t.Errorf("casVolume = %v, which does not say the store cannot check", err)
			}
			if want := !tc.unchecked; (len(storage.damageChecks) == 1) != want {
				t.Errorf("the store was asked %v; asked: %v", storage.damageChecks, want)
			}
			if storage.created != 0 || storage.discarded != 0 || hv == nil || hv.Supersedes != "" {
				t.Fatalf("created %d, discarded %d, record %+v; want the generation and clone kept",
					storage.created, storage.discarded, hv)
			}
		})
	}
}

// A MERGE INTO A DAMAGED GENERATION PUBLISHES THE JOB'S OWN VOLUME IN ITS PLACE:
// its own volume is unmounted before the snapshot, and the clone of the damaged
// generation, not the job's volume, is what is discarded.
func TestAMergeIntoADamagedGenerationPublishesTheJobsVolumeInItsPlace(t *testing.T) {
	t.Parallel()

	for name, damageErr := range map[string]error{
		"proved":                        nil,
		"proved, then the unmap failed": errors.New("unmap /dev/rbd3: (5) Input/output error"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testAMergeIntoADamagedGeneration(t, damageErr)
		})
	}
}

func testAMergeIntoADamagedGeneration(t *testing.T, damageErr error) {
	t.Helper()

	storage := &fakeCacheStore{damageErr: damageErr}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "ours"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	session := service.sessionOf(instance)
	source := service.casMountPath(session, config.CacheGo) + ".source"
	storage.current = "damaged"
	storage.damagedGenerations = map[string]bool{"damaged": true}
	volumes.damaged = map[string]bool{"/dev/rbd8": true}
	var sourceMountedAtSnapshot []bool
	storage.snapshotHook = func() {
		mounted, err := volumes.Mounted(t.Context(), source)
		if err != nil {
			t.Errorf("Mounted(%s): %v", source, err)
		}
		sourceMountedAtSnapshot = append(sourceMountedAtSnapshot, mounted)
	}
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.published != 1 || storage.publishExpected[0] != "damaged" || storage.current != "next" {
		t.Fatalf("published %d over %v, current %q; want the job's volume over the damaged generation",
			storage.published, storage.publishExpected, storage.current)
	}
	if !slices.Equal(storage.damageChecks, []string{"damaged"}) {
		t.Fatalf("the store was asked about %v, want the newest generation", storage.damageChecks)
	}
	if !slices.Equal(storage.damageKeys, []string{snapshotKey(storage)}) {
		t.Fatalf("the store was asked about keys %v, want the session's %q", storage.damageKeys, snapshotKey(storage))
	}
	snapshotted := storage.snapshotVolumes[len(storage.snapshotVolumes)-1]
	if snapshotted.Device != "/dev/rbd7" {
		t.Fatalf("snapshotted %s, want the job's own volume", snapshotted.Device)
	}
	if !slices.Equal(sourceMountedAtSnapshot, []bool{false}) {
		t.Fatalf("the job's volume was mounted at the snapshot: %v", sourceMountedAtSnapshot)
	}
	if len(storage.discardedVolumes) != 1 || storage.discardedVolumes[0].Device != "/dev/rbd8" {
		t.Fatalf("discarded %+v, want the clone of the damaged generation alone", storage.discardedVolumes)
	}
}

// snapshotKey is the key of the volume the store last snapshotted.
func snapshotKey(storage *fakeCacheStore) string {
	return storage.snapshotVolumes[len(storage.snapshotVolumes)-1].Key
}
