package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// THE TAIL IS WHAT A RETIREMENT STILL OWES ONCE ITS JOURNAL READS `done`: the
// published status, the node's endpoint receipt, the ledger row, the guard's
// marker, and `settled`. The host is already retired at this point — the
// server is stopped, the identity archived, the configuration rewritten — so
// nothing here is destructive and nothing here can fail in a way that leaves
// the host half-transitioned; what it can do is leave an OBLIGATION, which the
// next converge picks up from the same journal.
//
// THE ORDER IS LOCAL WORK FIRST, and it is not a preference: the status and
// the receipt need nothing from another host, so a run that cannot reach the
// ledger still leaves this host's own records true, and the ledger row is the
// one step that depends on a database this host may no longer be able to
// write. The marker is cleared only once the row is done, because the marker
// is what stops `converge-guard release` from taking the guard away from a
// retirement that still owes the ledger a row; and `settled` is written after
// the marker, so a crash between the two leaves an unsettled journal with no
// marker — the one state the guard's takeover rule reads as "the tail is
// unfinished", rather than an abandoned reservation.
//
// ZERO LEDGER OPENS ONCE THE ROW IS ACKNOWLEDGED. A `done` journal carrying
// `row_done` needs nothing from the database again, and every later converge
// of a retired host must be able to run with the ledger unreachable.

// retireTailAnswer is the answer of a run that reached `done`: what the
// transition did, what the tail finished, and what it left for the role.
type retireTailAnswer struct {
	Schema       int                `json:"schema"`
	Outcome      string             `json:"outcome"`
	Phase        retirement.Phase   `json:"phase"`
	Variant      retirement.Variant `json:"variant"`
	TransitionID string             `json:"transition_id"`
	Steps        []retireStep       `json:"steps"`
	Status       retirement.Phase   `json:"status"`
	// Receipt says what the node's endpoint receipt needed: `written`,
	// `current`, or `none` on a host that keeps no node.
	Receipt string `json:"receipt"`
	// Row is `done`, `already` or `pending`; RowWhy says why a pending one
	// could not be written here.
	Row         string `json:"row"`
	RowWhy      string `json:"row_why,omitempty"`
	CompletedBy string `json:"completed_by"`
	// Completion is the document the survivor's `--complete-row` takes when
	// this host could not write the row itself. It is present on every answer,
	// because the role does not decide from the row's word alone.
	Completion *retirement.Completion `json:"completion"`
	// Marker is `cleared` or `kept`.
	Marker  string `json:"marker"`
	Settled bool   `json:"settled"`
	State   string `json:"state"`
}

// The words the tail answers with.
const (
	retireOutcomeRetired = "retired"

	retireRowDone    = "done"
	retireRowAlready = "already"
	retireRowPending = "pending"

	retireReceiptWritten = "written"
	retireReceiptCurrent = "current"
	retireReceiptNone    = "none"

	retireMarkerCleared = "cleared"
	retireMarkerKept    = "kept"
	retireMarkerAbsent  = "absent"
)

// retireReceiptWait bounds the tail's wait for the restarted node's own
// runtime record. The node was started by the phase before this one, and its
// record appears at its first registration.
var retireReceiptWait = time.Minute

// retireTail finishes everything the retirement owes after `done` and answers
// what it left. The journal it is given has already been validated as this
// host's, this guard's and this transition's.
func retireTail(ctx context.Context, m retireMode, root *txLock, dir *os.File, j retirement.Journal,
	steps []retireStep,
) (any, *retireRefusal) {
	answer := &retireTailAnswer{Schema: retireSchema, Outcome: retireOutcomeRetired, Phase: j.Phase,
		Variant: j.Variant, TransitionID: j.Provenance.TransitionID, Steps: steps,
		Status: retirement.PhaseDone, Marker: retireMarkerKept}

	if r := retireTailStatus(j); r != nil {
		return nil, r
	}

	receipt, r := retireTailReceipt(ctx, m, j)
	if r != nil {
		return nil, r
	}

	answer.Receipt = receipt

	// THE COMPLETION IS EMITTED WHATEVER THE ROW SAYS. It is a statement about
	// this retirement and not about the write that follows, and the role hands
	// it to the survivor exactly when the row comes back pending.
	completion := retirement.CompletionOf(j)
	answer.Completion = &completion

	j, r = retireTailRow(ctx, j, answer)
	if r != nil {
		return nil, r
	}

	if !j.RowDone {
		// THE MARKER STAYS and the journal is unsettled: the survivor writes
		// the row, this host acknowledges it, and the next converge's tail
		// clears the marker and settles.
		return answer, nil
	}

	cleared, r := retireClearMarker(root, dir, m.run, j)
	if r != nil {
		return nil, r
	}

	answer.Marker = cleared

	if r := retireMarkSettled(&j); r != nil {
		return nil, r
	}

	answer.Settled = true

	return answer, nil
}

// retireTailStatus publishes `done` when the status is below it or cannot be
// read as a phase at all. A status that already reads `done` is left alone,
// because rewriting it would move its `updated_at` on every converge of a
// retired host for no fact anyone reads.
func retireTailStatus(j retirement.Journal) *retireRefusal {
	st, presence, err := retirement.ReadStatus()

	switch presence {
	case retirement.StatusPresent:
		if st.Phase == retirement.PhaseDone && st.Variant == j.Variant {
			return nil
		}
	case retirement.StatusAbsent, retirement.StatusMalformed:
		// A MALFORMED STATUS IS REPAIRED RATHER THAN REFUSED: the journal is
		// the record, the status is its publication, and this run knows what
		// it should say. An ABSENT one is the same repair — the file is what
		// closes the authority to ordinary writers, and a retired host whose
		// status vanished is a host an ordinary `ca rotate` would be admitted
		// on.
	default:
		return retireUnknown(retireReasonStatus, "the published status could not be read: "+err.Error(), "")
	}

	if err := retirement.WriteStatus(retirement.PhaseDone, j.Variant, retireNow()); err != nil {
		return retireUnknown(retireReasonStatus, "publish the status: "+err.Error(), "")
	}

	return nil
}

// retireTailReceipt keeps the retained node's endpoint receipt current.
//
// THE NODE WAS RESTARTED BY THE PHASE BEFORE THIS ONE, so its invocation is
// new and the configuration it loaded is the serverless one: a receipt from
// before the retirement names an invocation that is gone and a digest that has
// changed, and the fleet's next check refuses on it. The refresh is the same
// one every ordinary converge ends with, run here because a converge that
// retires this host ends at the retirement and never reaches that task.
func retireTailReceipt(ctx context.Context, m retireMode, j retirement.Journal) (string, *retireRefusal) {
	if j.Variant != retirement.VariantRetainedNode {
		return retireReceiptNone, nil
	}

	answer, r := refreshReceipt(ctx, receiptMode{configPath: m.configPath, run: m.run, refresh: true,
		wait: retireReceiptWait})
	if r != nil {
		return "", retireFromEndpointFor(retireReasonReceipt, r)
	}

	written, ok := answer.(receiptAnswer)
	if !ok {
		return "", retireUnknown(retireReasonReceipt, "the receipt's refresh answered a shape this command does not know", "")
	}

	if written.Outcome == outcomeWritten {
		return retireReceiptWritten, nil
	}

	return retireReceiptCurrent, nil
}

// retireTailRow completes this retirement's ledger row from this host, and
// records the acknowledgement in the journal when the write lands.
//
// THE HOST WRITES ITS OWN ROW WHEN IT CAN. It is the party that knows the
// transition finished, and the survivor's helper exists for the case this call
// cannot be made: a frozen controller whose binary is older than the schema
// the survivor migrated to, a DSN that no longer resolves, an environment file
// the rewrite took away. Those are PENDING — the retirement is complete on
// this host and the ledger has not heard yet — and everything else is a
// refusal, because a ledger that disagrees with this journal is not a thing to
// wait out.
func retireTailRow(ctx context.Context, j retirement.Journal, answer *retireTailAnswer,
) (retirement.Journal, *retireRefusal) {
	if j.RowDone {
		answer.Row = retireRowAlready
		answer.CompletedBy = j.CompletedBy

		return j, nil
	}

	db, why, r := retireOpenLedgerByLocator(ctx, j)

	switch {
	case r != nil:
		return j, r
	case db == nil:
		answer.Row = retireRowPending
		answer.RowWhy = why

		return j, nil
	}

	defer func() { _ = db.Close() }()

	row, outcome, err := db.CompleteRetirement(ctx, state.RetirementCompletion{
		Deployment: j.Deployment, Retiring: j.Retiring, Survivor: j.Survivor.Host,
		TransitionID: j.Provenance.TransitionID, ReservedAt: j.Provenance.Reservation,
		CompletedBy: j.Retiring, At: retireNow(),
	})

	switch {
	case errors.Is(err, state.ErrRetirementDone):
		return j, retireUnknown(retireReasonRetired, fmt.Sprintf("this deployment's retirement row is %s's and this host "+
			"is %s, so the row this transition would complete is not there", row.Retiring, j.Retiring),
			"the runbook in docs/operating/upgrades.md")
	case errors.Is(err, state.ErrRetirementReserved):
		return j, retireUnknown(retireReasonConflict, fmt.Sprintf("this deployment's retirement row is %s's (run %s, "+
			"state %s), and this host's transition is complete", row.Retiring, row.Run, row.State),
			"the runbook in docs/operating/upgrades.md")
	case errors.Is(err, state.ErrRetirementMismatch), errors.Is(err, state.ErrRetirementMoved):
		return j, retireUnknown(retireReasonMismatch, err.Error(), "the runbook in docs/operating/upgrades.md")
	case err != nil:
		if pending := retirePendingReason(err); pending != "" {
			answer.Row = retireRowPending
			answer.RowWhy = pending

			return j, nil
		}

		return j, retireUnknown(retireReasonLedger, "complete the retirement row: "+err.Error(), "")
	}

	j.RowDone = true
	j.CompletedBy = row.CompletedBy

	if err := j.Write(retireNow()); err != nil {
		// THE ROW IS WRITTEN AND THE JOURNAL DOES NOT SAY SO. The next
		// converge's tail reaches the same call, which answers `already` from
		// the row itself, so nothing is lost; what must not happen is the
		// marker being cleared over a journal that has not recorded it.
		return j, retireUnknown(retireReasonJournal, "record the completed row in the journal: "+err.Error(), "")
	}

	// THE LEDGER'S WORD IS TRANSLATED, not passed through: this answer's
	// vocabulary is the command's, and a word the ledger adds later must not
	// reach the role as one of the three its parser knows.
	switch outcome {
	case state.CompletionDone:
		answer.Row = retireRowDone
	case state.CompletionAlready:
		answer.Row = retireRowAlready
	default:
		return j, retireUnknown(retireReasonLedger, fmt.Sprintf("the ledger answered the completion %q, which this "+
			"command does not know", outcome), "")
	}

	answer.CompletedBy = j.CompletedBy

	return j, nil
}

// retireOpenLedgerByLocator opens the ledger the way a host past the archive
// must: from the journal's locator, because the installed configuration has no
// `server:` any more and the identity is at the archive.
//
// IT ANSWERS THREE WAYS. A handle; nil with a reason, which is a PENDING row
// and not a failure of this converge; or a refusal, which is everything this
// command cannot classify.
func retireOpenLedgerByLocator(ctx context.Context, j retirement.Journal) (*state.DB, string, *retireRefusal) {
	if j.Locator.Backend != string(config.StatePostgres) {
		return nil, "", retireUnknown(retireReasonLedger, fmt.Sprintf("the journal's locator names the backend %q, and a "+
			"retirement is defined for PostgreSQL", j.Locator.Backend), "")
	}

	dsn, why, r := retireLocatorDSN(j.Locator)

	switch {
	case r != nil:
		return nil, "", r
	case why != "":
		return nil, why, nil
	}

	db, err := state.OpenPostgresAdmin(ctx, j.Locator.Archive, dsn, state.WithRunningRelease(version.Version()))
	if err != nil {
		if pending := retirePendingReason(err); pending != "" {
			return nil, pending, nil
		}

		return nil, "", retireUnknown(retireReasonLedger, "open the ledger from the journal's locator: "+err.Error(), "")
	}

	// AND IT IS THIS DEPLOYMENT'S LEDGER. The locator names the archive the
	// identity moved to, so the binding is asked of the identity this
	// retirement recorded, never of a directory at the configured path that
	// something else may have created since.
	if err := db.VerifyDeploymentBinding(ctx, j.Deployment); err != nil {
		return nil, "", retireUnknown(retireReasonLedger, err.Error(), "")
	}

	return db, "", nil
}

// retireLocatorDSN reads the connection string the locator names, from the
// environment file when it names one and from this process's environment
// otherwise. A file that is gone or unreadable is a PENDING row: the rewrite
// took the server's environment away and the survivor completes instead.
func retireLocatorDSN(loc retirement.JournalLocator) (string, string, *retireRefusal) {
	if loc.DSNEnv == "" {
		return "", "", retireUnknown(retireReasonLedger, "the journal's locator names no DSN variable, so this host "+
			"cannot reach the ledger it retired from", "the runbook in docs/operating/upgrades.md")
	}

	if loc.EnvironmentFile == "" {
		value := os.Getenv(loc.DSNEnv)
		if value == "" {
			return "", fmt.Sprintf("%s is not set in this process's environment", loc.DSNEnv), nil
		}

		return value, "", nil
	}

	value, found, err := environmentFileValue(loc.EnvironmentFile, loc.DSNEnv)

	switch {
	case err != nil:
		return "", fmt.Sprintf("%s could not be read (%s)", loc.EnvironmentFile, err), nil
	case !found || value == "":
		return "", fmt.Sprintf("%s does not set %s", loc.EnvironmentFile, loc.DSNEnv), nil
	}

	return value, "", nil
}

// retirePendingReason is the CLOSED LIST of ways the ledger can be out of this
// host's reach without anything being wrong: the database did not answer, or
// it holds a schema or a release this binary may not write. Everything else —
// a checksum that does not match, a bookkeeping table that is not what it
// should be — is could-not-tell and refuses, because those say something about
// the ledger rather than about this host's distance from it.
func retirePendingReason(err error) string {
	switch {
	case errors.Is(err, state.ErrUnreachable):
		return "the ledger's database did not answer (" + err.Error() + ")"
	case errors.Is(err, state.ErrSchemaAhead):
		return "the ledger's schema is newer than this binary's, so this host may not write it"
	case errors.Is(err, state.ErrSchemaBehind):
		return "the ledger's schema is older than this binary's and no control plane has migrated it here"
	case errors.Is(err, state.ErrReleaseBehind):
		return "a newer billet has served this ledger, so this host may not write it"
	default:
		return ""
	}
}

// retireClearMarker takes the retirement's marker off the guard's record. The
// guard itself STAYS: it is this converge's, and the converge releases it the
// way every other one does.
//
// THE RECORD IS READ AGAIN HERE, never the shape this run classified before it
// began: the request writes the marker itself at `intent`, and a takeover
// rewrites the whole record, so the copy taken at the start is behind by at
// least one of this run's own writes. Reading it again is also what re-checks
// that the guard is still this converge's before anything is written to it.
func retireClearMarker(root *txLock, dir *os.File, run string, j retirement.Journal) (string, *retireRefusal) {
	shape, err := classifyGuardDirFrom(dir)
	if err != nil {
		return "", retireUnknown(retireReasonGuard, "read the guard's record before clearing the marker: "+err.Error(), "")
	}

	if r := judgeGuardShape(shape, run); r != nil {
		return "", r
	}

	marker := shape.Guard.Transition
	if marker == nil {
		return retireMarkerAbsent, nil
	}

	if marker.ID != j.Provenance.TransitionID {
		return "", retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s and this "+
			"retirement is %s; the marker is kept", marker.ID, j.Provenance.TransitionID), "")
	}

	record := shape.Guard
	record.Transition = nil

	if err := writeGuardRecordAt(dir, record, true); err != nil {
		return "", retireUnknown(retireReasonMarker, "clear the retirement's marker from the guard: "+err.Error(), "")
	}

	if err := syncDirFD(dir); err != nil {
		return "", retireUnknown(retireReasonMarker, "flush the guard directory: "+err.Error(), "")
	}

	if err := syncDirFD(root.dir); err != nil {
		return "", retireUnknown(retireReasonMarker, "flush the upgrade root: "+err.Error(), "")
	}

	return retireMarkerCleared, nil
}

// retireMarkSettled is the tail's last write, AFTER the marker is gone: a
// crash between the two leaves an unsettled journal beside a guard with no
// marker, which is the state the takeover rule reads as an unfinished tail.
// The reverse order would leave a settled journal beside a marker nothing
// clears.
func retireMarkSettled(j *retirement.Journal) *retireRefusal {
	j.Settled = true

	if err := j.Write(retireNow()); err != nil {
		return retireUnknown(retireReasonJournal, "record the retirement as settled: "+err.Error(), "")
	}

	return nil
}
