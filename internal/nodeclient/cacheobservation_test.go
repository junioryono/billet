package nodeclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
