package nodeclient_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// egressPlane registers at version and keeps every egress report it is sent, decoding
// strictly as a real plane does.
type egressPlane struct {
	version int
	mu      sync.Mutex
	paths   []string
	got     []nodeapi.EgressReportRequest
}

func (p *egressPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/register" {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version: p.version, LeaseTTLSeconds: 60, PollSeconds: 30,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var req nodeapi.EgressReportRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	p.mu.Lock()
	p.paths, p.got = append(p.paths, r.Method+" "+r.URL.Path), append(p.got, req)
	p.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func anEgressReport() alloc.JobEgress {
	return alloc.JobEgress{
		Listed: true, Queries: 4, RefusedQueries: 1,
		RefusedNames:  []alloc.EgressRefusal{{Name: "evil.example", Count: 1}},
		Names:         []alloc.EgressNames{{Addr: "198.51.100.1", Names: []string{"github.com"}}},
		DeniedPackets: 2, OutsideResolverPackets: 1, IPv6Packets: 3,
		Denied: []alloc.EgressDenial{{Addr: "203.0.113.7", Protocol: "tcp", Port: 443, Packets: 2}},
	}
}

func TestAnEgressReportIsSentToAPlaneAtItsVersion(t *testing.T) {
	t.Parallel()

	plane := &egressPlane{version: nodeapi.VersionEgress}
	c := registeredAt(t, plane)

	if err := c.RecordLeaseEgress(t.Context(), "l1", 7, anEgressReport()); err != nil {
		t.Fatalf("RecordLeaseEgress: %v", err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()

	if len(plane.got) != 1 || plane.paths[0] != "POST /v1/nodes/n1/leases/l1/egress" {
		t.Fatalf("sent %v", plane.paths)
	}

	if plane.got[0].Epoch != 7 || !reflect.DeepEqual(plane.got[0].Report, anEgressReport()) {
		t.Fatalf("sent %+v", plane.got[0])
	}
}

// BELOW THE VERSION THE REPORT IS DROPPED AS SUCCESS, and said once: the route does not
// exist there, and what the pairing loses is the report and never a job. The literal is
// the last wire without it, so moving the constant cannot move the boundary.
func TestAnEgressReportIsNotSentToAPlaneTooOldForIt(t *testing.T) { //nolint:paralleltest // it replaces the default logger.
	var logged bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	plane := &versionedPlane{version: 38}
	c := registeredAt(t, plane)

	if got := c.WireVersion(); got != 38 {
		t.Fatalf("negotiated %d, want the older plane's 38", got)
	}

	for range 3 {
		if err := c.RecordLeaseEgress(t.Context(), "l1", 7, anEgressReport()); err != nil {
			t.Fatalf("RecordLeaseEgress against a wire-38 plane = %v, want nil", err)
		}
	}

	if n := plane.others.Load(); n != 0 {
		t.Errorf("the node sent %d request(s) to a plane without the route, want none", n)
	}

	if n := strings.Count(logged.String(), "does not carry egress reports"); n != 1 {
		t.Errorf("the dropped report was logged %d times over three jobs, want once:\n%s", n, logged.String())
	}
}

func TestAnEgressReportBeforeRegistrationIsUnregisteredNotDropped(t *testing.T) {
	t.Parallel()

	plane := &egressPlane{version: nodeapi.VersionEgress}
	srv := httptest.NewServer(plane)
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.RecordLeaseEgress(t.Context(), "l1", 7, anEgressReport()); !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("RecordLeaseEgress before registering = %v, want ErrUnregistered", err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()

	if len(plane.paths) != 0 {
		t.Fatalf("an unregistered node sent %v", plane.paths)
	}
}
