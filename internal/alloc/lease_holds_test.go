package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func holdAllocator(t *testing.T, now *time.Time) *Allocator {
	t.Helper()

	return newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return *now }))
}

// A BOUND IS WRITTEN ONCE: a second opening, a retry or a restart with another setting answers the
// row as it stands, or a hold could be extended for ever.
func TestAHoldsBoundIsWrittenOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := holdAllocator(t, &now)

	first := now.Add(time.Hour)

	hold, err := a.OpenLeaseHold(t.Context(), "l1", "t", first)
	if err != nil || !hold.Holding() || !hold.Until.Equal(first) || hold.Tier != "t" || !hold.HeldAt.Equal(now) {
		t.Fatalf("the opening: %+v, %v", hold, err)
	}

	now = now.Add(time.Minute)

	again, err := a.OpenLeaseHold(t.Context(), "l1", "t", now.Add(24*time.Hour))
	if err != nil || !again.Until.Equal(first) || !again.HeldAt.Equal(hold.HeldAt) {
		t.Errorf("a second opening answered %+v, %v; want the bound it opened with", again, err)
	}
}

// A SEALED DEPLOYMENT OPENS NO NEW HOLD, and keeps one it had (#783). The seal is read in the
// transaction that would write the hold, so a seal committed before it is never missed, and a hold
// opened before the seal is answered as it stands.
func TestASealedDeploymentOpensNoNewHoldAndKeepsItsOld(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := holdAllocator(t, &now)

	before, err := a.OpenLeaseHold(t.Context(), "l1", "t", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("open before the seal: %v", err)
	}

	if _, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "draining", Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if hold, err := a.OpenLeaseHold(t.Context(), "l2", "t", now.Add(time.Hour)); !errors.Is(err, ErrAdmissionSealed) {
		t.Errorf("a new hold on a sealed deployment: %+v, %v; want ErrAdmissionSealed", hold, err)
	}

	if _, err := a.LeaseHoldOf(t.Context(), "l2"); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a refused hold was written: %v", err)
	}

	after, err := a.OpenLeaseHold(t.Context(), "l1", "t", now.Add(24*time.Hour))
	if err != nil || !after.Holding() || !after.Until.Equal(before.Until) {
		t.Errorf("the hold from before the seal: %+v, %v; want it as it stood", after, err)
	}
}

// NO HOLD IS NOT AN ENDED ONE, and an ended one is not opened again: a retry finds it ended.
func TestAnEndedHoldStaysEnded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := holdAllocator(t, &now)

	if _, err := a.LeaseHoldOf(t.Context(), "l1"); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a lease with no hold: %v", err)
	}

	if ended, err := a.EndLeaseHold(t.Context(), "l1", HoldEndedByOperator); err != nil || ended {
		t.Errorf("ending a hold that is not there: %t, %v", ended, err)
	}

	if _, err := a.OpenLeaseHold(t.Context(), "l1", "t", now.Add(time.Hour)); err != nil {
		t.Fatalf("open: %v", err)
	}

	if ended, err := a.EndLeaseHold(t.Context(), "l1", HoldEndedBySession); err != nil || !ended {
		t.Fatalf("end: %t, %v", ended, err)
	}

	// WHAT ENDED IT FIRST STANDS.
	if ended, err := a.EndLeaseHold(t.Context(), "l1", HoldEndedByOperator); err != nil || ended {
		t.Errorf("ending it twice: %t, %v", ended, err)
	}

	hold, err := a.OpenLeaseHold(t.Context(), "l1", "t", now.Add(time.Hour))
	if err != nil || hold.Holding() || hold.EndedBy != HoldEndedBySession {
		t.Errorf("opening an ended hold: %+v, %v", hold, err)
	}
}

// WHAT IS LISTED IS WHAT IS HOLDING, the soonest first; a hold past its bound has stopped holding
// whether or not anything has written that yet, and the pass that writes it ends only those.
func TestHoldsAreListedAndLapse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := holdAllocator(t, &now)

	for lease, in := range map[string]time.Duration{"late": 2 * time.Hour, "soon": time.Hour, "ended": 3 * time.Hour} {
		if _, err := a.OpenLeaseHold(t.Context(), lease, "t", now.Add(in)); err != nil {
			t.Fatalf("open %s: %v", lease, err)
		}
	}

	if _, err := a.EndLeaseHold(t.Context(), "ended", HoldEndedByOperator); err != nil {
		t.Fatal(err)
	}

	holds, err := a.HoldingLeaseHolds(t.Context())
	if err != nil || len(holds) != 2 || holds[0].LeaseID != "soon" || holds[1].LeaseID != "late" {
		t.Fatalf("listed %+v, %v", holds, err)
	}

	now = now.Add(90 * time.Minute)

	holds, err = a.HoldingLeaseHolds(t.Context())
	if err != nil || len(holds) != 1 || holds[0].LeaseID != "late" {
		t.Errorf("past one's bound, listed %+v, %v", holds, err)
	}

	if n, err := a.EndLapsedLeaseHolds(t.Context()); err != nil || n != 1 {
		t.Errorf("ended %d lapsed, %v; want the one", n, err)
	}

	if hold, err := a.LeaseHoldOf(t.Context(), "soon"); err != nil || hold.EndedBy != HoldEndedByBound {
		t.Errorf("the lapsed hold: %+v, %v", hold, err)
	}

	if hold, err := a.LeaseHoldOf(t.Context(), "late"); err != nil || !hold.Holding() {
		t.Errorf("the hold inside its bound: %+v, %v", hold, err)
	}

	if n, err := a.EndHoldingLeaseHolds(t.Context(), HoldEndedByDrain); err != nil || n != 1 {
		t.Errorf("a drain ended %d, %v; want the one left", n, err)
	}
}

// AN ENDED HOLD IS FORGOTTEN BY AGE, and one still holding never is, however old.
func TestOnlyAnEndedHoldIsForgotten(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a := holdAllocator(t, &now)

	for _, lease := range []string{"holding", "ended"} {
		if _, err := a.OpenLeaseHold(t.Context(), lease, "t", now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := a.EndLeaseHold(t.Context(), "ended", HoldEndedByOperator); err != nil {
		t.Fatal(err)
	}

	if n, err := a.ForgetEndedLeaseHolds(t.Context(), now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("forgot %d younger than the cut, %v", n, err)
	}

	if n, err := a.ForgetEndedLeaseHolds(t.Context(), now.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("forgot %d, %v; want the ended one", n, err)
	}

	if _, err := a.LeaseHoldOf(t.Context(), "holding"); err != nil {
		t.Errorf("the hold still holding was forgotten: %v", err)
	}
}

// AN ENDED HOLD STAYS WHILE ITS LEASE LIVES: a destroy still being retried would otherwise find no
// row, open a new hold, and keep a VM whose hold had ended.
func TestAnEndedHoldStaysWhileItsLeaseLives(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("small", 2, 8*config.GiB)}, WithClock(func() time.Time { return now }))

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := a.OpenLeaseHold(t.Context(), lease.ID, "small", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := a.EndLeaseHold(t.Context(), lease.ID, HoldEndedByOperator); err != nil {
		t.Fatal(err)
	}

	if n, err := a.ForgetEndedLeaseHolds(t.Context(), now.Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("forgot %d with its lease alive, %v", n, err)
	}

	hold, err := a.OpenLeaseHold(t.Context(), lease.ID, "small", now.Add(2*time.Hour))
	if err != nil || hold.Holding() {
		t.Errorf("opening it again: %+v, %v; want it ended still", hold, err)
	}
}
