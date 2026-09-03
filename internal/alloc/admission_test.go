package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// A SEALED DEPLOYMENT ESCROWS NOTHING, THROUGH EITHER DOOR.
//
// Escrow and Reserve are separate write transactions, and the production
// listener calls only Escrow — so a check placed on Reserve alone would look
// like enforcement on every read and enforce nothing on the one path that
// matters. Both are asserted here for that reason, and the check itself lives
// at the single point they share.
func TestASealedDeploymentAdmitsNothing(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{tier("sealed-tier", 2, 4*config.GiB)})

	// It admits before the seal, or the assertions below would pass against an
	// allocator that never admits anything.
	if _, err := a.Reserve(t.Context(), "sealed-tier"); err != nil {
		t.Fatalf("an open deployment refused a reservation: %v", err)
	}

	if _, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator,
		Reason:     "replacing a disk",
		Actor:      "ops@example.com",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// NOT SUBTESTS: the assertion below is that neither door wrote anything, and
	// parallel subtests would let it run before they had tried.
	_, err := a.Reserve(t.Context(), "sealed-tier")
	if !errors.Is(err, ErrAdmissionSealed) {
		t.Fatalf("a sealed deployment reserved anyway: %v", err)
	}

	// IT SAYS WHO AND WHY, because the operator meeting this is often not the one
	// who sealed it.
	for _, want := range []string{"ops@example.com", "replacing a disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	leases, err := a.Escrow(t.Context(), "sealed-tier", 4)
	if !errors.Is(err, ErrAdmissionSealed) {
		t.Fatalf("a sealed deployment escrowed anyway: %v", err)
	}
	if len(leases) > 0 {
		t.Errorf("escrow returned %d leases alongside its refusal", len(leases))
	}

	// AND NOTHING WAS WRITTEN. Asserting the error alone would pass against an
	// allocator that inserted the lease and then complained.
	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Leases != 1 {
		t.Errorf("the ledger holds %d leases, want only the one taken before the seal",
			usage.Leases)
	}
}

// A SEAL IS NOT NO CAPACITY, and the two must not be confused: no capacity is a
// transient fact about a full fleet that the next poll may find changed, while a
// seal is a decision only a person undoes. A listener logging "no capacity"
// during a drain would send an operator to look at their hardware.
//
// THE FLEET IS FULL IN BOTH CASES HERE, which is the combination the name
// promises: with abundant capacity the check never has to choose between the two
// answers, so the test would pass against an allocator that reported the seal
// only when it happened to reach the insert.
func TestASealIsDistinguishableFromAFullFleet(t *testing.T) {
	t.Parallel()

	// Exactly one slot, so the second request has no room either way.
	a := newAllocator(t, Limits{MaxVCPU: 2, MaxMemory: 4 * config.GiB},
		[]config.Tier{tier("full-tier", 2, 4*config.GiB)})

	if _, err := a.Reserve(t.Context(), "full-tier"); err != nil {
		t.Fatalf("the first reservation failed: %v", err)
	}

	// FULL, AND OPEN: this is the answer a seal must not be confused with.
	_, err := a.Reserve(t.Context(), "full-tier")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a full fleet did not report itself full: %v", err)
	}

	if _, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// FULL, AND SEALED: the seal is the answer, even though the fleet has no room
	// and the insert is never reached.
	_, err = a.Reserve(t.Context(), "full-tier")
	if !errors.Is(err, ErrAdmissionSealed) {
		t.Errorf("a sealed deployment with no headroom reported itself merely full: %v", err)
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Errorf("a sealed deployment reports itself full: %v", err)
	}

	// And through the door the listener actually uses.
	if _, err := a.Escrow(t.Context(), "full-tier", 1); !errors.Is(err, ErrAdmissionSealed) {
		t.Errorf("a sealed, full deployment escrowed without saying it was sealed: %v", err)
	}
}

// RESUMING ADMITS AGAIN, so the seal is a state rather than a one-way door.
func TestResumingLetsTheDeploymentAdmitAgain(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{tier("sealed-tier", 2, 4*config.GiB)})

	sealed, err := a.db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := a.Reserve(t.Context(), "sealed-tier"); !errors.Is(err, ErrAdmissionSealed) {
		t.Fatalf("the fixture did not reach the case: %v", err)
	}

	if _, err := a.db.Resume(t.Context(), state.ResumeRequest{
		Expect: sealed.Generation, Actor: "ops",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if _, err := a.Reserve(t.Context(), "sealed-tier"); err != nil {
		t.Fatalf("a resumed deployment still refuses work: %v", err)
	}
}

// A LEDGER BILLET COULD NOT READ IS NOT ONE THAT SAID YES.
//
// This is the fail-closed half, and it is the one that decides what happens on a
// deployment somebody sealed and a database that then answered badly. Failing
// closed costs an escrow the next poll retries; failing open admits work into a
// deployment sealed to stop exactly that.
func TestAnUnreadableAdmissionStateAdmitsNothing(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("unreadable-tier", 2, 4*config.GiB)})

	// It admits first, so the refusal below cannot be an allocator that never
	// admits anything.
	if _, err := a.Reserve(t.Context(), "unreadable-tier"); err != nil {
		t.Fatalf("an open deployment refused a reservation: %v", err)
	}

	// The row the check reads stops being readable, the way a ledger written by
	// something else — or damaged — would look.
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE admission`)

		return err
	}); err != nil {
		t.Fatalf("make admission unreadable: %v", err)
	}

	// NAMED SEPARATELY, because `if _, err := ...` shadows: the diagnostic
	// assertion below would otherwise re-read the Reserve error and say nothing
	// about Escrow at all.
	_, reserveErr := a.Reserve(t.Context(), "unreadable-tier")
	if !errors.Is(reserveErr, ErrAdmissionSealed) {
		t.Fatalf("an unreadable admission state admitted work: %v", reserveErr)
	}

	leases, escrowErr := a.Escrow(t.Context(), "unreadable-tier", 2)
	if !errors.Is(escrowErr, ErrAdmissionSealed) {
		t.Fatalf("an unreadable admission state escrowed work: %v", escrowErr)
	}
	if len(leases) > 0 {
		t.Errorf("escrow returned %d leases alongside its refusal", len(leases))
	}

	// AND BOTH SAY WHICH KIND OF REFUSAL THIS IS, so an operator is not sent to
	// look at a fleet that is not full.
	for _, err := range []error{reserveErr, escrowErr} {
		if !strings.Contains(err.Error(), "could not read") {
			t.Errorf("the refusal does not say the state was unreadable: %v", err)
		}
	}

	// AND NOTHING WAS WRITTEN. Asserting the errors alone would pass against an
	// allocator that inserted the leases and then complained.
	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Leases != 1 {
		t.Errorf("the ledger holds %d leases, want only the one taken before the state "+
			"became unreadable", usage.Leases)
	}
}
