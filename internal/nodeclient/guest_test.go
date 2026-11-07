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
		Hello: true, FinalSeen: true, NodeRestarted: true, FirstReceived: first, LastReceived: first.Add(time.Minute),
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
	// EVERY FIELD, the arrivals as the instants they name.
	got := plane.got[0]
	sent := got.Report
	if !sent.FirstReceived.Equal(report.FirstReceived) || !sent.LastReceived.Equal(report.LastReceived) {
		t.Fatalf("the plane was sent arrivals %s to %s, want %s to %s", sent.FirstReceived, sent.LastReceived,
			report.FirstReceived, report.LastReceived)
	}
	sent.FirstReceived, sent.LastReceived = report.FirstReceived, report.LastReceived
	if got.Epoch != 7 || !reflect.DeepEqual(sent, report) {
		t.Fatalf("the plane was sent %+v at epoch %d, want %+v at epoch 7", sent, got.Epoch, report)
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

// THE ARRIVAL TIMES CROSS THE WIRE IN UTC, as the same instants, each case on
// its own: JSON spells a time in its own zone, so a zone that carries an
// instant past the year 9999 would not encode, an offset with seconds in it
// would lose them, and a zero time in another zone would arrive as a time.
func TestAGuestReportsArrivalsCrossTheWireInUTC(t *testing.T) {
	t.Parallel()

	ahead := time.FixedZone("ahead", 60*60)
	odd := time.FixedZone("odd", 5*60*60+30*60+17)
	for _, tc := range []struct {
		name        string
		first, last time.Time
	}{
		{"a zone that carries them past the year 9999",
			time.Date(9999, 12, 31, 22, 0, 0, 0, time.UTC).In(ahead),
			time.Date(9999, 12, 31, 23, 30, 0, 5, time.UTC).In(ahead)},
		{"an offset with seconds in it",
			time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).In(odd),
			time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC).In(odd)},
		{"no arrival, in a zone", time.Time{}.In(odd), time.Time{}.In(odd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plane := &guestPlane{version: 29}
			c := registeredAt(t, plane)
			report := aGuestReport()
			report.FirstReceived, report.LastReceived = tc.first, tc.last
			if err := report.Validate(); err != nil {
				t.Fatalf("the report under test does not validate: %v", err)
			}
			if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
				t.Fatalf("RecordGuestReport: %v", err)
			}
			if report.FirstReceived.Location() != tc.first.Location() ||
				report.LastReceived.Location() != tc.last.Location() {
				t.Error("RecordGuestReport changed the caller's report")
			}

			plane.mu.Lock()
			defer plane.mu.Unlock()
			if len(plane.got) != 1 {
				t.Fatalf("the plane was sent %d reports, want 1", len(plane.got))
			}
			got := plane.got[0].Report
			if !got.FirstReceived.Equal(tc.first) || !got.LastReceived.Equal(tc.last) ||
				got.FirstReceived.IsZero() != tc.first.IsZero() || got.LastReceived.IsZero() != tc.last.IsZero() {
				t.Fatalf("the plane was sent arrivals %s to %s, want the instants %s to %s",
					got.FirstReceived, got.LastReceived, tc.first, tc.last)
			}
		})
	}
}

// A REPORT'S TEST RESULTS REACH ONLY A PLANE THAT KNOWS THEM: an older plane's strict
// decoder would refuse the whole report over the field. LITERAL VERSIONS, so moving
// the constant cannot move the boundary the fleet already runs.
func TestATestResultsFieldReachesOnlyAPlaneThatKnowsIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		carried bool
	}{{37, false}, {38, true}} {
		plane := &guestPlane{version: tc.version}
		c := registeredAt(t, plane)

		report := aGuestReport()
		report.TestsCodec, report.Tests = alloc.TestResultsCodec, []byte{7}
		if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
			t.Fatalf("at wire %d: RecordGuestReport: %v", tc.version, err)
		}

		plane.mu.Lock()
		if len(plane.got) != 1 {
			plane.mu.Unlock()
			t.Fatalf("at wire %d the plane was sent %d reports, want the report without or with them",
				tc.version, len(plane.got))
		}
		sent := plane.got[0].Report
		plane.mu.Unlock()

		if got := len(sent.Tests) == 1 && sent.TestsCodec == alloc.TestResultsCodec; got != tc.carried {
			t.Errorf("at wire %d the plane was sent tests %v under codec %d, want carried %t", tc.version,
				sent.Tests, sent.TestsCodec, tc.carried)
		}
		if sent.Accepted != report.Accepted {
			t.Errorf("at wire %d the rest of the report did not arrive: %+v", tc.version, sent)
		}
		if report.TestsCodec != alloc.TestResultsCodec || len(report.Tests) != 1 {
			t.Errorf("at wire %d the caller's report was changed", tc.version)
		}
	}
}

// A LOG INDEX'S NAME COUNTS ONLY ON THE WIRE THAT CARRIES IT: a node below it ships nothing, which
// is what an older control plane needs of it. LITERAL VERSIONS, for the reason above.
func TestALogIndexIDCountsOnlyOnItsWire(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		want    string
	}{{42, ""}, {43, "index-a"}} {
		c := registeredAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{Version: tc.version, LeaseTTLSeconds: 60,
				PollSeconds: 30, LogIndexID: "index-a"}); err != nil {
				t.Errorf("answer: %v", err)
			}
		}))

		got, ok := c.LogIndexID()
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("at wire %d the index is %q, %t; want %q", tc.version, got, ok, tc.want)
		}
	}
}

// logsPlane answers the chunk route with a status and an error code, and keeps what it was sent.
type logsPlane struct {
	status int
	code   string
	mu     sync.Mutex
	got    []nodeapi.LogChunkRequest
}

func (p *logsPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/register" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{Version: nodeapi.Version, LeaseTTLSeconds: 60,
			PollSeconds: 30}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}

	if r.Method != http.MethodPost || r.URL.Path != "/v1/nodes/n1/logs" {
		http.NotFound(w, r)

		return
	}

	var req nodeapi.LogChunkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	p.mu.Lock()
	p.got = append(p.got, req)
	p.mu.Unlock()

	if p.status == 0 {
		w.WriteHeader(http.StatusNoContent)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(p.status)
	_ = json.NewEncoder(w).Encode(nodeapi.ErrorResponse{Code: p.code, Message: "no"}) //nolint:errcheck,errchkjson // a test's stand-in, whose reader fails on what it lacks
}

// A CHUNK REACHES ITS ROUTE AS GIVEN; THE PLANE'S REFUSAL SAYS DO NOT SEND THIS AGAIN, AND ANY OTHER
// FAILURE SAYS THE OPPOSITE, since one wrong answer loses a line and the other wedges a log.
func TestAChunkIsShippedAndItsRefusalToldApart(t *testing.T) {
	t.Parallel()

	plane := &logsPlane{}
	c := registeredAt(t, plane)

	refused, err := c.ShipLogChunk(t.Context(), "l1", 12, []byte{1, 2, 255})
	if err != nil || refused {
		t.Fatalf("an accepted chunk answered %t, %v", refused, err)
	}

	plane.mu.Lock()
	got := plane.got
	plane.mu.Unlock()
	if len(got) != 1 || got[0].Lease != "l1" || got[0].FirstLine != 12 || !bytes.Equal(got[0].Data, []byte{1, 2, 255}) {
		t.Errorf("the plane was sent %+v", got)
	}

	plane = &logsPlane{status: http.StatusBadRequest, code: nodeapi.CodeRefused}
	refused, err = registeredAt(t, plane).ShipLogChunk(t.Context(), "l1", 0, []byte{1})
	if err == nil || !refused {
		t.Errorf("a refusal answered %t, %v; want a refusal", refused, err)
	}

	plane = &logsPlane{status: http.StatusServiceUnavailable}
	refused, err = registeredAt(t, plane).ShipLogChunk(t.Context(), "l1", 0, []byte{1})
	if err == nil || refused {
		t.Errorf("a failure answered %t, %v; want could-not-tell", refused, err)
	}
}
