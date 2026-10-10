package nodeplane_test

import (
	"bytes"
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
		Hello: true, FinalSeen: true, FirstReceived: first, LastReceived: first.Add(time.Minute),
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
