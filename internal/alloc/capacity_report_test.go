package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// LEASE OWNERSHIP AND PHASE ARE DIFFERENT FACTS. A pending capacity row, a
// discovery row and an unobserved capacity row must not collapse into one count.
func TestCapacityReportSeparatesEveryCharge(t *testing.T) {
	tr := tier("report", 1, config.GiB)
	tr.Reserved = 1
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{tr})
	leases, err := a.Escrow(t.Context(), tr.Label, 7)
	if err != nil || len(leases) != 7 {
		t.Fatalf("escrow = %v, %v", leases, err)
	}
	for i := 3; i < len(leases); i++ {
		lease := leases[i]
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
			t.Fatal(err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if i >= 4 {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
				t.Fatal(err)
			}
		}
		if i == 5 {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseOnline); err != nil {
				t.Fatal(err)
			}
		}
		if i == 6 {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseTeardown); err != nil {
				t.Fatal(err)
			}
		}
	}
	one, zero := 1, 0
	if err := a.RecordListenerCapacity(t.Context(), tr.Label, ListenerCapacity{
		Discovery: []string{leases[0].ID}, Pending: []string{leases[1].ID},
		Sent: &zero, Confirmed: &one, Exchange: "ambiguous",
	}); err != nil {
		t.Fatal(err)
	}
	report, err := a.CapacityReport(t.Context(), tr.Label)
	if err != nil {
		t.Fatal(err)
	}
	if report.Discovery != 1 || report.Pending != 2 || report.Unknown != 1 ||
		report.Launching != 1 || report.Running != 1 || report.Cleanup != 1 ||
		report.Floor != 1 || report.Headroom != 9 {
		t.Fatalf("capacity categories = %+v", report)
	}
	if report.Listener.Confirmed == nil || *report.Listener.Confirmed != 1 ||
		report.Listener.Sent == nil || *report.Listener.Sent != 0 ||
		report.Listener.Exchange != "ambiguous" || report.ObservedAt == "" {
		t.Fatalf("ambiguous withdrawal was reported as confirmed: %+v", report)
	}
	if err := a.Release(t.Context(), leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
		t.Fatal(err)
	}
	report, err = a.CapacityReport(t.Context(), tr.Label)
	if err != nil {
		t.Fatal(err)
	}
	if report.Discovery != 0 || report.Headroom != 10 {
		t.Fatalf("an old observation counted a released lease: %+v", report)
	}
}
