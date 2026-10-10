package nodeclient_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// guestPlane registers at version and keeps every guest report it is sent,
// with the path it arrived on.
type guestPlane struct {
	version int
	mu      sync.Mutex
	paths   []string
	got     []nodeapi.GuestReportRequest
}

func (p *guestPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	var req nodeapi.GuestReportRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}
	p.mu.Lock()
	p.paths, p.got = append(p.paths, r.Method+" "+r.URL.Path), append(p.got, req)
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func aGuestReport() alloc.GuestReport {
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	return alloc.GuestReport{
		AgentVersion: "billet-agent/0.1.0", Schema: 1, Codec: alloc.GuestReportCodec,
		Data: []byte{0, 1, 2, 255}, Accepted: 12, Refused: 1, DroppedBytes: 3,
		Hello: true, FinalSeen: true, FirstReceived: first, LastReceived: first.Add(time.Minute),
	}
}

func registeredAt(t *testing.T, plane http.Handler) *nodeclient.Client {
	t.Helper()

	srv := httptest.NewServer(plane)
	t.Cleanup(srv.Close)
	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("register: %v", err)
	}

	return c
}

// AT THE VERSION, THE REPORT IS SENT ONCE, to the lease's guest route, with its
// epoch and every field as the caller gave it.
func TestAGuestReportIsSentToAPlaneAtItsVersion(t *testing.T) {
	t.Parallel()

	// LITERAL VERSIONS, so moving the constant cannot move the boundary the
	// fleet already runs: 28 is the last wire without the route.
	plane := &guestPlane{version: 29}
	c := registeredAt(t, plane)

	report := aGuestReport()
	if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.got) != 1 || plane.paths[0] != "POST /v1/nodes/n1/leases/l1/guest" {
		t.Fatalf("the plane was sent %v, want one POST to /v1/nodes/n1/leases/l1/guest", plane.paths)
	}
	got := plane.got[0]
	if got.Epoch != 7 || got.Report.AgentVersion != report.AgentVersion ||
		!bytes.Equal(got.Report.Data, report.Data) || got.Report.Accepted != report.Accepted ||
		!got.Report.LastReceived.Equal(report.LastReceived) {
		t.Fatalf("the plane was sent %+v, want %+v at epoch 7", got, report)
	}
}

// BELOW THE VERSION THE NODE SENDS NOTHING, the call is success, and that is
// said once in the node's log rather than once a job: the route does not exist
// on that plane, and what is lost is the report, never a job.
//
// NOT PARALLEL: it reads what reaches the process's default logger.
func TestAGuestReportIsNotSentToAPlaneTooOldForIt(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	plane := &versionedPlane{version: 28}
	c := registeredAt(t, plane)
	if got := c.WireVersion(); got != 28 {
		t.Fatalf("negotiated %d, want the older plane's 28", got)
	}

	for range 3 {
		if err := c.RecordGuestReport(t.Context(), "l1", 7, aGuestReport()); err != nil {
			t.Fatalf("RecordGuestReport against a wire-28 plane = %v, want nil", err)
		}
	}
	if n := plane.others.Load(); n != 0 {
		t.Errorf("the node sent %d request(s) to a plane without the route, want none", n)
	}
	if n := strings.Count(logged.String(), "does not carry guest reports"); n != 1 {
		t.Errorf("the dropped report was logged %d times over three jobs, want once:\n%s", n, logged.String())
	}
}

// WITH NO WIRE NEGOTIATED, WHETHER THE PLANE TAKES A REPORT CANNOT BE TOLD, and
// that is ErrUnregistered for the caller to retry after registering, never a
// report dropped as if the plane were too old.
func TestAGuestReportBeforeRegistrationIsUnregisteredNotDropped(t *testing.T) {
	t.Parallel()

	plane := &guestPlane{version: nodeapi.VersionGuestReport}
	srv := httptest.NewServer(plane)
	t.Cleanup(srv.Close)
	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.RecordGuestReport(t.Context(), "l1", 7, aGuestReport()); !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("RecordGuestReport before registering = %v, want ErrUnregistered", err)
	}
	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.paths) != 0 {
		t.Fatalf("an unregistered node sent %v", plane.paths)
	}
}

// THE ARRIVAL TIMES CROSS THE WIRE IN UTC, as the same instants: JSON spells a
// time in its own zone, so a zone that carries an instant past the year 9999
// would not encode, and an offset with seconds in it would lose them.
func TestAGuestReportsArrivalsCrossTheWireInUTC(t *testing.T) {
	t.Parallel()

	plane := &guestPlane{version: 29}
	c := registeredAt(t, plane)

	report := aGuestReport()
	ahead := time.FixedZone("ahead", 60*60)
	odd := time.FixedZone("odd", 5*60*60+30*60+17)
	report.FirstReceived = time.Date(9999, 12, 31, 22, 0, 0, 0, time.UTC).In(odd)
	report.LastReceived = time.Date(9999, 12, 31, 23, 30, 0, 5, time.UTC).In(ahead)
	if err := report.Validate(); err != nil {
		t.Fatalf("the report under test does not validate: %v", err)
	}
	if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	if report.FirstReceived.Location() != odd || report.LastReceived.Location() != ahead {
		t.Error("RecordGuestReport changed the caller's report")
	}

	plane.mu.Lock()
	defer plane.mu.Unlock()
	if len(plane.got) != 1 {
		t.Fatalf("the plane was sent %d reports, want 1", len(plane.got))
	}
	got := plane.got[0].Report
	if !got.FirstReceived.Equal(report.FirstReceived) || !got.LastReceived.Equal(report.LastReceived) {
		t.Fatalf("the plane was sent arrivals %s to %s, want the instants %s to %s",
			got.FirstReceived, got.LastReceived, report.FirstReceived, report.LastReceived)
	}
}
