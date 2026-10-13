package node

import (
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A DAMAGED GENERATION STARTS OVER WITHOUT ANYONE: the first job whose clone of
// it will not mount, and whose check finds the filesystem damaged, starts an
// empty volume in its place, and what that job publishes replaces it.
func TestADamagedGenerationStartsOverAndIsReplaced(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{current: "damaged"}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	volumes.damaged = map[string]bool{"/dev/rbd8": true}
	session := service.sessionOf(instance)
	session.mu.Lock()
	hv, err := service.casVolume(t.Context(), session, config.CacheGo)
	session.mu.Unlock()
	if err != nil {
		t.Fatalf("casVolume over a damaged generation: %v", err)
	}
	if hv.Supersedes != "damaged" || hv.Volume.Device != "/dev/rbd7" || !volumes.fresh["/dev/rbd7"] {
		t.Fatalf("supersedes %q on %s (fresh %v); want a fresh volume in place of the damaged generation",
			hv.Supersedes, hv.Volume.Device, volumes.fresh["/dev/rbd7"])
	}
	if storage.discarded != 1 {
		t.Fatalf("discarded %d volumes, want the clone of the damaged generation", storage.discarded)
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

// A GENERATION IS STARTED OVER ONLY ON A CHECK'S PROOF: a mount that failed
// with the filesystem found clean, or with a check that could not tell, keeps
// the generation and the clone for the session's cleanup.
func TestAGenerationIsKeptUnlessTheCheckFindsItDamaged(t *testing.T) {
	t.Parallel()

	couldNotTell := errors.New("the check could not run")
	for name, checkErr := range map[string]error{"clean": nil, "could not tell": couldNotTell} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{current: "published"}
			service, volumes, _, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			volumes.failSuffix = "/go"
			volumes.checkErr = checkErr
			session := service.sessionOf(instance)
			session.mu.Lock()
			_, err := service.casVolume(t.Context(), session, config.CacheGo)
			hv := session.hosts[config.CacheGo]
			session.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), "the mount failed") {
				t.Fatalf("casVolume = %v, want the mount's failure", err)
			}
			if checkErr != nil && !errors.Is(err, checkErr) {
				t.Errorf("casVolume = %v, which drops what the check could not tell", err)
			}
			if len(volumes.checked) != 1 || volumes.checked[0] != "/dev/rbd8" {
				t.Errorf("checked %v, want the clone that failed to mount", volumes.checked)
			}
			if storage.created != 0 || storage.discarded != 0 || hv == nil || hv.Supersedes != "" {
				t.Fatalf("created %d, discarded %d, record %+v; want the generation and clone kept",
					storage.created, storage.discarded, hv)
			}
		})
	}
}

// A MERGE INTO A DAMAGED GENERATION PUBLISHES THE JOB'S OWN VOLUME IN ITS PLACE,
// discarding its clone of the damaged generation and unmounting its own volume
// before the snapshot.
func TestAMergeIntoADamagedGenerationPublishesTheJobsVolumeInItsPlace(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "ours"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.current = "damaged"
	volumes.damaged = map[string]bool{"/dev/rbd8": true}
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.published != 1 || storage.publishExpected[0] != "damaged" || storage.current != "next" {
		t.Fatalf("published %d over %v, current %q; want the job's volume over the damaged generation",
			storage.published, storage.publishExpected, storage.current)
	}
	snapshotted := storage.snapshotVolumes[len(storage.snapshotVolumes)-1]
	if snapshotted.Device != "/dev/rbd7" {
		t.Fatalf("snapshotted %s, want the job's own volume", snapshotted.Device)
	}
	if storage.discarded != 1 {
		t.Fatalf("discarded %d, want the clone of the damaged generation alone", storage.discarded)
	}
	for _, target := range volumes.targets {
		if mounted, err := volumes.Mounted(t.Context(), target); err != nil || mounted {
			t.Errorf("%s is still mounted (%v) after the publication", target, err)
		}
	}
}

// e2fsck's exit status decides: the uncorrected-errors bit is damage, 8 alone
// is a check that could not run, and a signal is no verdict.
func TestTheCheckReadsE2fsckExitStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		script  string
		damaged bool
		err     bool
	}{
		{"exit 0", false, false},
		{"exit 4", true, false},
		{"exit 12", true, false},
		{"exit 8", false, true},
		{"kill -9 $$", false, true},
	} {
		err := exec.CommandContext(t.Context(), "sh", "-c", tc.script).Run()
		damaged, verdictErr := e2fsckVerdict("/dev/rbd9", err, nil)
		if damaged != tc.damaged || (verdictErr != nil) != tc.err {
			t.Errorf("%q: damaged %v, error %v; want %v and an error %v",
				tc.script, damaged, verdictErr, tc.damaged, tc.err)
		}
	}
}
