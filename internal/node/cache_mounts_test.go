package node

import (
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A RESTARTED NODE MOUNTS ITS GUESTS' CACHE VOLUMES AGAIN (#374). The unit has a
// mount namespace of its own, so the next process finds the session's record
// saying "mounted" over an empty directory. The volume is mounted again before
// anything is served, and one that cannot be is refused, never served from the
// directory beneath it.
func TestARestartedNodeMountsItsGuestsCacheVolumesAgain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		mountFail bool
	}{
		{name: "the volume mounts again"},
		{name: "the volume cannot be mounted", mountFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			before, volumes, token, _ := casService(t, provider.TrustTrusted, goBazelCache(), storage)
			body := "compiled before the restart"
			object := "/v1/cas/go/cas/" + digestOf(body)
			if got := casRequest(t, before, token, http.MethodPut, object, body); got.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
			}

			// THE PREVIOUS PROCESS'S NAMESPACE IS GONE, and its mount with it.
			mountPath := before.casMountPath(before.byToken[token], config.CacheGo)
			if err := os.Remove(mountPath); err != nil {
				t.Fatalf("take the mount away: %v", err)
			}

			after, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", before.rootState,
				storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewCacheService after the restart: %v", err)
			}
			after.actionIO = volumes
			if tc.mountFail {
				volumes.failSuffix = "/go"
			}
			after.RestoreMounts(t.Context())

			got := casRequest(t, after, token, http.MethodGet, object, "")
			if !tc.mountFail {
				if got.Code != http.StatusOK || got.Body.String() != body {
					t.Fatalf("the object written before the restart answered %d %q", got.Code, got.Body.String())
				}

				return
			}

			if got.Code == http.StatusOK {
				t.Fatalf("a volume that could not be mounted again was served: %q", got.Body.String())
			}
			late := "written after the restart"
			if got := casRequest(t, after, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(late), late); got.Code == http.StatusOK {
				t.Fatal("a write was accepted for a volume that is not mounted")
			}
			if _, err := os.Lstat(mountPath); !os.IsNotExist(err) {
				t.Errorf("something was written where the volume should be mounted: %v", err)
			}
		})
	}
}
