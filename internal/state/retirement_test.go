package state

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// ledgerEngines opens a scratch ledger on SQLite and, when the suite has a
// DSN, on PostgreSQL, because the row is shared code whose only engine-specific
// half is the write serialisation each engine provides. Every retirement test
// runs its body once per engine as a subtest.
var ledgerEngines = []struct {
	name string
	open func(t *testing.T) *DB
}{
	{"sqlite", open},
	{"postgres", openPostgres},
}

func testReservation(retiring, survivor, run, id string) RetirementReservation {
	return RetirementReservation{
		Deployment: testDeployment, Retiring: retiring, Survivor: survivor,
		Run: run, TransitionID: id, At: time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC),
	}
}

const (
	transitionA = "0123456789abcdef0123456789abcdef"
	transitionB = "fedcba9876543210fedcba9876543210"
)

// A FRESH LEDGER TAKES THE FIRST RESERVATION AND REFUSES THE SECOND HOST ON IT.
//
// The second host's answer carries the row that refused it, so its sentence
// names the holder without a second read; the first row is untouched.
func TestTheFirstReservationHoldsAndAnotherHostIsRefusedOnIt(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			row, outcome, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA))
			if err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			if outcome != ReservationInserted {
				t.Errorf("outcome %q, want %q", outcome, ReservationInserted)
			}

			want := Retirement{
				Deployment: testDeployment, Retiring: "control-a", Survivor: "control-b",
				Run: "run-1", State: RetirementReserved, TransitionID: transitionA,
				ReservedAt: "2026-09-11T08:00:00Z", UpdatedAt: "2026-09-11T08:00:00Z",
			}
			if row != want {
				t.Errorf("reserved row %+v, want %+v", row, want)
			}

			refused, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-b", "control-a", "run-2", transitionB))
			if !errors.Is(err, ErrRetirementReserved) {
				t.Fatalf("the second host's reservation: %v, want ErrRetirementReserved", err)
			}

			if refused != want {
				t.Errorf("the refusal carries %+v, want the holder's row %+v", refused, want)
			}

			stored, present, err := db.ReadRetirement(t.Context(), testDeployment)
			if err != nil || !present {
				t.Fatalf("ReadRetirement: present=%v err=%v", present, err)
			}

			if stored != want {
				t.Errorf("the refused reservation changed the row to %+v", stored)
			}
		})
	}
}

// THE SAME HOST ADOPTS ITS OWN `reserved` ROW, and the adoption keeps the
// original transition id and reservation time: a run that died before its
// journal is recovered by the next run of the same host, under the id the
// first run minted, never a fresh one.
func TestTheSameHostAdoptsItsReservedRowUnderTheOriginalID(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			second := testReservation("control-a", "control-b", "run-2", transitionB)
			second.At = second.At.Add(time.Hour)

			row, outcome, err := db.ReserveRetirement(t.Context(), second)
			if err != nil {
				t.Fatalf("the adopting reservation: %v", err)
			}

			if outcome != ReservationAdopted {
				t.Errorf("outcome %q, want %q", outcome, ReservationAdopted)
			}

			if row.Run != "run-2" || row.TransitionID != transitionA ||
				row.ReservedAt != "2026-09-11T08:00:00Z" || row.UpdatedAt != "2026-09-11T09:00:00Z" {
				t.Errorf("adopted row %+v: want run-2 under the ORIGINAL id %s and reservation "+
					"time, updated at 09:00", row, transitionA)
			}

			stored, _, err := db.ReadRetirement(t.Context(), testDeployment)
			if err != nil {
				t.Fatalf("ReadRetirement: %v", err)
			}

			if stored != row {
				t.Errorf("the ledger holds %+v, the answer said %+v", stored, row)
			}
		})
	}
}

// A RESERVATION IS REFUSED BEFORE ANY WRITE when it names no survivor, the
// same host twice, or no transition id.
func TestAMalformedReservationWritesNothing(t *testing.T) {
	db := open(t)

	for name, r := range map[string]RetirementReservation{
		"survivor is the retiring host": testReservation("a", "a", "run", transitionA),
		"no transition id":              testReservation("a", "b", "run", ""),
		"no run":                        testReservation("a", "b", "", transitionA),
		"no survivor":                   testReservation("a", "", "run", transitionA),
	} {
		if _, _, err := db.ReserveRetirement(t.Context(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, present, err := db.ReadRetirement(t.Context(), testDeployment); err != nil || present {
		t.Errorf("a refused reservation left a row (present=%v err=%v)", present, err)
	}
}

// ADVANCING TO `intent` IS HELD TO THE ROW'S ID AND STATE, and a row at
// `intent` is no longer adoptable.
func TestAdvancingToIntentIsConditionalAndEndsAdoption(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			at := time.Date(2026, 9, 11, 8, 5, 0, 0, time.UTC)

			if err := db.AdvanceRetirementToIntent(t.Context(), testDeployment, "control-a",
				transitionB, "run-1", at); !errors.Is(err, ErrRetirementMoved) {
				t.Errorf("advancing under another transition id: %v, want ErrRetirementMoved", err)
			}

			if err := db.AdvanceRetirementToIntent(t.Context(), testDeployment, "control-a",
				transitionA, "run-1", at); err != nil {
				t.Fatalf("AdvanceRetirementToIntent: %v", err)
			}

			if err := db.AdvanceRetirementToIntent(t.Context(), testDeployment, "control-a",
				transitionA, "run-1", at); !errors.Is(err, ErrRetirementMoved) {
				t.Errorf("advancing a row already at intent: %v, want ErrRetirementMoved", err)
			}

			row, _, err := db.ReadRetirement(t.Context(), testDeployment)
			if err != nil {
				t.Fatalf("ReadRetirement: %v", err)
			}

			if row.State != RetirementIntent || row.UpdatedAt != "2026-09-11T08:05:00Z" {
				t.Errorf("row after the advance: %+v", row)
			}

			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-3", transitionB)); !errors.Is(err, ErrRetirementReserved) {
				t.Errorf("the same host reserving over its own intent row: %v, want ErrRetirementReserved", err)
			}

			if err := db.ReleaseRetirement(t.Context(), testDeployment, "control-a", "run-1"); !errors.Is(err, ErrRetirementMoved) {
				t.Errorf("releasing a row at intent: %v, want ErrRetirementMoved", err)
			}
		})
	}
}

// A RELEASE IS SCOPED TO THE RUN THAT HOLDS THE ROW.
func TestAReleaseTouchesOnlyTheRowItsRunHolds(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			for name, args := range map[string][2]string{
				"another run":  {"control-a", "run-9"},
				"another host": {"control-b", "run-1"},
			} {
				err := db.ReleaseRetirement(t.Context(), testDeployment, args[0], args[1])
				if !errors.Is(err, ErrRetirementMoved) {
					t.Errorf("%s: %v, want ErrRetirementMoved", name, err)
				}
			}

			if _, present, err := db.ReadRetirement(t.Context(), testDeployment); err != nil || !present {
				t.Fatalf("a refused release deleted the row (present=%v err=%v)", present, err)
			}

			if err := db.ReleaseRetirement(t.Context(), testDeployment, "control-a", "run-1"); err != nil {
				t.Fatalf("ReleaseRetirement: %v", err)
			}

			if _, present, err := db.ReadRetirement(t.Context(), testDeployment); err != nil || present {
				t.Errorf("after the release: present=%v err=%v, want positively absent", present, err)
			}
		})
	}
}

func testCompletion(retiring, survivor, id, reservedAt, by string) RetirementCompletion {
	return RetirementCompletion{
		Deployment: testDeployment, Retiring: retiring, Survivor: survivor,
		TransitionID: id, ReservedAt: reservedAt, CompletedBy: by,
		At: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC),
	}
}

// COMPLETION FROM `intent`: the binding fields must all agree, the write is
// idempotent under the same id, and a row for another host is left alone.
func TestACompletionIsHeldToEveryBindingFieldAndIsIdempotent(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			if err := db.AdvanceRetirementToIntent(t.Context(), testDeployment, "control-a",
				transitionA, "run-1", time.Now()); err != nil {
				t.Fatalf("AdvanceRetirementToIntent: %v", err)
			}

			const reservedAt = "2026-09-11T08:00:00Z"

			for name, c := range map[string]RetirementCompletion{
				"another transition id": testCompletion("control-a", "control-b", transitionB, reservedAt, "control-b"),
				"another reservation":   testCompletion("control-a", "control-b", transitionA, "2026-09-11T08:00:01Z", "control-b"),
				"another survivor":      testCompletion("control-a", "control-c", transitionA, reservedAt, "control-c"),
			} {
				_, _, err := db.CompleteRetirement(t.Context(), c)
				if !errors.Is(err, ErrRetirementMismatch) {
					t.Errorf("%s: %v, want ErrRetirementMismatch", name, err)
				}
			}

			_, _, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-b", "control-a", transitionA, reservedAt, "control-a"))
			if !errors.Is(err, ErrRetirementReserved) {
				t.Errorf("completing another host's row: %v, want ErrRetirementReserved", err)
			}

			row, _, err := db.ReadRetirement(t.Context(), testDeployment)
			if err != nil || row.State != RetirementIntent {
				t.Fatalf("a refused completion changed the row: %+v (%v)", row, err)
			}

			done, outcome, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionA, reservedAt, "control-b"))
			if err != nil {
				t.Fatalf("CompleteRetirement: %v", err)
			}

			if outcome != CompletionDone || done.State != RetirementDone ||
				done.CompletedBy != "control-b" || done.CompletedAt != "2026-09-11T09:00:00Z" {
				t.Errorf("completion answered %q with %+v", outcome, done)
			}

			again, outcome, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionA, reservedAt, "control-a"))
			if err != nil || outcome != CompletionAlready {
				t.Fatalf("a replayed completion: %q, %v; want %q", outcome, err, CompletionAlready)
			}

			if again.CompletedBy != "control-b" {
				t.Errorf("the replay rewrote completed_by to %q", again.CompletedBy)
			}

			_, _, err = db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionB, reservedAt, "control-b"))
			if !errors.Is(err, ErrRetirementMismatch) {
				t.Errorf("a done row completed under another id: %v, want ErrRetirementMismatch", err)
			}

			for name, r := range map[string]RetirementReservation{
				"the retired host": testReservation("control-a", "control-b", "run-5", transitionB),
				"the survivor":     testReservation("control-b", "control-a", "run-5", transitionB),
				"a third host":     testReservation("control-c", "control-b", "run-5", transitionB),
			} {
				refused, _, err := db.ReserveRetirement(t.Context(), r)
				if !errors.Is(err, ErrRetirementDone) {
					t.Errorf("%s reserving after a done row: %v, want ErrRetirementDone", name, err)
				}

				if refused.Retiring != "control-a" || refused.State != RetirementDone {
					t.Errorf("%s: the refusal carries %+v, want the done row", name, refused)
				}
			}

			_, _, err = db.CompleteRetirement(t.Context(),
				testCompletion("control-b", "control-a", transitionB, reservedAt, "control-a"))
			if !errors.Is(err, ErrRetirementDone) {
				t.Errorf("completing another host over a done row: %v, want ErrRetirementDone", err)
			}
		})
	}
}

// COMPLETION FROM `reserved` NEEDS THE ID TOO. The id is minted at the
// reservation and stored in the insert, so a document without the row's id is
// not this row's, whatever its reservation time says.
func TestACompletionFromReservedRequiresTheRowsTransitionID(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
				t.Fatalf("ReserveRetirement: %v", err)
			}

			const reservedAt = "2026-09-11T08:00:00Z"

			_, _, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionB, reservedAt, "control-b"))
			if !errors.Is(err, ErrRetirementMismatch) {
				t.Fatalf("a reserved row completed under another id: %v, want ErrRetirementMismatch", err)
			}

			row, outcome, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionA, reservedAt, "control-b"))
			if err != nil || outcome != CompletionDone || row.State != RetirementDone {
				t.Fatalf("completing from reserved: %q %+v %v", outcome, row, err)
			}
		})
	}
}

// A POSITIVELY ABSENT ROW IS RECONSTRUCTED FROM THE DOCUMENT, with no run,
// because a ledger restored from before the reservation still belongs to a
// deployment that retired this host.
func TestACompletionReconstructsAPositivelyAbsentRow(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			row, outcome, err := db.CompleteRetirement(t.Context(),
				testCompletion("control-a", "control-b", transitionA, "2026-09-11T07:00:00Z", "control-b"))
			if err != nil {
				t.Fatalf("CompleteRetirement on an empty ledger: %v", err)
			}

			want := Retirement{
				Deployment: testDeployment, Retiring: "control-a", Survivor: "control-b",
				State: RetirementDone, TransitionID: transitionA,
				ReservedAt: "2026-09-11T07:00:00Z", UpdatedAt: "2026-09-11T09:00:00Z",
				CompletedBy: "control-b", CompletedAt: "2026-09-11T09:00:00Z",
			}
			if outcome != CompletionDone || row != want {
				t.Errorf("reconstruction answered %q with %+v, want %+v", outcome, row, want)
			}

			stored, present, err := db.ReadRetirement(t.Context(), testDeployment)
			if err != nil || !present || stored != want {
				t.Errorf("the ledger holds %+v (present=%v err=%v)", stored, present, err)
			}

			if _, _, err := db.ReserveRetirement(t.Context(),
				testReservation("control-b", "control-c", "run-2", transitionB)); !errors.Is(err, ErrRetirementDone) {
				t.Errorf("a reservation after the reconstruction: %v, want ErrRetirementDone", err)
			}
		})
	}
}

// A MALFORMED COMPLETION WRITES NOTHING.
func TestAMalformedCompletionWritesNothing(t *testing.T) {
	db := open(t)

	for name, c := range map[string]RetirementCompletion{
		"no id":              testCompletion("a", "b", "", "2026-09-11T07:00:00Z", "b"),
		"no reservation":     testCompletion("a", "b", transitionA, "", "b"),
		"nobody completed":   testCompletion("a", "b", transitionA, "2026-09-11T07:00:00Z", ""),
		"survivor is itself": testCompletion("a", "a", transitionA, "2026-09-11T07:00:00Z", "a"),
	} {
		if _, _, err := db.CompleteRetirement(t.Context(), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, present, err := db.ReadRetirement(t.Context(), testDeployment); err != nil || present {
		t.Errorf("a refused completion reconstructed a row (present=%v err=%v)", present, err)
	}
}

// TWO RESERVATIONS RACING END WITH EXACTLY ONE HOLDER, decided by the engine's
// single writer and not by which request read first.
func TestRacingReservationsLeaveExactlyOneHolder(t *testing.T) {
	for _, engine := range ledgerEngines {
		t.Run(engine.name, func(t *testing.T) {
			db := engine.open(t)
			const contenders = 6

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				inserted int
				refused  int
				faults   []error
			)

			for i := range contenders {
				wg.Add(1)

				go func() {
					defer wg.Done()

					me := "control-" + string(rune('a'+i))
					other := "control-" + string(rune('a'+(i+1)%contenders))

					_, outcome, err := db.ReserveRetirement(t.Context(),
						testReservation(me, other, "run-"+me, transitionA))

					mu.Lock()
					defer mu.Unlock()

					switch {
					case err == nil && outcome == ReservationInserted:
						inserted++
					case errors.Is(err, ErrRetirementReserved):
						refused++
					default:
						faults = append(faults, err)
					}
				}()
			}

			wg.Wait()

			if inserted != 1 || refused != contenders-1 || len(faults) != 0 {
				t.Errorf("%d inserted, %d refused, faults %v; want 1, %d, none",
					inserted, refused, faults, contenders-1)
			}
		})
	}
}

// A READ-ONLY OPEN CAN REPORT THE ROW AND CANNOT WRITE IT, so `rollout status`
// and a dry run see the reservation without being able to take one.
func TestAnInspectHandleReadsTheRetirementRowAndRefusesToWriteIt(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, _, err := db.ReserveRetirement(t.Context(),
		testReservation("control-a", "control-b", "run-1", transitionA)); err != nil {
		t.Fatalf("ReserveRetirement: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := OpenInspect(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenInspect: %v", err)
	}

	t.Cleanup(func() { _ = ro.Close() })

	row, present, err := ro.ReadRetirement(t.Context(), testDeployment)
	if err != nil || !present || row.Retiring != "control-a" {
		t.Errorf("the inspect handle read %+v (present=%v err=%v)", row, present, err)
	}

	if _, _, err := ro.ReserveRetirement(t.Context(),
		testReservation("control-c", "control-b", "run-2", transitionB)); !errors.Is(err, ErrInspect) {
		t.Errorf("a reservation through the inspect handle: %v, want ErrInspect", err)
	}
}
