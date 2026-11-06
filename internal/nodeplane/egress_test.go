package nodeplane_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

func anEgressReport() alloc.JobEgress {
	return alloc.JobEgress{
		Listed: true, Queries: 4, RefusedQueries: 1,
		RefusedNames: []alloc.EgressRefusal{{Name: "evil.example", Count: 1}},
		Names:        []alloc.EgressNames{{Addr: "198.51.100.1", Names: []string{"github.com"}}},
	}
}

// reportedEgress is one report the fake store kept, in egressStore.
type reportedEgress struct {
	lease  string
	epoch  int64
	report alloc.JobEgress
}

// egressStore is the fake store, keeping egress reports too.
type egressStore struct {
	*fakeStore
	kept []reportedEgress
}

func (s *egressStore) RecordLeaseEgress(_ context.Context, leaseID string, epoch int64, report alloc.JobEgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.kept = append(s.kept, reportedEgress{lease: leaseID, epoch: epoch, report: report})

	return nil
}

// A PAIRING THAT NEGOTIATED BELOW THE VERSION IS REFUSED, NAMING BOTH, and one at it is
// kept with its epoch. LITERAL VERSIONS, so moving the constant cannot move the boundary.
func TestAnEgressReportFromAWireThatDoesNotCarryItIsRefused(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(nodeapi.EgressReportRequest{Epoch: 7, Report: anEgressReport()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, tc := range []struct {
		version int
		status  int
		kept    int
	}{
		{39, http.StatusBadRequest, 0},
		{40, http.StatusNoContent, 1},
	} {
		store := &egressStore{fakeStore: &fakeStore{}}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d", tc.version)
		registerAt(t, base, tc.version, incarnation)

		status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/egress", incarnation, body)
		if status != tc.status {
			t.Fatalf("a report at wire %d answered %d %+v, want %d", tc.version, status, refusal, tc.status)
		}

		if tc.status != http.StatusNoContent {
			want := fmt.Sprintf("needs wire 40 and this node negotiated %d", tc.version)
			if refusal.Code != nodeapi.CodeRefused || !strings.Contains(refusal.Message, want) {
				t.Errorf("at wire %d the refusal is %+v, want %q saying %q", tc.version, refusal,
					nodeapi.CodeRefused, want)
			}
		}

		store.mu.Lock()
		kept := slices.Clone(store.kept)
		store.mu.Unlock()

		if len(kept) != tc.kept {
			t.Fatalf("at wire %d the ledger was asked to record %d reports, want %d", tc.version, len(kept), tc.kept)
		}

		if tc.kept == 1 && (kept[0].lease != "l1" || kept[0].epoch != 7) {
			t.Errorf("recorded %+v", kept[0])
		}
	}
}

// A NAME A TERMINAL WOULD ACT ON IS REFUSED AT THE BOUNDARY AND NEVER REACHES THE LEDGER:
// a node may be running anything, and the guest chose the name.
func TestAnUnkeepableEgressReportIsRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	bad := anEgressReport()
	bad.RefusedNames[0].Name = "a\x1b[31m.example"

	body, err := json.Marshal(nodeapi.EgressReportRequest{Epoch: 7, Report: bad})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	store := &egressStore{fakeStore: &fakeStore{}}
	_, base := serve(t, store)
	registerAt(t, base, nodeapi.VersionEgress, "current")

	status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/egress", "current", body)
	if status != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused {
		t.Fatalf("answered %d %+v", status, refusal)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.kept) != 0 {
		t.Fatalf("the ledger was asked to record %+v", store.kept)
	}
}

// A REPORT ABOUT ANOTHER HOST'S LEASE IS NOT KEPT: it would stand as what the host saw of a
// job that host never ran. The same lease placed on the reporting node is the control.
func TestANodeCannotReportTheEgressOfAnotherHostsLease(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(nodeapi.EgressReportRequest{Epoch: 1, Report: anEgressReport()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, tc := range []struct {
		placedOn string
		status   int
		kept     int
	}{
		{"other", http.StatusConflict, 0},
		{"n1", http.StatusNoContent, 1},
	} {
		store := &egressStore{fakeStore: &fakeStore{lease: &alloc.Lease{ID: "l1", TargetNode: tc.placedOn, Epoch: 1}}}
		_, base := serve(t, store)
		registerAt(t, base, nodeapi.VersionEgress, "current")

		status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/egress", "current", body)
		if status != tc.status {
			t.Fatalf("a report of a lease placed on %s answered %d %+v, want %d", tc.placedOn, status, refusal, tc.status)
		}

		store.mu.Lock()
		kept := len(store.kept)
		store.mu.Unlock()

		if kept != tc.kept {
			t.Fatalf("a report of a lease placed on %s reached the ledger %d times, want %d", tc.placedOn, kept, tc.kept)
		}
	}
}

func TestAStoreThatKeepsNoEgressReportsRefusesTheRoute(t *testing.T) {
	t.Parallel()

	body, err := json.Marshal(nodeapi.EgressReportRequest{Epoch: 7, Report: anEgressReport()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, base := serve(t, &fakeStore{})
	registerAt(t, base, nodeapi.VersionEgress, "current")

	if status, _ := postAs(t, base, "/v1/nodes/n1/leases/l1/egress", "current", body); status != http.StatusNotImplemented {
		t.Fatalf("answered %d, want %d", status, http.StatusNotImplemented)
	}
}

// THE LARGEST REPORT THE VALIDATION ALLOWS FITS ONE REQUEST: refused for its size, every
// job with many destinations would lose its report over a limit nobody chose for it.
func TestTheLargestEgressReportFitsOneRequest(t *testing.T) {
	t.Parallel()

	name := func(tag string, i int) string {
		return fmt.Sprintf("%s-%d.%s.example", tag, i, strings.Repeat("a", 225))
	}

	report := alloc.JobEgress{Listed: true, Queries: 1 << 40, RefusedQueries: 1 << 40, DeniedPackets: 1 << 40,
		OutsideResolverPackets: 1 << 40, IPv6Packets: 1 << 40}

	for i := range alloc.MaxEgressRefusedNames {
		report.RefusedNames = append(report.RefusedNames, alloc.EgressRefusal{Name: name("refused", i), Count: 1 << 40})
	}

	for i := range alloc.MaxEgressDenied {
		report.Denied = append(report.Denied, alloc.EgressDenial{
			Addr: fmt.Sprintf("2001:db8:ffff:ffff:ffff:ffff:ffff:%x", i), Protocol: "tcp", Port: 65535, Packets: 1 << 40,
		})
	}

	for i := range alloc.MaxEgressNamedAddrs {
		names := make([]string, 0, alloc.MaxEgressNamesPerAddr)
		for j := range alloc.MaxEgressNamesPerAddr {
			names = append(names, name("named", i*10+j))
		}

		report.Names = append(report.Names, alloc.EgressNames{
			Addr: fmt.Sprintf("2001:db8:ffff:ffff:ffff:ffff:ffff:%x", i), Names: names,
		})
	}

	if err := report.Validate(); err != nil {
		t.Fatalf("this is not the largest report allowed: %v", err)
	}

	body, err := json.Marshal(nodeapi.EgressReportRequest{Epoch: 1 << 40, Report: report})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	store := &egressStore{fakeStore: &fakeStore{}}
	_, base := serve(t, store)
	registerAt(t, base, nodeapi.VersionEgress, "current")

	if status, refusal := postAs(t, base, "/v1/nodes/n1/leases/l1/egress", "current", body); status != http.StatusNoContent {
		t.Fatalf("answered %d %+v", status, refusal)
	}
}
