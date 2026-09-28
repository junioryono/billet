package nodeclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// versionedPlane answers a registration at a chosen version and everything else
// with a bare 404, which is what a control plane from before a route existed
// does.
type versionedPlane struct {
	version int
	others  atomic.Int64
}

func (p *versionedPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/register" {
		p.others.Add(1)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
		Version: p.version, LeaseTTLSeconds: 60, PollSeconds: 30,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// A NODE NEGOTIATED BELOW THE OBSERVATION VERSION SENDS NOTHING, and the call
// is success: the route does not exist on that plane, a bare 404 would read as
// a decode failure on every cache request, and what is lost is one diagnostic
// column rather than anything a job depends on.
func TestACacheObservationIsNotSentToAPlaneTooOldForIt(t *testing.T) {
	t.Parallel()

	plane := &versionedPlane{version: nodeapi.VersionCacheObservation - 1}
	srv := httptest.NewServer(plane)
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := c.WireVersion(); got != nodeapi.VersionCacheObservation-1 {
		t.Fatalf("negotiated %d, want the older plane's %d", got, nodeapi.VersionCacheObservation-1)
	}

	if err := c.RecordCacheObservation(t.Context(), "l1", 1,
		alloc.CacheObservation{ImageCache: alloc.ImageCacheCold}); err != nil {
		t.Fatalf("RecordCacheObservation against an older plane = %v, want nil", err)
	}

	if n := plane.others.Load(); n != 0 {
		t.Fatalf("the node sent %d request(s) to a plane that cannot answer them", n)
	}
}

// observationPlane registers at a chosen version and records every observation
// body it is sent.
type observationPlane struct {
	version int
	bodies  chan map[string]any
}

func (p *observationPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/register" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version: p.version, LeaseTTLSeconds: 60, PollSeconds: 30,
		}); err != nil {
			return
		}

		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)

		return
	}
	p.bodies <- body
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte("{}")); err != nil {
		return
	}
}

// THE BUILD CACHES GO ONLY TO A PLANE THAT KNOWS THEM. An older plane decodes
// strictly, so a body carrying them would lose the image and Actions halves
// too; one carrying nothing else is not sent at all.
func TestBuildCacheOutcomesAreNotSentToAPlaneTooOldForThem(t *testing.T) {
	t.Parallel()

	builds := alloc.BuildCaches{Git: alloc.BuildCacheWarm}
	for version, want := range map[int][]string{
		nodeapi.VersionCacheAuthority - 1: {"image_cache"},
		nodeapi.VersionCacheAuthority:     {"image_cache", "git_cache"},
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

		if err := c.RecordCacheObservation(t.Context(), "l1", 1,
			alloc.CacheObservation{ImageCache: alloc.ImageCacheCold, BuildCaches: builds}); err != nil {
			t.Fatalf("v%d: RecordCacheObservation: %v", version, err)
		}
		body := <-plane.bodies
		for _, field := range []string{"image_cache", "git_cache"} {
			if _, sent := body[field]; sent != slices.Contains(want, field) {
				t.Errorf("v%d: %s sent = %v, want %v (%v)", version, field, sent, !sent, body)
			}
		}

		if err := c.RecordCacheObservation(t.Context(), "l1", 1,
			alloc.CacheObservation{BuildCaches: builds}); err != nil {
			t.Fatalf("v%d: RecordCacheObservation: %v", version, err)
		}
		select {
		case body := <-plane.bodies:
			if version < nodeapi.VersionCacheAuthority {
				t.Errorf("v%d: an observation with nothing an older plane records was sent: %v",
					version, body)
			}
		default:
			if version >= nodeapi.VersionCacheAuthority {
				t.Errorf("v%d: a build-cache observation was not sent", version)
			}
		}
	}
}
