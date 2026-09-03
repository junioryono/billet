package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// AdmissionMode is whether the deployment is accepting new work.
//
// UNKNOWN IS A VALUE, and it is the zero value deliberately. A caller that
// cannot read the ledger has not learned that admission is open, and the whole
// point of a seal is that the failure to observe it must not become permission
// to admit. Every consumer therefore has to say what it does about Unknown,
// rather than inheriting an answer from a bool.
type AdmissionMode int

const (
	AdmissionUnknown AdmissionMode = iota
	AdmissionOpen
	AdmissionSealed
)

func (m AdmissionMode) String() string {
	switch m {
	case AdmissionOpen:
		return "open"
	case AdmissionSealed:
		return "sealed"
	case AdmissionUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Provenance says who took a seal, which decides who may clear it.
const (
	// ProvenanceLocalDown is a seal held by a lifecycle command for the duration
	// of its own shutdown. A later successful `billet local up` clears it.
	ProvenanceLocalDown = "local-down"
	// ProvenanceOperator is a seal somebody took deliberately. It survives a
	// control-plane restart and a lifecycle command, and only an explicit resume
	// clears it — silently reopening admission because a service was restarted is
	// exactly the failure this exists to prevent.
	ProvenanceOperator = "operator"
)

// ErrAdmissionGeneration means the seal changed between reading it and acting on
// it, so the caller was about to undo a decision it never saw.
var ErrAdmissionGeneration = errors.New("state: the admission generation moved")

// ErrAdmissionProvenance means the caller is not entitled to clear this seal.
// It is a sentinel so a command can add the remedy for its own case — the state
// layer knows which provenance holds the seal, and only the caller knows which
// command clears it.
var ErrAdmissionProvenance = errors.New("state: this seal was taken by somebody else")

// Admission is the deployment's current admission state.
type Admission struct {
	Mode       AdmissionMode
	Generation int64
	Provenance string
	Reason     string
	Actor      string
	ChangedAt  string
}

// Sealed reports whether new work must be refused. It answers true for Unknown:
// a state billet could not read is not a state it may admit against.
func (a Admission) Sealed() bool { return a.Mode != AdmissionOpen }

// ReadAdmission reads the admission state through any querier, so the same
// answer serves a status command on the read-only pool and an allocation
// decision inside its own write transaction.
//
// The caller decides what an error means. This does NOT fold a failed read into
// a mode, because the two are different facts: a read that failed says nothing
// about the deployment, and a caller that must fail closed and a caller that
// must report uncertainty need to tell them apart.
func ReadAdmission(ctx context.Context, q Querier) (Admission, error) {
	row, err := ReadQueries(q).ReadAdmission(ctx)
	if err != nil {
		// NOT a mode. The row is inserted by the migration that creates the table,
		// so its absence means something is wrong with the ledger rather than that
		// admission is open.
		return Admission{}, fmt.Errorf("state: read admission: %w", err)
	}

	a := Admission{
		Generation: row.Generation,
		Provenance: row.Provenance,
		Reason:     row.Reason,
		Actor:      row.Actor,
		ChangedAt:  row.ChangedAt,
	}

	switch mode := row.Mode; mode {
	case "open":
		a.Mode = AdmissionOpen
	case "sealed":
		a.Mode = AdmissionSealed
	default:
		// The CHECK constraint makes this unreachable through billet, which is
		// why it stays Unknown rather than being trusted: a value that got past
		// the schema is not one to guess about.
		a.Mode = AdmissionUnknown
	}

	return a, nil
}

// Admission reads the current admission state on the read-only pool.
func (db *DB) Admission(ctx context.Context) (Admission, error) {
	var a Admission

	err := db.View(ctx, func(q Querier) error {
		var err error
		a, err = ReadAdmission(ctx, q)

		return err
	})

	return a, err
}

// SealRequest is one decision to stop admitting work.
type SealRequest struct {
	// Expect is the generation the caller believes is current. A seal that finds
	// a different one has been overtaken and refuses rather than overwriting
	// somebody else's decision.
	Expect int64
	// Provenance decides who may clear this seal.
	Provenance string
	// Reason and Actor are what an operator reads later when they find their
	// deployment admitting nothing. Neither is optional in practice: a seal
	// nobody can attribute is one nobody dares clear.
	Reason string
	Actor  string
	// KeepExisting asks for "make sure this is sealed" rather than "seal this":
	// when a seal of the same provenance is already held, keep it, leave the
	// generation where it is, and return what is there.
	//
	// IT IS OPT-IN BECAUSE IT CHANGES WHAT A SEAL MEANS. Applied to every caller,
	// a second operator deliberately resealing with a new reason would silently
	// do nothing, and — worse — the generation would stop moving, so a fence
	// another operator was holding would still look current after somebody else
	// had taken the seal. An existing test caught exactly that.
	//
	// The caller that wants it is an idempotent command: `billet drain` run twice
	// must not rewrite somebody's attribution or invalidate their fence. It has to
	// be decided HERE rather than by the command reading first, because between
	// that read and this transaction another operator can resume, and the command
	// would report "already sealed" against a deployment that is now open.
	KeepExisting bool
}

// Seal stops the deployment admitting new work, and returns the state it wrote.
func (db *DB) Seal(ctx context.Context, req SealRequest) (Admission, error) {
	if req.Provenance != ProvenanceLocalDown && req.Provenance != ProvenanceOperator {
		return Admission{}, fmt.Errorf("state: seal provenance %q is not one billet issues",
			req.Provenance)
	}

	return db.transition(ctx, transition{
		mode: "sealed", expect: req.Expect, provenance: req.Provenance,
		reason: req.Reason, actor: req.Actor, keepExisting: req.KeepExisting,
	})
}

// ResumeRequest is one decision to admit work again.
type ResumeRequest struct {
	Expect int64
	// Clears names the provenance this caller is entitled to undo. A lifecycle
	// command may clear its own seal and must not clear an operator's.
	Clears string
	Actor  string
}

// Resume lets the deployment admit work again.
//
// IT WILL NOT CLEAR A SEAL IT DID NOT TAKE. A `billet local up` that reopened an
// operator's maintenance seal because it happened to restart the services would
// admit work into a deployment somebody had deliberately quiesced, and would do
// it silently — the operator's evidence would be a job running during their
// maintenance window.
func (db *DB) Resume(ctx context.Context, req ResumeRequest) (Admission, error) {
	return db.transition(ctx, transition{
		mode: "open", expect: req.Expect, clears: req.Clears, actor: req.Actor,
	})
}

func byWhom(a Admission) string {
	if a.Actor == "" {
		return ""
	}

	return " (" + a.Actor + ")"
}

// transition is one decision to change admission.
type transition struct {
	mode   string
	expect int64
	// clears, when set, is the provenance the caller is entitled to undo.
	clears string
	// keepExisting makes an already-held seal of the same provenance a no-op.
	keepExisting bool
	provenance   string
	reason       string
	actor        string
}

// setAdmission writes one transition, refusing if the generation moved.
//
// THE READ AND THE WRITE ARE ONE TRANSACTION, for the same reason the headroom
// check and the lease insert are: a compare followed by a hopeful update is not
// a compare-and-swap, and the thing it fails to exclude is the second operator.
func (db *DB) transition(ctx context.Context, t transition) (Admission, error) {
	var out Admission

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		current, err := ReadAdmission(ctx, tx)
		if err != nil {
			return err
		}

		// ALREADY SEALED BY THE SAME KIND OF CALLER IS NOT A CHANGE — for a caller
		// that asked for that. See SealRequest.KeepExisting for why it is opt-in.
		//
		// Like the open case below, it does not consult the generation: the
		// caller's intent — that this deployment be sealed by this kind of holder —
		// is already satisfied, whoever most recently satisfied it.
		if t.keepExisting && t.mode == "sealed" && current.Mode == AdmissionSealed &&
			current.Provenance == t.provenance {
			out = current

			return nil
		}

		if t.mode == "open" {
			// ALREADY THERE IS NOT A CHANGE. A lifecycle command that always
			// resumes on its way out must not move the generation by doing
			// nothing, or it invalidates an unrelated operator's fence as a side
			// effect.
			if current.Mode == AdmissionOpen {
				out = current

				return nil
			}

			// AND A ROW BILLET CANNOT READ IS NOT ONE IT MAY OPEN. Resuming from
			// an unrecognised mode would turn "I could not tell what this says"
			// into "admit work", which is the collapse the whole three-valued
			// answer exists to prevent — and it would do it while bypassing the
			// provenance check, since an unreadable row has no provenance to
			// authorise against.
			if current.Mode != AdmissionSealed {
				return fmt.Errorf("state: admission reads %q, which billet does not recognise, "+
					"so it will not open it. Resolve the admission row before resuming",
					current.Mode)
			}
		}

		if current.Generation != t.expect {
			return fmt.Errorf("%w: expected %d, found %d. Something changed admission between "+
				"reading it and acting on it; look at `billet status` and run this again",
				ErrAdmissionGeneration, t.expect, current.Generation)
		}

		// AUTHORISED INSIDE THE SAME TRANSACTION AS THE WRITE, against the row the
		// write will act on. Checking provenance on a row read separately proves
		// nothing about the row updated afterwards: an operator resealing in
		// between would have their seal cleared by a lifecycle command that had
		// authorised itself against the seal it replaced.
		if t.clears != "" && current.Provenance != t.clears {
			return fmt.Errorf("%w: admission was sealed by %s%s, and this may only "+
				"clear a %s seal", ErrAdmissionProvenance, current.Provenance,
				byWhom(current), t.clears)
		}

		next := current.Generation + 1

		if err := WriteQueries(tx).SetAdmission(ctx, ledgerdb.SetAdmissionParams{
			Mode:       t.mode,
			Generation: next,
			Provenance: t.provenance,
			Reason:     t.reason,
			Actor:      t.actor,
			ChangedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Expect:     current.Generation,
		}); err != nil {
			return fmt.Errorf("state: write admission: %w", err)
		}

		out, err = ReadAdmission(ctx, tx)

		return err
	})
	if err != nil {
		return Admission{}, err
	}

	return out, nil
}
