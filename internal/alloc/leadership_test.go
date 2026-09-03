package alloc

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// The two hosts this file is about: the controller that claimed first, and the
// replacement that takes the deployment from it.
const (
	incumbentHolder   = "host-a"
	replacementHolder = "host-b"
)

// stageSuccessorClaim writes the row a replacement controller's claim writes,
// through a handle that is not the allocator's.
//
// The same shape internal/state uses, and here for the same reason: on SQLite no
// second process can take the exclusion, and what a successor does to the LEDGER
// is exactly this statement. The end-to-end article — a real session lost, a real
// replacement — is TestAControllerCannotWriteOnceAReplacementHasClaimed in
// internal/state.
func stageSuccessorClaim(t *testing.T, db *state.DB) {
	t.Helper()

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := state.WriteQueries(tx).ClaimController(
			t.Context(), ledgerdb.ClaimControllerParams{
				Holder:    replacementHolder,
				ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano),
			})

		return err
	}); err != nil {
		t.Fatalf("stage a successor's claim: %v", err)
	}
}

// A FENCED CONTROLLER FREES NO CAPACITY, AND THE REFUSAL IS NOT A LEASE VERDICT.
//
// TWO PROPERTIES IN ONE TEST BECAUSE THEY ARE THE SAME PROPERTY. Capacity comes
// back only when something PROVES the compute is gone, and a controller that has
// been replaced has proved nothing at all — its Release is refused, the lease
// stays charged, and the replacement inherits a ledger that still knows about the
// work. Anything else hands a slot back while a guest is running on it, which is
// the overcommit the whole escrow ordering exists to prevent.
//
// AND THE SENTINEL MUST NOT READ AS ErrFenced OR ErrLeaseNotFound. Those two say
// "this lease belongs to somebody else", which a listener answers by dropping it
// out of its own bookkeeping — for a lost leadership that would be the fenced
// controller forgetting the compute it launched, on its way out, one lease at a
// time.
//
// It runs on both engines through openTestLedgerPair, because the fence is a
// property of a TRANSACTION and that is exactly what differs between a file with
// a write lock and a server with an advisory one.
func TestALostLeadershipFreesNoCapacity(t *testing.T) {
	plane, other := openTestLedgerPair(t, true)

	tiers := []config.Tier{tier("linux", 2, 4*config.GiB)}

	a, err := New(plane, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), testRegistration(
		"test-host-firecracker", config.ProviderFirecracker)); err != nil {
		t.Fatalf("registering a host: %v", err)
	}

	lease := reserve(t, a, tiers[0].Label)

	if _, err := plane.ClaimController(t.Context(), incumbentHolder, "deployment-under-test"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	before, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if before.Leases != 1 {
		t.Fatalf("the deployment holds %d open lease(s) before the handover, want 1",
			before.Leases)
	}

	stageSuccessorClaim(t, other)

	err = a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone)
	if !errors.Is(err, state.ErrLeadershipLost) {
		t.Fatalf("a replaced controller's Release returned %v, want ErrLeadershipLost", err)
	}

	if errors.Is(err, ErrFenced) || errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("a lost leadership must not read as a verdict about this lease: %v", err)
	}

	// THE READ IS UNFENCED, so this is the ledger's own answer rather than a
	// second copy of the refusal.
	after, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage after the handover: %v", err)
	}

	if after.Leases != before.Leases || after.VCPU != before.VCPU || after.Memory != before.Memory {
		t.Errorf("the refused release changed usage from %+v to %+v; capacity a replaced "+
			"controller could not account for must stay charged", before, after)
	}

	// AND IT ADMITS NOTHING NEW EITHER. A controller that may not release may not
	// escrow: an assignment taken here would be a promise to GitHub that the
	// deployment's real controller has never heard of.
	if _, err := a.Reserve(t.Context(), tiers[0].Label); !errors.Is(
		err, state.ErrLeadershipLost) {
		t.Fatalf("a replaced controller's Reserve returned %v, want ErrLeadershipLost", err)
	}

	if final, err := a.Usage(t.Context()); err != nil {
		t.Fatalf("Usage after the refused reservation: %v", err)
	} else if final.Leases != before.Leases {
		t.Errorf("the refused reservation left %d open lease(s), want %d",
			final.Leases, before.Leases)
	}
}
