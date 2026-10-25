package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ONE BATCH ANSWERS EVERY LEASE AS ITS OWN RENEWAL WOULD. Across more leases
// than one transaction carries, a renewed lease outlives the TTL it would have
// expired at and one left out does not; a stale epoch, an unknown id, a lease
// already released, one an operator asked to be released, an id the packed
// statement cannot carry and a lease named twice each get exactly the answer
// Heartbeat gives, in the order they were asked.
func TestHeartbeatManyAnswersEachLeaseAsItsOwnRenewalWould(t *testing.T) {
	now := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tiers := []config.Tier{tier("linux", 1, config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 1000 * config.GiB}, tiers,
		WithClock(func() time.Time { return clock() }), WithLeaseTTL(30*time.Second))
	ctx := t.Context()

	held, err := a.Escrow(ctx, "linux", heartbeatBatch+44)
	if err != nil || len(held) != heartbeatBatch+44 {
		t.Fatalf("Escrow = %d leases, %v", len(held), err)
	}

	// THE CONTROL: a lease nobody renews, which the reap below must take, or
	// the survival of the renewed ones proves nothing.
	unrenewed := reserve(t, a, "linux")

	// NAMED ONLY AT A STALE EPOCH, by a holder that has lost it: its expiry
	// must not move.
	stale := reserve(t, a, "linux")

	released := reserve(t, a, "linux")
	if err := a.Release(ctx, released.ID, released.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	forced := reserve(t, a, "linux")
	launched(t, a, forced, "test-host-firecracker")
	if err := a.Advance(ctx, forced.ID, forced.Epoch, PhaseCustody); err != nil {
		t.Fatalf("Advance(custody): %v", err)
	}
	if result, err := a.ForceRelease(ctx, forced.ID); err != nil || !result.Pending {
		t.Fatalf("ForceRelease = %+v, %v; want a pending request", result, err)
	}

	refs := make([]LeaseRef, 0, len(held)+6)
	for _, l := range held {
		refs = append(refs, LeaseRef{ID: l.ID, Epoch: l.Epoch})
	}

	cases := []struct {
		name string
		ref  LeaseRef
		want error
	}{
		{"a stale epoch", LeaseRef{ID: stale.ID, Epoch: stale.Epoch + 1}, ErrFenced},
		{"a stale epoch beside the current one", LeaseRef{ID: held[3].ID, Epoch: held[3].Epoch + 1}, ErrFenced},
		{"an unknown id", LeaseRef{ID: "no-such-lease", Epoch: 1}, ErrLeaseNotFound},
		{"an id holding a NUL", LeaseRef{ID: "x\x00", Epoch: 1}, ErrLeaseNotFound},
		{"a released lease", LeaseRef{ID: released.ID, Epoch: released.Epoch}, ErrLeaseNotFound},
		{"a force-released lease", LeaseRef{ID: forced.ID, Epoch: forced.Epoch}, ErrForceRelease},
		{"an id wider than the record", LeaseRef{ID: strings.Repeat("a", packedLeaseIDWidth+1), Epoch: 1},
			ErrLeaseNotFound},
		{"a lease named twice", LeaseRef{ID: held[0].ID, Epoch: held[0].Epoch}, nil},
	}

	// THE REFUSALS SIT BETWEEN RENEWALS, on both sides of the batch boundary,
	// so an answer written at the wrong index would land on a renewal.
	at := []int{1, 2, 40, 41, 200, heartbeatBatch, heartbeatBatch + 5, len(refs)}
	for i := len(cases) - 1; i >= 0; i-- {
		refs = append(refs[:at[i]], append([]LeaseRef{cases[i].ref}, refs[at[i]:]...)...)
	}

	now = now.Add(20 * time.Second)

	answers := a.HeartbeatMany(ctx, refs)
	if len(answers) != len(refs) {
		t.Fatalf("%d answers for %d leases", len(answers), len(refs))
	}

	refused := map[LeaseRef]error{}
	for _, c := range cases {
		if c.want != nil {
			refused[c.ref] = c.want
		}
	}

	for i, ref := range refs {
		want, isRefused := refused[ref]

		switch {
		case isRefused && !errors.Is(answers[i], want):
			t.Errorf("lease %d (%s): answered %v, want %v", i, ref.ID, answers[i], want)
		case !isRefused && answers[i] != nil:
			t.Errorf("lease %d (%s): answered %v, want it renewed", i, ref.ID, answers[i])
		}

		// THE SAME REFUSAL ONE RENEWAL GIVES, asked before the clock moves. Only
		// a refusal is compared here, because one renewal of a lease the batch
		// renewed would renew it again and hide a batch that wrote nothing.
		if answers[i] == nil {
			continue
		}

		if one := a.Heartbeat(ctx, ref.ID, ref.Epoch); !sameRenewalAnswer(one, answers[i]) {
			t.Errorf("lease %d (%s): the batch answered %v and one renewal %v", i, ref.ID, answers[i], one)
		}
	}

	// PAST THE TTL THEY WERE ESCROWED UNDER, inside the one they were renewed
	// for: the reaper takes the control and leaves every renewed lease.
	now = now.Add(15 * time.Second)

	// UNTIL A PASS TAKES NOTHING, because one pass takes a bounded number and a
	// batch that wrote nothing leaves every lease here expired.
	for {
		n, err := a.Reap(ctx)
		if err != nil {
			t.Fatalf("Reap: %v", err)
		}

		if n == 0 {
			break
		}
	}

	if _, err := a.Lease(ctx, unrenewed.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("the lease nobody renewed is still open after its TTL (%v), so survival proves nothing", err)
	}

	if _, err := a.Lease(ctx, stale.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("a lease named only at a stale epoch was renewed: %v", err)
	}

	for _, l := range held {
		if _, err := a.Lease(ctx, l.ID); err != nil {
			t.Fatalf("renewed lease %s was reaped: %v", l.ID, err)
		}
	}

	// AND THE RENEWALS ARE WHAT ONE RENEWAL ANSWERS, asked only now that the
	// reap has shown the batch's own writes kept them.
	for i, ref := range refs {
		if answers[i] != nil {
			continue
		}

		if one := a.Heartbeat(ctx, ref.ID, ref.Epoch); !sameRenewalAnswer(one, answers[i]) {
			t.Errorf("lease %d (%s): the batch renewed it and one renewal answered %v", i, ref.ID, one)
		}
	}
}

// A PACKED STRING THE STATEMENT CANNOT CUT INTO RECORDS ENDS, and renews
// nothing. The packer admits no such string, but the statement must not rely
// on it: SQLite's length() stops at a NUL, so a record holding one reads
// short, and a split that never shrinks would hold the writer slot until the
// transaction's deadline.
func TestHeartbeatLeasesEndsOnAStringItCannotCut(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 1, config.GiB)})

	lease := reserve(t, a, "linux")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	record := fmt.Sprintf("%-*s%0*d", packedLeaseIDWidth, lease.ID, packedEpochWidth, lease.Epoch)

	for _, packed := range []string{
		record + "x",
		record + strings.Repeat("y", 82),
		record + "x\x00" + strings.Repeat(" ", packedLeaseIDWidth-2) + strings.Repeat("0", packedEpochWidth),
		strings.Repeat("z", 165),
	} {
		// PostgreSQL refuses a NUL in text outright, which is its own answer.
		if testingPostgres() && strings.ContainsRune(packed, 0) {
			continue
		}

		var renewed int

		err := a.db.Tx(ctx, func(tx *sql.Tx) error {
			rows, err := state.WriteQueries(tx).HeartbeatLeases(ctx, ledgerdb.HeartbeatLeasesParams{
				HeartbeatAt: ts(time.Now()), ExpiresAt: ts(time.Now().Add(time.Minute)), Packed: packed,
			})
			renewed = len(rows)

			return err
		})
		if err != nil {
			t.Fatalf("a string of %d characters: %v", len(packed), err)
		}

		if renewed != 0 {
			t.Fatalf("a string of %d characters that is not whole records renewed %d leases", len(packed), renewed)
		}
	}
}

// sameRenewalAnswer reports whether two renewal answers are the same answer:
// both renewed, or both refused for the same reason.
func sameRenewalAnswer(a, b error) bool {
	for _, sentinel := range []error{ErrFenced, ErrLeaseNotFound, ErrForceRelease} {
		if errors.Is(a, sentinel) || errors.Is(b, sentinel) {
			return errors.Is(a, sentinel) && errors.Is(b, sentinel)
		}
	}

	return a == nil && b == nil
}

// A TRANSACTION THAT FAILS ANSWERS FOR EVERY LEASE IN IT, and renews none:
// a pass that could not reach the ledger has heard nothing about any lease,
// which the listener reads as no answer rather than as lost.
func TestHeartbeatManyAnswersAFailedTransactionForEveryLease(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 1, config.GiB)})

	leases, err := a.Escrow(t.Context(), "linux", 3)
	if err != nil || len(leases) != 3 {
		t.Fatalf("Escrow = %d leases, %v", len(leases), err)
	}

	refs := []LeaseRef{{leases[0].ID, leases[0].Epoch}, {"no-such-lease", 1}, {leases[2].ID, leases[2].Epoch}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for i, err := range a.HeartbeatMany(ctx, refs) {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("lease %d answered %v under a cancelled transaction, want its error", i, err)
		}
	}
}
