package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

	closed, err := db.UsageRecordAt(t.Context(), 2)
	if err != nil || closed.LeaseID != second.ID || closed.FinishedAt != rows[0].FinishedAt {
		t.Fatalf("UsageRecordAt(2) = %+v, %v; want %s closed at %s", closed, err, second.ID, rows[0].FinishedAt)
	}

	if _, err := db.UsageRecordAt(t.Context(), 3); !errors.Is(err, state.ErrNoUsageRecord) {
		t.Fatalf("UsageRecordAt(3) = %v, want the definite no-record answer", err)
	}

	// Zero is what an unfinished row holds, and no row is ever at it.
	if _, err := db.UsageRecordAt(t.Context(), 0); !errors.Is(err, state.ErrNoUsageRecord) {
		t.Fatalf("UsageRecordAt(0) = %v, want the definite no-record answer", err)
	}
}

// NUMBERS BECOME VISIBLE IN THE ORDER THEY WERE TAKEN, with closers on two handles
// beside a reader, which is the controller and an operator's force-destroy. A
// reader paging past N relies on no lower number committing later. The writers'
// own lock serializes these closes on both engines, so what this holds is the whole
// path; state's TestTheUsageCounterHoldsASecondTakerUntilTheFirstCommits holds the
// counter's lock on its own.
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

	// A GUARD, so a close that left its lease unnumbered fails here, with the count,
	// rather than leaving the reader waiting for a number that will never come.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
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

	if readerErr != nil {
		t.Fatalf("after %d of %d leases: %v", len(seen), closes, readerErr)
	}
	if len(seen) != closes {
		t.Fatalf("the reader saw %d of %d closed leases", len(seen), closes)
	}
}

// THE LEASES THAT FINISHED BEFORE THERE WERE NUMBERS TAKE THEIR PLACE IN BATCHES, beside
// leases closing now, and a reader past any number finds each of them exactly once.
func TestLeasesThatFinishedBeforeNumbersAreNumberedInBatches(t *testing.T) {
	db := openTestLedger(t)

	a, err := New(db, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{tier("linux", 1, config.GiB)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mustRegister(t, a, NodeRegistration{Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 16 * config.GiB})

	// A HISTORY FROM BEFORE THE MIGRATION: finished, with no number, oldest first by
	// its clock whatever order it was written in.
	older := []string{strings.Repeat("c", 32), strings.Repeat("a", 32), strings.Repeat("b", 32)}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i, lease := range older {
			if _, err := tx.ExecContext(t.Context(), fmt.Sprintf(`INSERT INTO job_history
				(lease_id, tier, queued_at, finished_at) VALUES ('%s', 'linux', 't', '2026-01-0%d')`,
				lease, i+1)); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	listed, err := a.UnnumberedUsage(t.Context(), 2)
	if err != nil {
		t.Fatalf("UnnumberedUsage: %v", err)
	}
	if want := older[:2]; !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want the oldest two %v", listed, want)
	}

	// A LEASE CLOSES BETWEEN THE LISTING AND THE BATCH, as one does on a running plane.
	closing := reserve(t, a, "linux")
	if err := a.Release(t.Context(), closing.ID, closing.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	numbered, err := a.NumberUsage(t.Context(), listed)
	if err != nil || numbered != 2 {
		t.Fatalf("NumberUsage = %d, %v; want 2", numbered, err)
	}

	// AN ATTEMPT TO NUMBER THEM AGAIN MOVES NOTHING, and burns its numbers harmlessly.
	if again, err := a.NumberUsage(t.Context(), listed); err != nil || again != 0 {
		t.Fatalf("a second batch numbered %d, %v; want 0", again, err)
	}

	rest, err := a.UnnumberedUsage(t.Context(), 10)
	if err != nil {
		t.Fatalf("UnnumberedUsage: %v", err)
	}
	if !slices.Equal(rest, older[2:]) {
		t.Fatalf("left %v, want %v", rest, older[2:])
	}

	if _, err := a.NumberUsage(t.Context(), rest); err != nil {
		t.Fatalf("NumberUsage: %v", err)
	}

	waiting, err := db.UsageStillNumbering(t.Context())
	if err != nil || waiting {
		t.Fatalf("UsageStillNumbering = %v, %v; nothing is left", waiting, err)
	}

	var (
		got  []string
		last int64
	)

	for {
		rows, err := db.UsageSince(t.Context(), last, 2)
		if err != nil {
			t.Fatalf("UsageSince: %v", err)
		}
		if len(rows) == 0 {
			break
		}

		for _, r := range rows {
			if r.Sequence <= last {
				t.Fatalf("%d came after %d", r.Sequence, last)
			}

			got = append(got, r.LeaseID)
			last = r.Sequence
		}
	}

	want := []string{closing.ID, older[0], older[1], older[2]}
	if !slices.Equal(got, want) {
		t.Fatalf("the export reads %v, want each lease once, in the order it was numbered: %v", got, want)
	}
}
