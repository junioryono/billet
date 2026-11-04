package nodeclient_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/lease"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// `bypassed` GOES ONLY TO A PLANE THAT KNOWS THE WORD: an older one validates an
// observation's words and would refuse all of it. Below the version the job's cache is
// said as unavailable, which is true as far as it goes.
func TestBypassedIsSaidAsUnavailableToAPlaneTooOldForIt(t *testing.T) {
	t.Parallel()

	for version, want := range map[int]string{
		nodeapi.VersionCacheBreaker - 1: "unavailable",
		nodeapi.VersionCacheBreaker:     "bypassed",
	} {
		plane := &observationPlane{version: version, bodies: make(chan map[string]any, 4)}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}

		if err := c.RecordCacheObservation(t.Context(), "l1", 1, alloc.CacheObservation{
			ImageCache:   alloc.ImageCacheBypassed,
			ActionsCache: alloc.ActionsCacheBypassed,
			BuildCaches:  alloc.BuildCaches{Sticky: alloc.BuildCacheBypassed, Go: alloc.BuildCacheWarm},
		}); err != nil {
			t.Fatalf("v%d: RecordCacheObservation: %v", version, err)
		}
		body := <-plane.bodies
		for _, field := range []string{"image_cache", "actions_cache", "sticky_cache"} {
			if body[field] != want {
				t.Errorf("v%d: %s = %v, want %s", version, field, body[field], want)
			}
		}
		if body["go_cache"] != "warm" {
			t.Errorf("v%d: another cache's word moved: %v", version, body["go_cache"])
		}
	}
}

// A PLANE BELOW THE VERSION HAS NO SUCH ROUTE, so it is not asked and loses one
// diagnostic; one at it hears the report whole.
func TestABreakersReportGoesOnlyToAPlaneWithTheRoute(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	report := lease.CacheStoreReport{
		Store: "ceph", State: lease.CacheStoreOpen, Since: since, Failures: 3, Reason: lease.CacheStoreTimeout,
	}

	for version, wantSent := range map[int]bool{
		nodeapi.VersionCacheBreaker - 1: false,
		nodeapi.VersionCacheBreaker:     true,
	} {
		plane := &observationPlane{version: version, bodies: make(chan map[string]any, 4)}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}

		// BEFORE A REGISTRATION THERE IS NO WIRE TO ASK, and it is said so, not lost.
		if err := c.ReportCacheStore(t.Context(), report); err == nil {
			t.Fatalf("v%d: a report before any registration claimed to land", version)
		}

		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := c.ReportCacheStore(t.Context(), report); err != nil {
			t.Fatalf("v%d: ReportCacheStore: %v", version, err)
		}

		select {
		case body := <-plane.bodies:
			if !wantSent {
				t.Errorf("v%d: a plane with no such route was asked: %v", version, body)

				continue
			}
			for field, want := range map[string]any{
				"store": "ceph", "state": "open", "since": "2026-11-04T08:00:00Z",
				"failures": float64(3), "reason": "timeout",
			} {
				if body[field] != want {
					t.Errorf("%s = %v, want %v", field, body[field], want)
				}
			}
		default:
			if wantSent {
				t.Errorf("v%d: the report was not sent", version)
			}
		}
	}
}
