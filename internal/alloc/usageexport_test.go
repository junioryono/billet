package alloc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// A LEASE IS NUMBERED ONCE, WHEN IT CLOSES, AND THE ROW KEEPS WHOSE IT WAS. A lease
// still open has no number, so an exporter cannot read past one that has not
// finished; the owner and the purchase time survive the lease row.
func TestAClosingLeaseTakesItsUsageNumberAndKeepsItsOwner(t *testing.T) {
	db := openTestLedger(t)

	a, err := New(db, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 2, 4*config.GiB)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 16 * config.GiB})

	first := reserve(t, a, "linux")
	second := reserve(t, a, "linux")

	// A HISTORY ROW IS NOT A FINISH: assignment opens one, and it must not export.
	if err := a.Assign(t.Context(), second.ID, second.Epoch, 99, 7); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.RecordLeaseGitHubTarget(t.Context(), first.ID, first.Epoch, "acme"); err != nil {
		t.Fatalf("record the lease's owner: %v", err)
	}

	open, err := db.UsageSince(t.Context(), 0, 10)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("%d open leases were exported: %+v", len(open), open)
	}

	if err := a.Release(t.Context(), first.ID, first.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rows, err := db.UsageSince(t.Context(), 0, 10)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(rows) != 1 || rows[0].LeaseID != first.ID || rows[0].Sequence != 1 {
		t.Fatalf("after one close the export is %+v, want lease %s at 1", rows, first.ID)
	}
	if rows[0].Target != "acme" {
		t.Errorf("the owner the lease recorded is %q on its history, want acme", rows[0].Target)
	}
	if rows[0].BoughtAt == "" {
		t.Error("a lease GitHub never assigned a job says nothing about when it was bought")
	}
	if rows[0].FinishedAt == "" {
		t.Error("a closed lease has no finish time")
	}

	if err := a.Release(t.Context(), second.ID, second.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rows, err = db.UsageSince(t.Context(), 1, 10)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(rows) != 1 || rows[0].LeaseID != second.ID || rows[0].Sequence != 2 {
		t.Fatalf("reading past 1 answers %+v, want lease %s at 2", rows, second.ID)
	}

	lease, err := db.UsageLease(t.Context(), 2)
	if err != nil || lease != second.ID {
		t.Fatalf("UsageLease(2) = %q, %v; want %s", lease, err, second.ID)
	}

	if _, err := db.UsageLease(t.Context(), 3); !errors.Is(err, state.ErrNoUsageRecord) {
		t.Fatalf("UsageLease(3) = %v, want the definite no-record answer", err)
	}

	// Zero is what an unfinished row holds, and no row is ever at it.
	if _, err := db.UsageLease(t.Context(), 0); !errors.Is(err, state.ErrNoUsageRecord) {
		t.Fatalf("UsageLease(0) = %v, want the definite no-record answer", err)
	}
}

// NUMBERS BECOME VISIBLE IN THE ORDER THEY WERE TAKEN, with closers on two handles
// beside a reader, which is the controller and an operator's force-destroy. A
// reader paging past N relies on no lower number committing later. Under
// BILLET_TEST_LEDGER=postgres this is the case a PostgreSQL sequence would fail:
// the counter row's lock is what holds it there.
func TestAReaderNeverSeesAUsageNumberBeforeALowerOne(t *testing.T) {
	const closes = 40

	first, second := openTestLedgerPair(t, true)
	tiers := []config.Tier{tier("linux", 1, config.GiB)}
	limits := Limits{MaxVCPU: 200, MaxMemory: 400 * config.GiB}

	a, err := New(first, limits, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b, err := New(second, limits, tiers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 200, Memory: 400 * config.GiB})

	leases := make([]*Lease, 0, closes)
	for range closes {
		leases = append(leases, reserve(t, a, "linux"))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		reader    sync.WaitGroup
		seen      = map[string]bool{}
		last      int64
		readerErr error
	)

	reader.Go(func() {
		for len(seen) < closes {
			rows, err := first.UsageSince(ctx, last, 5)
			if err != nil {
				readerErr = err

				return
			}

			for _, r := range rows {
				switch {
				case r.Sequence != last+1:
					readerErr = fmt.Errorf("read %d straight after %d: a number committed before a lower one",
						r.Sequence, last)

					return
				case seen[r.LeaseID]:
					readerErr = fmt.Errorf("lease %s was read twice", r.LeaseID)

					return
				}

				seen[r.LeaseID] = true
				last = r.Sequence
			}
		}
	})

	var closers sync.WaitGroup

	for i, lease := range leases {
		closer := a
		if i%2 == 1 {
			closer = b
		}

		closers.Go(func() {
			if err := closer.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
				t.Errorf("Release: %v", err)
			}
		})
	}

	closers.Wait()

	if t.Failed() {
		cancel()
	}

	reader.Wait()

	if readerErr != nil && !t.Failed() {
		t.Fatal(readerErr)
	}
	if !t.Failed() && len(seen) != closes {
		t.Fatalf("the reader saw %d of %d closed leases", len(seen), closes)
	}
}
