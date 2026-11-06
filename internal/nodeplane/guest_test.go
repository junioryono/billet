package nodeplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

func aGuestReport() alloc.GuestReport {
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	return alloc.GuestReport{
		AgentVersion: "billet-agent/0.1.0", Schema: 1, Codec: alloc.GuestReportCodec,
		Data: []byte{0, 1, 2, 255}, Accepted: 12, Refused: 1, DroppedBytes: 3,
		Hello: true, FinalSeen: true, NodeRestarted: true, FirstReceived: first, LastReceived: first.Add(time.Minute),
	}
}

// sameReport is reflect.DeepEqual with the arrival times compared as instants,
// which is all JSON keeps of them.
func sameReport(got, want alloc.GuestReport) bool {
	if !got.FirstReceived.Equal(want.FirstReceived) || !got.LastReceived.Equal(want.LastReceived) {
		return false
	}
	got.FirstReceived, got.LastReceived = want.FirstReceived, want.LastReceived

	return reflect.DeepEqual(got, want)
}

// postAs sends body to path as the node process incarnation names, and
// returns the status and the refusal, if any.
func postAs(t *testing.T, base, path, incarnation string, body []byte) (int, nodeapi.ErrorResponse) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req.Header.Set(nodeapi.HeaderIncarnation, incarnation)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()

	var refusal nodeapi.ErrorResponse
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
			t.Fatalf("decode the answer to %s (status %d): %v", path, resp.StatusCode, err)
		}
	}

	return resp.StatusCode, refusal
}

// registerAt registers n1 as incarnation at exactly one wire version.
func registerAt(t *testing.T, base string, version int, incarnation string) {
	t.Helper()

	body, err := json.Marshal(nodeapi.RegisterRequest{
		MinVersion: version, Version: version, Incarnation: incarnation,
		Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if status, refusal := postAs(t, base, "/v1/register", incarnation, body); status != http.StatusOK {
		t.Fatalf("register at wire %d answered %d %+v", version, status, refusal)
	}
}

// A GUEST REPORT CROSSES THE NODE WIRE WITH ITS EPOCH, its data and every
// count as the node sent them, so the ledger fences it like every other write
// from the process holding the lease.
func TestAGuestReportCrossesTheNodeWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	report := aGuestReport()
	if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.guests) != 1 {
		t.Fatalf("the ledger was asked to record %d guest reports, want 1", len(store.guests))
	}
	got := store.guests[0]
	if got.lease != "l1" || got.epoch != 7 || !sameReport(got.report, report) {
		t.Fatalf("the ledger was asked to record %+v, want %+v for l1 at epoch 7", got, report)
	}
}

// A PAIRING THAT NEGOTIATED BELOW THE GUEST REPORT'S VERSION IS REFUSED,
// naming both versions, and one at the version is kept: its wire does not
// carry the report, so the plane records nothing rather than something that
// wire never said.
func TestAGuestReportFromAWireThatDoesNotCarryItIsRefused(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(nodeapi.GuestReportRequest{Epoch: 7, Report: aGuestReport()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, tc := range []struct {
		version int
		status  int
		kept    int
	}{
		// LITERAL VERSIONS, so moving the constant cannot move the boundary
		// the fleet already runs: 28 is the last wire without the route.
		{28, http.StatusBadRequest, 0},
		{29, http.StatusNoContent, 1},
	} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d", tc.version)
		registerAt(t, base, tc.version, incarnation)

		status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/guest", incarnation, body)
		if status != tc.status {
			t.Fatalf("a guest report at wire %d answered %d %+v, want %d", tc.version, status, refusal, tc.status)
		}
		if tc.status != http.StatusNoContent {
			want := fmt.Sprintf("needs wire 29 and this node negotiated %d", tc.version)
			if refusal.Code != nodeapi.CodeRefused || !strings.Contains(refusal.Message, want) {
				t.Errorf("at wire %d the refusal is %+v, want %q saying %q", tc.version, refusal,
					nodeapi.CodeRefused, want)
			}
		}

		store.mu.Lock()
		kept := len(store.guests)
		store.mu.Unlock()
		if kept != tc.kept {
			t.Errorf("at wire %d the ledger was asked to record %d guest reports, want %d", tc.version, kept, tc.kept)
		}
	}
}

// A REPORT THIS CONTROL PLANE CANNOT KEEP IS REFUSED AT THE BOUNDARY and never
// reaches the ledger. Every case is the one accepted body with one thing
// broken, and says which refusal it expects.
func TestAnUnkeepableGuestReportIsRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(nodeapi.GuestReportRequest{Epoch: 7, Report: aGuestReport()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	accepted := string(raw)
	oversized, err := json.Marshal(make([]byte, alloc.MaxGuestReportBytes+1))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"the accepted body": {accepted, ""},
		"an unknown codec": {strings.Replace(accepted, `"codec":1`, `"codec":2`, 1),
			"codec 2 is not one"},
		"data over its bound": {strings.Replace(accepted, `"data":"AAEC/w=="`, `"data":`+string(oversized), 1),
			"over the 262144 byte bound"},
		"a version a reader would print as two lines": {strings.Replace(accepted, `"billet-agent/0.1.0"`,
			`"1.0\nforged"`, 1), "not printable ASCII"},
		"arrivals out of order": {strings.Replace(accepted, `"last_received":"2026-10-09T12:01:00Z"`,
			`"last_received":"2026-10-09T11:00:00Z"`, 1), "arrived after its last"},
		"a field this plane does not know": {strings.Replace(accepted, `"epoch":7`, `"epoch":7,"verdict":"ok"`, 1),
			`unknown field "verdict"`},
		"a second value after the first": {accepted + `{}`, "more than one JSON value"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{}
			_, base := serve(t, store)
			c := dial(t, base)

			status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/guest", c.Incarnation(), []byte(tc.body))
			store.mu.Lock()
			kept := len(store.guests)
			store.mu.Unlock()
			if tc.want == "" {
				if status != http.StatusNoContent || kept != 1 {
					t.Fatalf("the accepted body answered %d %+v and kept %d, want 204 and one kept",
						status, refusal, kept)
				}

				return
			}
			if status != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused ||
				!strings.Contains(refusal.Message, tc.want) {
				t.Fatalf("answered %d %q (%s), want 400 %q saying %q", status, refusal.Code, refusal.Message,
					nodeapi.CodeRefused, tc.want)
			}
			if kept != 0 {
				t.Fatalf("the ledger was asked to record %d guest reports from a refused request", kept)
			}
		})
	}
}

// THE LARGEST REPORT IS ONE REQUEST: Data at its bound and the longest version
// fit the wire's 1 MiB body once base64 is applied, and are kept.
func TestTheLargestGuestReportFitsOneRequest(t *testing.T) {
	t.Parallel()

	report := aGuestReport()
	report.Data = bytes.Repeat([]byte{0xff}, alloc.MaxGuestReportBytes)
	report.AgentVersion = strings.Repeat("~", 64)
	report.Accepted, report.Refused, report.DroppedBytes = 1<<62, 1<<62, 1<<62
	body, err := json.Marshal(nodeapi.GuestReportRequest{Epoch: 1 << 62, Report: report})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(body) >= 1<<20 {
		t.Fatalf("the largest guest report is %d bytes, over the wire's 1 MiB body", len(body))
	}

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)
	if err := c.RecordGuestReport(t.Context(), "l1", 1<<62, report); err != nil {
		t.Fatalf("the largest guest report was refused: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.guests) != 1 || !bytes.Equal(store.guests[0].report.Data, report.Data) {
		t.Fatalf("the largest guest report was not kept whole")
	}
}

// A LEASE STORE THAT KEEPS NO GUEST REPORTS REFUSES THE ROUTE, permanently,
// rather than answering a 404 the node would read as an older plane or a
// success it did not have.
func TestAStoreThatKeepsNoGuestReportsRefusesTheRoute(t *testing.T) {
	t.Parallel()

	// Embedding the interface hides every method LeaseStore does not name.
	_, base := serve(t, struct{ nodeplane.LeaseStore }{&fakeStore{}})
	c := dial(t, base)

	err := c.RecordGuestReport(t.Context(), "l1", 7, aGuestReport())
	if !errors.Is(err, nodeclient.ErrRefused) || !strings.Contains(err.Error(), "keeps no guest reports") {
		t.Fatalf("RecordGuestReport against a store without them = %v, want ErrRefused saying so", err)
	}
}

// THROUGH THE REAL CLIENT, THE REAL WIRE AND THE REAL ALLOCATOR, a guest
// report is kept for the lease and fenced on its epoch: the allocator the
// control plane serves the wire with is the store the route finds.
func TestAGuestReportReachesTheLedgerThroughTheWire(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(a),
		nodeplane.WithTierCatalog([]config.Tier{tier}))
	srv := httptest.NewServer(nodeplane.Handler(log, p, a, nil))
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker,
		GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
		VCPU: 8, Memory: 32 * config.GiB}); err != nil {
		t.Fatalf("register: %v", err)
	}
	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := c.Bind(t.Context(), lease.ID, lease.Epoch, "n1"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// ANOTHER HOST CANNOT REPORT FOR THIS LEASE, even at its epoch: the route
	// is guarded by the lease's ownership, and the ledger places it on n1.
	other, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n2"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := other.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker,
		GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
		VCPU: 8, Memory: 32 * config.GiB}); err != nil {
		t.Fatalf("register n2: %v", err)
	}
	if err := other.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); !errors.Is(err,
		nodeclient.ErrSuperseded) || !strings.Contains(err.Error(), "the ledger places lease") {
		t.Fatalf("another host's guest report for n1's lease = %v, want ErrSuperseded from the ledger's placement", err)
	}

	if err := c.RecordGuestReport(t.Context(), lease.ID, lease.Epoch+1, aGuestReport()); !errors.Is(err, alloc.ErrFenced) {
		t.Fatalf("a guest report at a stale epoch = %v, want ErrFenced", err)
	}
	if err := c.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	got, err := a.LeaseGuestReport(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	if !sameReport(got.GuestReport, aGuestReport()) || got.Node != "n1" {
		t.Fatalf("the ledger kept %+v from %q, want %+v from n1", got.GuestReport, got.Node, aGuestReport())
	}
}

// TEST RESULTS FROM A PAIRING BELOW THE WIRE THAT CARRIES THEM ARE DROPPED AND THE
// REPORT KEPT, so a node sending what its wire does not carry has nothing recorded
// that wire never said, and costs nothing else. LITERAL VERSIONS, for the reason above.
func TestTestResultsFromAWireThatDoesNotCarryThemAreDropped(t *testing.T) {
	t.Parallel()

	report := aGuestReport()
	report.TestsCodec, report.Tests = alloc.TestResultsCodec, []byte{7}
	body, err := json.Marshal(nodeapi.GuestReportRequest{Epoch: 7, Report: report})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, tc := range []struct {
		version int
		kept    bool
	}{{37, false}, {38, true}} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d", tc.version)
		registerAt(t, base, tc.version, incarnation)

		status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/guest", incarnation, body)
		if status != http.StatusNoContent {
			t.Fatalf("at wire %d the report answered %d %+v, want it kept", tc.version, status, refusal)
		}

		store.mu.Lock()
		if len(store.guests) != 1 {
			store.mu.Unlock()
			t.Fatalf("at wire %d the ledger was asked to record %d reports, want 1", tc.version, len(store.guests))
		}
		got := store.guests[0].report
		store.mu.Unlock()

		if kept := len(got.Tests) == 1 && got.TestsCodec == alloc.TestResultsCodec; kept != tc.kept {
			t.Errorf("at wire %d the ledger was asked to keep tests %v under codec %d, want kept %t", tc.version,
				got.Tests, got.TestsCodec, tc.kept)
		}
	}
}

// TEST RESULTS THIS CONTROL PLANE CANNOT KEEP ARE REFUSED AT THE BOUNDARY, as a report
// it cannot keep is, and at their bound they fit one request.
func TestTestResultsAreHeldToTheirBoundAtTheWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	report := aGuestReport()
	report.Data = make([]byte, alloc.MaxGuestReportBytes)
	report.TestsCodec, report.Tests = alloc.TestResultsCodec, make([]byte, alloc.MaxTestResultsBytes)
	if err := c.RecordGuestReport(t.Context(), "l1", 7, report); err != nil {
		t.Fatalf("the largest report with the largest test results: %v", err)
	}

	report.Tests = make([]byte, alloc.MaxTestResultsBytes+1)
	err := c.RecordGuestReport(t.Context(), "l1", 7, report)
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Errorf("test results past their bound = %v, want refused", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.guests) != 1 {
		t.Errorf("the ledger was asked to record %d reports, want the one that fits", len(store.guests))
	}
}

// fakeIndexer is the control plane's log index.
type fakeIndexer struct {
	err error
	got []string
}

func (f *fakeIndexer) IndexLogChunk(_ context.Context, node, lease string, first int64, _ []byte) error {
	f.got = append(f.got, fmt.Sprintf("%s %s %d", node, lease, first))

	return f.err
}

// A CHUNK REACHES THE INDEX WITH THE NODE THE WIRE SAYS SENT IT, never one the body names; a pairing
// below the wire is refused; and the index's own refusal is one the node reads as final, where
// any other failure is one it tries again.
func TestALogChunkCrossesTheWire(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(nodeapi.LogChunkRequest{Lease: "l1", FirstLine: 7, Data: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		version int
		err     error
		status  int
		code    string
		indexed bool
	}{
		// LITERAL VERSIONS, for the reason above.
		{"the last wire without it", 41, nil, http.StatusBadRequest, nodeapi.CodeRefused, false},
		{"the wire with it", 42, nil, http.StatusNoContent, "", true},
		{"a refusal", 42, fmt.Errorf("%w: no", nodeplane.ErrRefused), http.StatusBadRequest, nodeapi.CodeRefused, true},
		{"a failure", 42, errors.New("busy"), http.StatusServiceUnavailable, "", true},
	} {
		index := &fakeIndexer{err: tc.err}
		log := slog.New(slog.DiscardHandler)
		p := nodeplane.New(log, deployment, time.Minute)
		srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil, nodeplane.WithLogIndex(index)))
		t.Cleanup(srv.Close)

		incarnation := fmt.Sprintf("wire-%d", tc.version)
		registerAt(t, srv.URL, tc.version, incarnation)

		status, refusal := postAs(t, srv.URL, "/v1/nodes/n1/logs", incarnation, body)
		if status != tc.status || refusal.Code != tc.code {
			t.Errorf("%s: answered %d %+v, want %d %q", tc.name, status, refusal, tc.status, tc.code)
		}
		if indexed := len(index.got) == 1; indexed != tc.indexed {
			t.Errorf("%s: the index was asked %v, want asked %t", tc.name, index.got, tc.indexed)
		}
		if tc.indexed && index.got[0] != "n1 l1 7" {
			t.Errorf("%s: the index was asked %v", tc.name, index.got)
		}
	}
}

// A CONTROL PLANE WITH NO INDEX SAYS IT COULD NOT TAKE A CHUNK, and the node tries again: its next
// registration says whether there is an index to ship to at all.
func TestAPlaneWithNoIndexCouldNotTakeAChunk(t *testing.T) {
	t.Parallel()

	_, base := serve(t, &fakeStore{})
	registerAt(t, base, nodeapi.Version, "current")

	body, err := json.Marshal(nodeapi.LogChunkRequest{Lease: "l1", Data: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}

	// NOT A REFUSAL, which a node takes as for good and marks past: an index can come back under the
	// id the node still holds.
	status, refusal := postAs(t, base, "/v1/nodes/n1/logs", "current", body)
	if status != http.StatusServiceUnavailable || refusal.Code == nodeapi.CodeRefused {
		t.Errorf("answered %d %+v, want could-not-tell", status, refusal)
	}
}

// THE INDEX'S NAME GOES ONLY TO A NODE THAT CAN READ IT, and asking a node the plane has not heard
// of is could-not-tell, by the sentinel, never a log with no lines.
func TestAnIndexIsNamedAtItsWireAndAnUnknownNodeIsNoAnswer(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithLogIndexID("index-a"))

	for _, tc := range []struct {
		version int
		want    string
	}{{41, ""}, {42, "index-a"}} {
		body, err := json.Marshal(nodeapi.RegisterRequest{MinVersion: tc.version, Version: tc.version, Incarnation: "i",
			Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
			Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/register", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(nodeapi.HeaderIncarnation, "i")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("register: %v", err)
		}

		var got nodeapi.RegisterResponse

		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.LogIndexID != tc.want {
			t.Errorf("at wire %d the answer names %q, want %q", tc.version, got.LogIndexID, tc.want)
		}
	}

	if _, err := p.ReadJobLog(t.Context(), "nobody", nodeapi.LogReadSpec{Lease: "l1"}); !errors.Is(err, nodeplane.ErrNoNode) {
		t.Errorf("a node not registered: %v", err)
	}
}
