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
		report.Launching != 1 || report.Idle != 1 || report.Running != 0 || report.Cleanup != 1 ||
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

// A POOLED RUNNER IS CLASSIFIED BY ITS POOL RECORD, because its lease stays in
// `launching` from the launch to the teardown (#291). Read from the phase, a fleet
// running a job on every lease reported every lease launching and running 0.
func TestCapacityReportReadsPooledRunnersFromTheirRecords(t *testing.T) {
	tr := tier("pooled", 1, config.GiB)
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{tr})
	leases, err := a.Escrow(t.Context(), tr.Label, 4)
	if err != nil || len(leases) != 4 {
		t.Fatalf("escrow = %v, %v", leases, err)
	}
	for i, lease := range leases {
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
			t.Fatal(err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
			t.Fatal(err)
		}
	}
	// leases[0] is still launching: its launch has not returned, so no member.
	for i := 1; i < len(leases); i++ {
		if err := a.RegisterPoolRunner(t.Context(), PoolRunner{
			LeaseID: leases[i].ID, Tier: tr.Label, LaunchRequestID: int64(i + 1),
			RunnerName: "runner-" + leases[i].ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.StartPoolRunner(t.Context(), leases[2].ID, tr.Label, 7,
		"runner-"+leases[2].ID, 3, 11, "job-3", JobIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := a.RetirePoolRunner(t.Context(), leases[3].ID); err != nil {
		t.Fatal(err)
	}

	report, err := a.CapacityReport(t.Context(), tr.Label)
	if err != nil {
		t.Fatal(err)
	}
	if report.Launching != 1 || report.Idle != 1 || report.Running != 1 ||
		report.Cleanup != 1 || report.Unknown != 0 {
		t.Fatalf("pooled runners were classified by lease phase: %+v", report)
	}
}

// A RUNNER RECORD THE REPORT CANNOT RECONCILE IS UNKNOWN, never launching: a
// launching count is a claim that the launch has not returned.
func TestCapacityReportCountsAnUnreconciledRunnerAsUnknown(t *testing.T) {
	for name, member := range map[string]PoolRunner{
		"an unknown status": {Tier: "pooled", Status: "adopted"},
		"another tier":      {Tier: "elsewhere", Status: PoolRunnerBusy},
	} {
		var report TierCapacity
		report.countLaunched(PhaseLaunching, member, true, "pooled")
		if report.Unknown != 1 || report.Launching != 0 || report.Running != 0 {
			t.Errorf("%s was reported as %+v", name, report)
		}
	}
}

// THE BATCHED REPORT IS EACH TIER'S OWN, from one snapshot: every tier is
// there, and each matches what CapacityReport says of it alone, so a lease is
// counted in its own tier and in no other.
func TestCapacityReportsIsEachTiersReport(t *testing.T) {
	small, large := tier("small", 1, config.GiB), tier("large", 2, 2*config.GiB)
	large.Reserved = 1
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{small, large})

	smallLeases, err := a.Escrow(t.Context(), small.Label, 3)
	if err != nil || len(smallLeases) != 3 {
		t.Fatalf("escrow small = %v, %v", smallLeases, err)
	}

	if _, err := a.Escrow(t.Context(), large.Label, 2); err != nil {
		t.Fatal(err)
	}

	if err := a.RecordListenerCapacity(t.Context(), small.Label, ListenerCapacity{
		Discovery: []string{smallLeases[0].ID}, Pending: []string{smallLeases[1].ID},
	}); err != nil {
		t.Fatal(err)
	}

	reports, err := a.CapacityReports(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(reports) != 2 {
		t.Fatalf("CapacityReports covers %d tiers, want 2: %+v", len(reports), reports)
	}

	for _, label := range []string{small.Label, large.Label} {
		alone, err := a.CapacityReport(t.Context(), label)
		if err != nil {
			t.Fatal(err)
		}

		batched, ok := reports[label]
		if !ok {
			t.Fatalf("CapacityReports has no %s", label)
		}

		if batched.Discovery != alone.Discovery || batched.Pending != alone.Pending ||
			batched.Unknown != alone.Unknown || batched.Floor != alone.Floor || batched.Headroom != alone.Headroom ||
			batched.ObservedAt != alone.ObservedAt {
			t.Errorf("%s: batched %+v, alone %+v", label, batched, alone)
		}
	}

	if got := reports[small.Label]; got.Discovery != 1 || got.Pending != 1 || got.Unknown != 1 {
		t.Errorf("small = %+v, want one each of discovery, pending and unknown", got)
	}

	if got := reports[large.Label]; got.Unknown != 2 || got.Discovery != 0 || got.Floor != 1 || got.ObservedAt != "" {
		t.Errorf("large = %+v, want its two unobserved leases unknown, its floor, and no report", got)
	}
}

// EVERY CLOSE ADVANCES ITS TIER'S RELEASE GENERATION, AND ONLY ITS TIER'S, in
// the transaction that closes it (#140). A drain trusts a listener's count only
// while this has not moved since the count was read, so a release that
// advanced nothing would leave a count standing that the release made wrong.
func TestEveryCloseAdvancesItsTiersReleases(t *testing.T) {
	one, other := tier("one", 1, config.GiB), tier("other", 1, config.GiB)
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{one, other})

	generation := func(label string) int64 {
		t.Helper()

		active, at, err := a.ActiveRunnersAt(t.Context(), label)
		if err != nil {
			t.Fatalf("ActiveRunnersAt(%s): %v", label, err)
		}

		// ONE PREDICATE, written twice: the statement that reads the count with
		// the generation must count what ActiveRunnerLeases counts.
		want, err := a.ActiveRunnerLeases(t.Context(), label)
		if err != nil {
			t.Fatalf("ActiveRunnerLeases(%s): %v", label, err)
		}

		if active != want {
			t.Fatalf("tier %s counts %d runners read with its generation and %d alone", label, active, want)
		}

		all, err := a.TierReleases(t.Context())
		if err != nil {
			t.Fatalf("TierReleases: %v", err)
		}

		if all[label] != at {
			t.Fatalf("tier %s's generation is %d read alone and %d read with the others", label, at, all[label])
		}

		return at
	}

	if got := generation("one"); got != 0 {
		t.Fatalf("a tier none of whose leases closed is at generation %d, want 0", got)
	}

	// THE COUNT'S CASES: escrow that started nothing, a runner launching, and a
	// launching runner whose registration GitHub removed. Only the second counts.
	held, err := a.Escrow(t.Context(), "one", 3)
	if err != nil || len(held) != 3 {
		t.Fatalf("Escrow = %v, %v", held, err)
	}

	for i, lease := range held[1:] {
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
			t.Fatalf("Bind: %v", err)
		}

		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, int64(i+1)); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}

	if err := a.MarkDeregistered(t.Context(), held[2].ID); err != nil {
		t.Fatalf("MarkDeregistered: %v", err)
	}

	if active, _, err := a.ActiveRunnersAt(t.Context(), "one"); err != nil || active != 1 {
		t.Fatalf("ActiveRunnersAt = %d, %v; want the one launching runner still registered", active, err)
	}

	if got := generation("one"); got != 0 {
		t.Fatalf("nothing closed, but tier one is at generation %d", got)
	}

	// OTHER CLOSES FIRST, so it has a row that a bump of every row would move.
	theirs := reserve(t, a, "other")
	if err := a.Release(t.Context(), theirs.ID, theirs.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release(other): %v", err)
	}

	done := reserve(t, a, "one")
	if err := a.Release(t.Context(), done.ID, done.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	failed := reserve(t, a, "one")
	if err := a.ReleaseFailed(t.Context(), failed.ID, failed.Epoch, "the launch failed"); err != nil {
		t.Fatalf("ReleaseFailed: %v", err)
	}

	if got := generation("one"); got != 2 {
		t.Errorf("after two closes tier one is at generation %d, want 2", got)
	}

	if got := generation("other"); got != 1 {
		t.Errorf("tier other closed one lease and is at generation %d after tier one's closes, want 1", got)
	}
}
