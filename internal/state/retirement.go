package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// The retirement row's states. ONE VOCABULARY, reaching every statement as a
// parameter, so a misspelling is a compile error here and never a filter that
// silently matches nothing.
const (
	RetirementReserved = "reserved"
	RetirementIntent   = "intent"
	RetirementDone     = "done"
)

// ErrRetirementReserved means the deployment's retirement row names another
// host, or names this host in a state a reservation cannot adopt.
//
// ITS OWN ERROR because the remedy is the other run's business: the row is what
// two requests contend for, and the loser's answer is that somebody else holds
// it, never that the ledger is at fault.
var ErrRetirementReserved = errors.New(
	"state: another retirement holds this deployment's reservation")

// ErrRetirementDone means this deployment has already retired a controller.
//
// DISTINCT FROM ErrRetirementReserved because nothing resolves it: a `done` row
// is never released, since a deployment that retired one controller has no
// survivor for a second, and a request refused on it is refused for the life of
// the deployment. The sentence a caller builds from the row says so.
var ErrRetirementDone = errors.New("state: this deployment has retired a controller")

// ErrRetirementMoved means a conditional write found the row in another state
// than the one the caller read.
//
// A write here is held to the state it leaves (a filter on state, host, id and
// reservation), and zero rows affected is the row having moved between the read
// and the write, on another host or in another process. The caller re-reads and
// decides again; it never overwrites.
var ErrRetirementMoved = errors.New(
	"state: the retirement row is not in the state this write expected")

// ErrRetirementMismatch means a completion document does not describe the row
// it was presented against.
//
// The document carries the transition id, the reservation and both hosts; a row
// that disagrees on any of them belongs to another retirement, and completing
// it on this document's word would close a transition the document never saw.
var ErrRetirementMismatch = errors.New(
	"state: the completion does not match this deployment's retirement row")

// Retirement is the deployment's retirement row as the ledger holds it.
//
// TIMES ARE THE STRINGS WRITTEN, not parsed: the reservation time is compared
// for EQUALITY between a completion document and the row, and a value that went
// through a parse and a re-format could differ in a digit no reader intends.
type Retirement struct {
	Deployment   string
	Retiring     string
	Survivor     string
	Run          string
	State        string
	TransitionID string
	ReservedAt   string
	UpdatedAt    string
	CompletedBy  string
	CompletedAt  string
}

func retirementFromRow(r ledgerdb.ControllerRetirement) Retirement {
	return Retirement{
		Deployment:   r.Deployment,
		Retiring:     r.Retiring,
		Survivor:     r.Survivor,
		Run:          r.Run,
		State:        r.State,
		TransitionID: r.TransitionID,
		ReservedAt:   r.ReservedAt,
		UpdatedAt:    r.UpdatedAt,
		CompletedBy:  r.CompletedBy,
		CompletedAt:  r.CompletedAt,
	}
}

// ledgerTime is the one spelling of a time this table stores.
func ledgerTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ReadRetirement reports the deployment's retirement row.
//
// THE BOOLEAN IS POSITIVE ABSENCE: false with a nil error means the read
// succeeded and found no row. A read that failed returns the error and nothing
// else, so a caller can never mistake a ledger it could not read for one that
// holds no reservation.
func (db *DB) ReadRetirement(ctx context.Context, deployment string) (Retirement, bool, error) {
	var (
		out     Retirement
		present bool
	)

	err := db.View(ctx, func(q Querier) error {
		var err error

		out, present, err = ReadRetirementIn(ctx, ReadQueries(q), deployment)

		return err
	})
	if err != nil {
		return Retirement{}, false, err
	}

	return out, present, nil
}

// ReadRetirementIn is ReadRetirement inside a transaction or a snapshot the
// caller already holds, so a report that reads the binding, the rollout and the
// registrations in one read transaction reads the retirement row in the same
// one. The boolean is positive absence, as in ReadRetirement.
func ReadRetirementIn(ctx context.Context, reads ReadOps, deployment string) (Retirement, bool, error) {
	row, err := reads.ReadRetirement(ctx, deployment)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Retirement{}, false, nil
	case err != nil:
		return Retirement{}, false, fmt.Errorf("state: read the retirement row: %w", err)
	}

	return retirementFromRow(row), true, nil
}

// RetirementReservation is what a request writes before it collects any
// evidence.
type RetirementReservation struct {
	Deployment string
	// Retiring and Survivor are the two hosts' INVENTORY names.
	Retiring string
	Survivor string
	// Run is the holder of the converge making the request.
	Run string
	// TransitionID is minted by the caller at the reservation and never changes
	// for the life of the row; the journal and the guard's marker copy it.
	TransitionID string
	// At is this host's clock at the reservation; reports collected before it
	// are refused by the request that follows.
	At time.Time
}

// ReservationOutcome says how a reservation came to be held.
type ReservationOutcome string

const (
	// ReservationInserted means no row existed and this request wrote one.
	ReservationInserted ReservationOutcome = "reserved"
	// ReservationAdopted means this host's own `reserved` row, left by a run
	// that did not reach its journal, was re-bound to this run. Its transition
	// id and reservation time are the original's.
	ReservationAdopted ReservationOutcome = "adopted"
)

// ReserveRetirement takes the deployment's retirement reservation for this
// host, or refuses on the row that holds it.
//
// READ THEN WRITE, INSIDE ONE WRITE TRANSACTION, so the single writer decides
// between two requests: SQLite holds the write lock from BEGIN IMMEDIATE and
// PostgreSQL from the advisory lock beginWrite takes, and nothing reserves
// between finding no row and inserting one. A `reserved` row for THIS host is
// adopted rather than refused, because the run that inserted it may have died
// before its journal existed and the same host's next request is how it
// recovers; a row for another host, or this host's row past `reserved`,
// refuses with the row so the caller can say who holds it. A `done` row refuses
// with ErrRetirementDone whatever it names.
//
// The row returned on a refusal is the one that refused, so the caller's
// sentence names the right host and date without a second read.
func (db *DB) ReserveRetirement(
	ctx context.Context, r RetirementReservation,
) (Retirement, ReservationOutcome, error) {
	switch {
	case r.Deployment == "" || r.Retiring == "" || r.Survivor == "" || r.Run == "":
		return Retirement{}, "", errors.New(
			"state: a retirement reservation needs the deployment, both hosts and the run")
	case r.TransitionID == "":
		return Retirement{}, "", errors.New(
			"state: a retirement reservation needs its transition id minted first")
	case r.Retiring == r.Survivor:
		return Retirement{}, "", errors.New(
			"state: a retirement's survivor cannot be the retiring host")
	}

	var (
		out     Retirement
		outcome ReservationOutcome
	)

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)
		now := ledgerTime(r.At)

		row, err := q.ReadRetirement(ctx, r.Deployment)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := q.InsertRetirement(ctx, ledgerdb.InsertRetirementParams{
				Deployment:   r.Deployment,
				Retiring:     r.Retiring,
				Survivor:     r.Survivor,
				Run:          r.Run,
				State:        RetirementReserved,
				TransitionID: r.TransitionID,
				ReservedAt:   now,
				UpdatedAt:    now,
			}); err != nil {
				return fmt.Errorf("insert the reservation: %w", err)
			}

			out = Retirement{
				Deployment: r.Deployment, Retiring: r.Retiring, Survivor: r.Survivor,
				Run: r.Run, State: RetirementReserved, TransitionID: r.TransitionID,
				ReservedAt: now, UpdatedAt: now,
			}
			outcome = ReservationInserted

			return nil
		case err != nil:
			return fmt.Errorf("read the retirement row: %w", err)
		}

		out = retirementFromRow(row)

		switch {
		case row.State == RetirementDone:
			return ErrRetirementDone
		case row.Retiring != r.Retiring || row.State != RetirementReserved:
			return ErrRetirementReserved
		}

		res, err := q.AdoptRetirement(ctx, ledgerdb.AdoptRetirementParams{
			Run: r.Run, UpdatedAt: now,
			Deployment: r.Deployment, Retiring: r.Retiring, Reserved: RetirementReserved,
		})
		if err != nil {
			return fmt.Errorf("adopt the reservation: %w", err)
		}

		if err := oneRowMoved(res); err != nil {
			return err
		}

		out.Run, out.UpdatedAt = r.Run, now
		outcome = ReservationAdopted

		return nil
	})

	switch {
	case errors.Is(err, ErrRetirementDone), errors.Is(err, ErrRetirementReserved):
		// THE ROW TRAVELS WITH THE REFUSAL, unwrapped: the caller matches the
		// sentinel and reads the row for its sentence.
		return out, "", err
	case err != nil:
		return Retirement{}, "", fmt.Errorf("state: reserve the retirement: %w", err)
	}

	return out, outcome, nil
}

// oneRowMoved turns a conditional write's result into ErrRetirementMoved when
// it matched nothing.
//
// A driver that cannot count rows is a fault, not a match: both engines billet
// runs on report the count, and an unknown count is no proof the write landed.
func oneRowMoved(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count the rows the write changed: %w", err)
	}

	if n != 1 {
		return ErrRetirementMoved
	}

	return nil
}

// AdvanceRetirementToIntent moves this host's `reserved` row to `intent`.
//
// CALLED AFTER THE JOURNAL'S `intent` IS DURABLE, never before: the journal is
// the retiring host's own record and the row is the shared one, and a crash
// between the two leaves a `reserved` row beside a journal the resume validates
// and advances through this same write. The transition id is part of the
// filter, so a journal minted under another reservation cannot advance this
// row; ErrRetirementMoved is the row not being this host's `reserved` row with
// that id any more.
func (db *DB) AdvanceRetirementToIntent(
	ctx context.Context, deployment, retiring, transitionID, run string, at time.Time,
) error {
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := WriteQueries(tx).AdvanceRetirementToIntent(ctx,
			ledgerdb.AdvanceRetirementToIntentParams{
				Intent: RetirementIntent, Run: run, UpdatedAt: ledgerTime(at),
				Deployment: deployment, Retiring: retiring,
				TransitionID: transitionID, Reserved: RetirementReserved,
			})
		if err != nil {
			return err
		}

		return oneRowMoved(res)
	})

	switch {
	case errors.Is(err, ErrRetirementMoved):
		return err
	case err != nil:
		return fmt.Errorf("state: advance the retirement row to intent: %w", err)
	}

	return nil
}

// RetirementCompletion is the completion document's binding fields, presented
// by the retiring host at its journal's `done` or by the survivor on its behalf.
type RetirementCompletion struct {
	Deployment   string
	Retiring     string
	Survivor     string
	TransitionID string
	// ReservedAt is the row's reservation time AS THE ROW SPELLS IT, copied into
	// the journal at intent and into the document from there.
	ReservedAt string
	// CompletedBy names the host that made this write: the retiring host itself,
	// or the survivor completing a row the retiring host could not.
	CompletedBy string
	At          time.Time
}

// CompletionOutcome says what a completion did.
type CompletionOutcome string

const (
	// CompletionDone means the row was moved to `done` by this write, or written
	// as `done` where the read found it positively absent.
	CompletionDone CompletionOutcome = "done"
	// CompletionAlready means the row was already `done` under this transition
	// id, and nothing was written.
	CompletionAlready CompletionOutcome = "already"
)

// CompleteRetirement moves the deployment's row to `done` on a completion
// document, or refuses on the row it found.
//
// THE READ DECIDES THE BRANCH AND THE WRITE IS HELD TO IT. An `intent` or a
// `reserved` row for the retiring host whose transition id, reservation and
// survivor equal the document's is advanced (the filter repeats every one of
// them, so a row that moved between the read and the write is ErrRetirementMoved
// and never overwritten); a `done` row with the same id is idempotent; a row for
// this host that disagrees on any binding field is another retirement's and
// refuses ErrRetirementMismatch; a row for another host refuses
// ErrRetirementReserved, or ErrRetirementDone when it is complete, and is left
// exactly as it was. A POSITIVELY ABSENT row (the read succeeded and found none:
// a ledger restored from before the reservation) is RECONSTRUCTED as `done`
// from the document, with no run, because the deployment did retire this host
// and a later request must find the record that says so.
//
// A transition id is required on every branch, including the `reserved` one:
// the id is minted at the reservation and stored in the row's insert, so a
// document whose id the row does not carry is not this row's.
func (db *DB) CompleteRetirement(
	ctx context.Context, c RetirementCompletion,
) (Retirement, CompletionOutcome, error) {
	switch {
	case c.Deployment == "" || c.Retiring == "" || c.Survivor == "" ||
		c.TransitionID == "" || c.ReservedAt == "" || c.CompletedBy == "":
		return Retirement{}, "", errors.New(
			"state: a retirement completion needs every binding field and who completed it")
	case c.Retiring == c.Survivor:
		return Retirement{}, "", errors.New(
			"state: a retirement's survivor cannot be the retiring host")
	}

	var (
		out     Retirement
		outcome CompletionOutcome
	)

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)
		now := ledgerTime(c.At)

		row, err := q.ReadRetirement(ctx, c.Deployment)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := q.InsertRetirement(ctx, ledgerdb.InsertRetirementParams{
				Deployment: c.Deployment, Retiring: c.Retiring, Survivor: c.Survivor,
				Run: "", State: RetirementDone, TransitionID: c.TransitionID,
				ReservedAt: c.ReservedAt, UpdatedAt: now,
				CompletedBy: c.CompletedBy, CompletedAt: now,
			}); err != nil {
				return fmt.Errorf("reconstruct the retirement row: %w", err)
			}

			out = Retirement{
				Deployment: c.Deployment, Retiring: c.Retiring, Survivor: c.Survivor,
				State: RetirementDone, TransitionID: c.TransitionID,
				ReservedAt: c.ReservedAt, UpdatedAt: now,
				CompletedBy: c.CompletedBy, CompletedAt: now,
			}
			outcome = CompletionDone

			return nil
		case err != nil:
			return fmt.Errorf("read the retirement row: %w", err)
		}

		out = retirementFromRow(row)

		if row.Retiring != c.Retiring {
			if row.State == RetirementDone {
				return ErrRetirementDone
			}

			return ErrRetirementReserved
		}

		switch {
		case row.TransitionID != c.TransitionID:
			return fmt.Errorf("%w: the row's transition id is %s, the completion's %s",
				ErrRetirementMismatch, row.TransitionID, c.TransitionID)
		case row.State == RetirementDone:
			outcome = CompletionAlready

			return nil
		case row.ReservedAt != c.ReservedAt:
			return fmt.Errorf("%w: the row was reserved at %s, the completion says %s",
				ErrRetirementMismatch, row.ReservedAt, c.ReservedAt)
		case row.Survivor != c.Survivor:
			return fmt.Errorf("%w: the row's survivor is %s, the completion's %s",
				ErrRetirementMismatch, row.Survivor, c.Survivor)
		}

		res, err := q.CompleteRetirement(ctx, ledgerdb.CompleteRetirementParams{
			Done: RetirementDone, CompletedBy: c.CompletedBy, CompletedAt: now, UpdatedAt: now,
			Deployment: c.Deployment, Retiring: c.Retiring, Survivor: c.Survivor,
			TransitionID: c.TransitionID, ReservedAt: c.ReservedAt,
			Reserved: RetirementReserved, Intent: RetirementIntent,
		})
		if err != nil {
			return fmt.Errorf("complete the retirement row: %w", err)
		}

		if err := oneRowMoved(res); err != nil {
			return err
		}

		out.State, out.CompletedBy, out.CompletedAt, out.UpdatedAt = RetirementDone,
			c.CompletedBy, now, now
		outcome = CompletionDone

		return nil
	})

	switch {
	case errors.Is(err, ErrRetirementDone), errors.Is(err, ErrRetirementReserved),
		errors.Is(err, ErrRetirementMismatch), errors.Is(err, ErrRetirementMoved):
		return out, "", err
	case err != nil:
		return Retirement{}, "", fmt.Errorf("state: complete the retirement: %w", err)
	}

	return out, outcome, nil
}

// ReleaseRetirement deletes the `reserved` row this run holds for this host.
//
// THE ONE DELETE, scoped to the run as well as the host and the state: a request
// refused after its own insert releases what it inserted, an operator's
// abandonment releases what the named run holds, and a row adopted by another
// run, another host's row, or a row that reached `intent` is ErrRetirementMoved
// and untouched.
func (db *DB) ReleaseRetirement(ctx context.Context, deployment, retiring, run string) error {
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := WriteQueries(tx).DeleteReservedRetirement(ctx,
			ledgerdb.DeleteReservedRetirementParams{
				Deployment: deployment, Retiring: retiring, Run: run, Reserved: RetirementReserved,
			})
		if err != nil {
			return err
		}

		return oneRowMoved(res)
	})

	switch {
	case errors.Is(err, ErrRetirementMoved):
		return err
	case err != nil:
		return fmt.Errorf("state: release the retirement reservation: %w", err)
	}

	return nil
}
