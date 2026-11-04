package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// provingStore answers where a volume is mapped now, as the Ceph store does.
type provingStore struct {
	*fakeCacheStore

	device string
	err    error
}

func (p *provingStore) MappedDevice(context.Context, storecontract.Volume) (string, error) {
	return p.device, p.err
}

// A VOLUME THAT IS GONE ENDS ITS PUBLICATION AT ONCE, and is discarded: an earlier
// failure took it, so asking again until the window ended only held the session
// open and logged it on every pass. Any other failure is asked again.
func TestASnapshotOfAVolumeThatIsGoneEndsThePublication(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err           error
		wantSnapshots int
		wantDiscarded int
		wantOpen      bool
	}{
		"gone": {
			err:           fmt.Errorf("ceph: find the device: %w", storecontract.ErrVolumeGone),
			wantSnapshots: 1, wantDiscarded: 1,
		},
		"could not tell": {
			err:           errors.New("ceph: list the mapped rbd devices: the monitors did not answer"),
			wantSnapshots: 2, wantOpen: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{snapshotErr: tc.err}
			service, token, instance := scopedServiceIn(t, t.TempDir(), provider.TrustUntrusted,
				defaultBranchCache(), storage)
			attachAndCommit(t, service, token)
			if err := service.SettleCompleted(t.Context(), instance, true, publishingAuthority()); err != nil {
				t.Fatalf("SettleCompleted: %v", err)
			}
			if err := service.Close(t.Context(), instance); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Twice: a publication still owed is asked for again, one ended is not.
			for range 2 {
				// A session still owed its publication reports that as an error.
				if err := service.RetryClosed(t.Context()); err != nil && !tc.wantOpen {
					t.Fatalf("RetryClosed: %v", err)
				}
			}

			if storage.snapshots != tc.wantSnapshots || storage.discarded != tc.wantDiscarded {
				t.Errorf("snapshots/discards = %d/%d, want %d/%d", storage.snapshots, storage.discarded,
					tc.wantSnapshots, tc.wantDiscarded)
			}
			if storage.published != 0 {
				t.Errorf("published %d, want nothing", storage.published)
			}
			if open := service.sessionOf(instance) != nil; open != tc.wantOpen {
				t.Errorf("session open = %t, want %t", open, tc.wantOpen)
			}
		})
	}
}

// A RECOVERED VOLUME IS MOUNTED FROM WHERE IT IS MAPPED NOW. Its recorded number
// outlives a reboot, and mounting whatever holds it would serve another job's
// disk as this job's; one that cannot be proved is refused as a miss.
func TestARecoveredVolumeIsMountedFromWhereItIsMappedNow(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		device    string
		err       error
		wantMount bool
	}{
		"its number moved": {device: "/dev/rbd-moved", wantMount: true},
		"nothing maps it":  {err: storecontract.ErrVolumeGone},
		"could not tell":   {err: errors.New("the monitors did not answer")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			before, volumes, token, _ := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			body := "compiled before the restart"
			object := "/v1/cas/go/cas/" + digestOf(body)
			if got := casRequest(t, before, token, http.MethodPut, object, body); got.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
			}
			mountPath := before.casMountPath(before.byToken[token], config.CacheGo)
			recorded, err := os.Readlink(mountPath)
			if err != nil {
				t.Fatalf("read the first mount: %v", err)
			}
			if err := os.Remove(mountPath); err != nil {
				t.Fatalf("take the mount away: %v", err)
			}
			mounts := len(volumes.targets)

			after, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", before.rootState,
				&provingStore{fakeCacheStore: storage, device: tc.device, err: tc.err},
				&fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewCacheService after the restart: %v", err)
			}
			after.actionIO = volumes
			after.RestoreMounts(t.Context())

			if !tc.wantMount {
				if len(volumes.targets) != mounts {
					t.Fatalf("mounted %v, though the volume's device could not be proved",
						volumes.targets[mounts:])
				}
				if got := casRequest(t, after, token, http.MethodGet, object, ""); got.Code == http.StatusOK {
					t.Fatalf("a volume that was not mounted served %q", got.Body.String())
				}

				return
			}

			now, err := os.Readlink(mountPath)
			if err != nil {
				t.Fatalf("the volume was not mounted again: %v", err)
			}
			if now == recorded || now != volumes.dir(tc.device) {
				t.Fatalf("mounted %s, want the device it is mapped to now, %s", now, volumes.dir(tc.device))
			}
		})
	}
}

// THE SOURCE OF A MERGE IS PROVED BEFORE IT IS MOUNTED. Its objects are copied
// into a generation this repository publishes, so a number that had gone to
// another job's volume would carry that job's objects across.
func TestAMergeDoesNotMountASourceItCannotProve(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "ours"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.current = "theirs"
	service.store = &provingStore{fakeCacheStore: storage, err: storecontract.ErrVolumeGone}
	finish(t, service, instance)
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}

	if slices.ContainsFunc(volumes.targets, func(target string) bool {
		return strings.HasSuffix(target, ".source")
	}) {
		t.Errorf("mounted a merge's source from an unproved device: %v", volumes.targets)
	}
	if storage.published != 0 || storage.current != "theirs" {
		t.Errorf("published %d with current %q, want nothing published", storage.published, storage.current)
	}
}
