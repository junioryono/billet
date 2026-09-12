package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE REQUEST (`--input -`): the input ingested whole, then under the
// transaction lock and this converge's guard the journal re-read, and under
// the identity exclusion the row read and the table dispatched; a request
// proper judges its eligibility ONCE, from the reports the role collected
// after the reservation, and records the decision in `intent` before anything
// is stopped: the guard's marker, the stage (a retained node), the journal,
// the row, the status, in that order. The transition's phases after intent
// follow in the next change.

// The keys the serverless rendering drops from the installed configuration:
// exactly the ones config.Validate refuses without a server.
var serverlessDroppedKeys = []string{"server", "github", "targets", "backup"}

// retirePlan is the request's decision as judged, everything `intent`
// records.
type retirePlan struct {
	variant   retirement.Variant
	rendering []byte
	survivor  retirement.JournalSurvivor
	nodes     []retirement.JournalNode
	failover  bool
	identity  string
	row       state.Retirement
	cfg       *config.Config
	installed string
	archive   string
}

// retireIntentReport is the dry run's answer over a request's input: what a
// real run would do, and the decision it would record.
type retireIntentReport struct {
	Schema           int                        `json:"schema"`
	Outcome          string                     `json:"outcome"`
	Would            string                     `json:"would"`
	Variant          retirement.Variant         `json:"variant"`
	Survivor         retirement.JournalSurvivor `json:"survivor"`
	Nodes            []retirement.JournalNode   `json:"nodes"`
	FailoverVerified bool                       `json:"endpoint_failover_verified"`
	TransitionID     string                     `json:"transition_id"`
	State            string                     `json:"state"`
	// Unlocked says what this report could not judge, because a preview holds
	// nothing.
	Unlocked string `json:"unlocked"`
}

// retireRequest is `--input -`.
func retireRequest(ctx context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(maxRetireInputBytes)
	if r != nil {
		return nil, r
	}

	in, r := decodeRetireInput(raw)
	if r != nil {
		return nil, r
	}

	// A DRY RUN TAKES NOTHING AT ALL: it reads the configuration, the journal,
	// the guard and the records where they lie, judges the same request and
	// reports. The lock and the guard belong to a run that will write.
	if m.dryRun {
		// THE JOURNAL FIRST: a preview describes a REQUEST, and a host with a
		// transition under way may have no configuration left to read at all,
		// so judging the configuration before the journal answers about the
		// wrong thing.
		if r := requireNoJournalForRequest(); r != nil {
			return nil, r
		}

		obs, r := observeRetireConfig(m.configPath)
		if r != nil {
			return nil, r
		}

		if r := requirePreparedHost(obs); r != nil {
			return nil, r
		}

		return retireRequestReport(ctx, m, in, obs)
	}

	root, dir, shape, r := retireGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer root.release()
	defer func() { _ = dir.Close() }()

	// THE JOURNAL IS READ BEFORE THE CONFIGURATION, because past the archive
	// there may be no configuration to read and no identity at its configured
	// path: such a resume reads both from the journal's locator, which is the
	// tail's reader and not in this binary yet, and it is refused here rather
	// than met below as a missing file.
	j, journalFact, r := readRetireJournal()
	if r != nil {
		return nil, r
	}

	if r := refuseResumePastTheArchive(j, journalFact); r != nil {
		return nil, r
	}

	// EVERY REFUSAL FROM HERE TO THE TRANSITION SAYS WHAT THE HOST HOLDS WHEN
	// IT REFUSES. A retirement at `stopped` is not a host where nothing has
	// happened, whatever then refuses — an unreachable ledger, a configuration
	// that moved, an exclusion another writer holds — and `nothing` would send
	// an operator looking for a state the host is not in. IT IS READ AND NOT
	// REMEMBERED, because this run may have written the journal itself: a
	// request that records its intent and then fails to advance the row, to
	// publish the status or to release the exclusion has left `intent` behind,
	// and the fact this run started with says absent. A journal that cannot be
	// read then is could-not-tell, never `nothing`.
	at := func(r *retireRefusal) *retireRefusal {
		if r == nil {
			return nil
		}

		// THE READ'S OWN ERROR IS NOT THIS ANSWER'S: what refused is already in
		// the refusal, and all this adds is which phase the host stands at.
		now, presence, err := retirement.ReadJournal()
		if err != nil && presence == retirement.JournalPresent {
			presence = retirement.JournalUnreadable
		}

		switch presence {
		case retirement.JournalAbsent:
			return r
		case retirement.JournalPresent:
			return atRetirePhase(now, r)
		default:
			r.State = retireStateUnknown

			return r
		}
	}

	// THE CONFIGURATION IS OBSERVED UNDER THE LOCK, because everything below
	// rests on it: its digest is compared with the one the role read, its
	// backend and controllers decide eligibility, and its identity directory
	// is what the exclusion and the archive name.
	obs, r := observeRetireConfig(m.configPath)
	if r != nil {
		return nil, at(r)
	}

	if r := requirePreparedHost(obs); r != nil {
		return nil, at(r)
	}

	cfg := obs.cfg

	// THE EXCLUSION A RESUME TAKES IS THE TRANSITION'S, because from `stopped`
	// on the status this host published refuses every ordinary writer, this run
	// included. IT IS GRANTED TO THIS TRANSITION ALONE, and proved from what is
	// readable without taking anything: the journal names this host, and this
	// converge's guard carries the marker that names the journal's transition.
	// A journal describing another retirement, or one no marker on this guard
	// claims, takes the ordinary access and meets whatever the status says.
	access := withIdentityAccess
	if retireDrivesThisJournal(m, shape, j, journalFact) {
		access = withRetiringIdentityAccess
	}

	out, r := access(ctx, cfg.Server.IdentityDir, func() (any, *retireRefusal) {
		identity, r := retireIdentity(cfg.Server.IdentityDir)
		if r != nil {
			return nil, r
		}

		// THE JOURNAL'S ASSOCIATION FIRST, BEFORE ANY LEDGER IS OPENED: an open
		// migrates a schema, repairs ownership and hands artefacts back, and a
		// journal that does not describe this deployment's retirement is not a
		// reason to do any of that.
		if journalFact != retirement.JournalFactAbsent && j.Deployment != identity {
			return nil, retireUnknown(retireReasonJournal, fmt.Sprintf("the journal at %s names deployment %s and this "+
				"host's identity is %s", j.Phase, j.Deployment, identity), "the runbook in docs/operating/upgrades.md")
		}

		db, r := retireOpenLedgerFor(ctx, cfg, m.environmentFile, m.dryRun)
		if r != nil {
			return nil, r
		}

		defer func() { _ = db.Close() }()

		row, present, err := db.ReadRetirement(ctx, identity)
		if err != nil {
			return nil, retireUnknown(retireReasonLedger, err.Error(), "")
		}

		if r := requireMarkerAssociation(shape.Guard.Transition, row, present); r != nil {
			return nil, r
		}

		d, r := dispatchRetire(row, present, journalFact, m)
		if r != nil {
			return nil, r
		}

		if d != retirement.DispatchAdopt {
			return resumeRetirement(ctx, m, shape, db, d, j, identity, row)
		}

		plan, r := judgeRetireRequest(ctx, m, in, obs, identity, row, db)
		if r != nil {
			// A REFUSAL RELEASES ONLY A ROW THIS CONVERGE INSERTED, and only
			// when there is no marker beside it: the role says which through
			// --reservation-fresh, from its own --reserve answer; an adopted
			// row is kept for the next request, a DRY RUN releases nothing at
			// all, and a row beside a marker is the abandonment's to release,
			// since deleting it here would leave the marker naming no row.
			r.Reservation = "kept"

			switch {
			case m.dryRun, !m.reservationFresh:
			case shape.Guard.Transition != nil:
				r.Why += "; the reservation is kept because the guard carries this transition's marker, which " +
					"`billet server retire --abandon-reservation` releases"
			default:
				if err := db.ReleaseRetirement(ctx, identity, m.retiringHost, m.run); err != nil {
					r.Why += "; and releasing the reservation: " + err.Error()
				} else {
					r.Reservation = "released"
				}
			}

			return nil, r
		}

		return applyRetireIntent(ctx, m, root, dir, shape, db, plan)
	})
	if r != nil {
		return nil, at(r)
	}

	// THE TRANSITION RUNS OUTSIDE EVERY IDENTITY HOLD: it waits for a backup
	// that is itself taking the authority lock, and it takes that lock for the
	// archive alone. The transaction lock and the guard are still held.
	recorded, ok := out.(retirement.Journal)
	if !ok {
		return nil, retireUnknown(retireReasonJournal, "the intent answered no journal to drive", "")
	}

	return nil, runRetireTransition(ctx, m, obs, recorded)
}

// runRetireTransition drives the phases and answers what the host reached.
// Every outcome is a refusal today: the transition ends at `done`, and the
// tail that completes the ledger row is the next change.
func runRetireTransition(ctx context.Context, m retireMode, obs *installedConfigObservation, j retirement.Journal,
) *retireRefusal {
	j, steps, r := retireTransition(ctx, m, obs, j)
	if r != nil {
		// A REFUSAL THAT ALREADY SAID `unknown` KEEPS IT: the phase this run
		// last knew is not what the host holds, and naming it would send the
		// next converge to a state nothing is in.
		if r.State != retireStateUnknown {
			r.State = string(j.Phase)
		}

		return r
	}

	return &retireRefusal{Schema: retireSchema, Outcome: retireOutcomeUnknown, Reason: retireReasonPhase,
		Why: fmt.Sprintf("the transition is complete on this host (%s); the tail that completes the ledger row, "+
			"acknowledges it and clears the guard's marker is not in this binary yet", describeRetireSteps(steps)),
		State: string(j.Phase)}
}

// describeRetireSteps renders what this run did, for the answer.
func describeRetireSteps(steps []retireStep) string {
	if len(steps) == 0 {
		return "this run performed no step"
	}

	words := make([]string, 0, len(steps))
	for _, s := range steps {
		words = append(words, s.Action)
	}

	return "this run performed " + strings.Join(words, ", ")
}

// resumeRetirement takes a transition already under way: the journal is held
// to this retirement and to this guard, its ownership is rebound to the holder
// driving it now, and a row still at `reserved` beside an `intent` journal (the
// crash between the two intent writes) is advanced.
func resumeRetirement(ctx context.Context, m retireMode, shape claimShape, db *state.DB, d retirement.Dispatch,
	j retirement.Journal, identity string, row state.Retirement,
) (any, *retireRefusal) {
	if r := validateRetireJournal(&j, m, shape, identity, row); r != nil {
		return nil, r
	}

	if j.Ownership.Owner != m.run {
		j.Rebind(m.run)

		if err := j.Write(retireNow()); err != nil {
			return nil, retireUnknown(retireReasonJournal, "record this converge as the journal's owner: "+err.Error(), "")
		}
	}

	if d == retirement.DispatchAdvanceRow {
		if err := db.AdvanceRetirementToIntent(ctx, identity, m.retiringHost, j.Provenance.TransitionID, m.run,
			retireNow()); err != nil {
			return nil, retireUnknown(retireReasonLedger, "advance the row to intent: "+err.Error(), "")
		}
	}

	return j, nil
}

// validateRetireJournal holds an incomplete journal to the retirement the row
// describes, to this guard's holder or its chain, and to the marker the
// transition wrote at intent.
func validateRetireJournal(j *retirement.Journal, m retireMode, shape claimShape, identity string,
	row state.Retirement,
) *retireRefusal {
	facts := retirement.RowFacts{State: row.State, Retiring: row.Retiring, Survivor: row.Survivor,
		ReservedAt: row.ReservedAt, TransitionID: row.TransitionID}

	if err := j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Identity: identity, Holder: m.run,
		TakenOverFrom: shape.Guard.TakenOverFrom, Row: &facts}); err != nil {
		return retireUnknown(retireReasonJournal, err.Error(), "the runbook in docs/operating/upgrades.md")
	}

	// THE MARKER IS REQUIRED, not merely consistent: it is what keeps the
	// guard from being released under a transition that has stopped a server
	// and moved an identity, and a resume without one is a host whose guard
	// somebody could take away mid-transition.
	if shape.Guard.Transition == nil {
		return retireUnknown(retireReasonMarker, fmt.Sprintf("the retirement at %s carries no marker on this converge's "+
			"guard, so the guard could be released under it", j.Phase), "the runbook in docs/operating/upgrades.md")
	}

	if shape.Guard.Transition.ID != j.Provenance.TransitionID {
		return retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s and the journal "+
			"names %s; the marker is kept", shape.Guard.Transition.ID, j.Provenance.TransitionID), "")
	}

	return nil
}

// requirePreparedHost is the retirement's prerequisite: an installer has
// recorded the service account, so the global authority exclusion is in force
// and the status a transition publishes means something to every other
// writer. A legacy host is excluded by the identity directory's own lock,
// which the archive moves, so there is nothing to close there.
func requirePreparedHost(obs *installedConfigObservation) *retireRefusal {
	class, err := retirement.Classify(obs.cfg.Server.IdentityDir)

	switch {
	case err != nil:
		return retireUnknown(retireReasonIdentity, err.Error(), "")
	case class.Mode != retirement.ModePrepared:
		return retireRefuse(retireReasonIdentity, fmt.Sprintf("this host is %s: no service account is recorded at %s, so "+
			"no global authority exclusion is in force and a retirement has nothing to close", class.Mode,
			retirement.ServiceAccountPath()), "`billet local up` or the host role prepares the host")
	case !class.Directory:
		return retireRefuse(retireReasonIdentity, "the configured identity directory is absent, so there is nothing to "+
			"retire", "the runbook in docs/operating/upgrades.md")
	}

	return nil
}

// refuseResumePastTheArchive is this binary's boundary: a transition is driven
// to done by the run that recorded its intent, and a resume is admitted while
// the host still holds what the request read — the configured identity
// directory and an installed configuration with a server section. Past the
// archive both are gone by design, and the reader that works from the
// journal's locator alone is the tail's.
func refuseResumePastTheArchive(j retirement.Journal, fact retirement.JournalFact) *retireRefusal {
	if fact == retirement.JournalFactDone {
		return atRetirePhase(j, retireUnknown(retireReasonPhase, "this host's retirement is at done; the tail that "+
			"completes the ledger row, acknowledges it and clears the guard's marker is not in this binary yet", ""))
	}

	if fact != retirement.JournalFactIncomplete {
		return nil
	}

	if j.Phase != retirement.PhaseStopped {
		return atRetirePhase(j, retireUnknown(retireReasonPhase, fmt.Sprintf("this host's retirement is at %s, past the "+
			"archive: resuming it reads the identity and the ledger from the journal's locator (%s), which is not in "+
			"this binary yet", j.Phase, j.Locator.Archive), "the runbook in docs/operating/upgrades.md"))
	}

	// A JOURNAL AT `stopped` WHOSE MOVE ALREADY COMPLETED is the same host: the
	// phase says the identity is at its configured path and the filesystem says
	// it is at the archive, which is the remainder of an interruption between
	// the rename and the phase. The driver handles it; the command cannot reach
	// it yet, because the exclusion and the identity it would read are inside
	// the directory that has moved.
	moved, err := retireDirPresent(j.Archive)
	if err != nil {
		return atRetirePhase(j, retireUnknown(retireReasonIdentity, err.Error(), ""))
	}

	if !moved {
		return nil
	}

	return atRetirePhase(j, retireUnknown(retireReasonPhase, fmt.Sprintf("this host's retirement is at %s and its "+
		"identity is already at %s: the move completed before its phase could be written, and resuming from there reads "+
		"the identity and the ledger from the journal's locator, which is not in this binary yet", j.Phase, j.Archive),
		"the runbook in docs/operating/upgrades.md"))
}

// atRetirePhase says what the host holds on a refusal that read a journal: the
// phase it reached, never the `nothing` of a run that found none. A retry that
// answered `nothing` over a host at `done` would say no retirement had ever
// happened here.
func atRetirePhase(j retirement.Journal, r *retireRefusal) *retireRefusal {
	r.State = string(j.Phase)

	return r
}

// retireDrivesThisJournal says whether the journal on this host is the
// transition THIS converge is driving, from facts no lock is needed to read.
func retireDrivesThisJournal(m retireMode, shape claimShape, j retirement.Journal, fact retirement.JournalFact) bool {
	switch fact {
	case retirement.JournalFactIntent, retirement.JournalFactIncomplete, retirement.JournalFactDone:
	default:
		return false
	}

	if shape.Guard.Transition == nil || shape.Guard.Transition.ID != j.Provenance.TransitionID {
		return false
	}

	// THE JOURNAL'S OWN VALIDATION, minus what needs a ledger: this host, the
	// provenance self-consistent, and the journal OWNED by this holder or by
	// one its guard took over from. A marker matching a journal nobody in this
	// guard's chain owns is not this converge's transition, and the exception
	// is what lets a run past a closed authority.
	return j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Holder: m.run,
		TakenOverFrom: shape.Guard.TakenOverFrom}) == nil
}

// readRetireJournal reads the journal and classifies it for the dispatch. A
// journal that cannot be judged is could-not-tell and never absence.
func readRetireJournal() (retirement.Journal, retirement.JournalFact, *retireRefusal) {
	j, presence, err := retirement.ReadJournal()

	switch presence {
	case retirement.JournalAbsent:
		return j, retirement.JournalFactAbsent, nil
	case retirement.JournalPresent:
		return j, retirement.JournalFactOf(true, j.Phase), nil
	default:
		// THE PHASE CANNOT BE ESTABLISHED, which is not the same as a host
		// nothing has happened on: a retry over the same unreadable journal
		// must not answer `nothing` where the first run answered `unknown`.
		r := retireUnknown(retireReasonJournal, "the retirement journal could not be judged: "+errorText(err), "")
		r.State = retireStateUnknown

		return j, "", r
	}
}

// requireNoJournalForRequest is the dry run's rule: a preview describes a
// request, and a retirement already under way is the mutating run's to resume.
func requireNoJournalForRequest() *retireRefusal {
	j, fact, r := readRetireJournal()
	if r != nil {
		return r
	}

	if fact == retirement.JournalFactAbsent {
		return nil
	}

	return atRetirePhase(j, retireUnknown(retireReasonPhase, fmt.Sprintf("a retirement journal exists at %s (transition "+
		"%s); a dry run describes a request and does not judge a transition already under way", j.Phase,
		j.Provenance.TransitionID), ""))
}

// retireRequestReport is the dry run over a request: nothing is taken and
// nothing is written. The guard is classified where it lies, the identity is
// peeked, the ledger is opened read-only, and the same judgement runs.
func retireRequestReport(ctx context.Context, m retireMode, in *retireInput, obs *installedConfigObservation,
) (any, *retireRefusal) {
	cfg := obs.cfg

	shape, err := classifyClaim()
	if err != nil {
		return nil, retireUnknown(retireReasonGuard, err.Error(), "")
	}

	// THE SAME JUDGEMENT THE MUTATING RUN MAKES, over the shape read without
	// the lock: a preview that reported a request the guard would refuse
	// would be a lie the role acts on. The guard DIRECTORY is held to the
	// same trust too, which the mutating path gets from its own open.
	if r := judgeGuardShape(shape, m.run); r != nil {
		return nil, r
	}

	if r := judgeGuardDirTrust(); r != nil {
		return nil, r
	}

	identity, r := retireIdentity(cfg.Server.IdentityDir)
	if r != nil {
		return nil, r
	}

	db, r := retireOpenLedgerFor(ctx, cfg, m.environmentFile, true)
	if r != nil {
		return nil, r
	}

	defer func() { _ = db.Close() }()

	row, present, err := db.ReadRetirement(ctx, identity)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, err.Error(), "")
	}

	if r := requireMarkerAssociation(shape.Guard.Transition, row, present); r != nil {
		return nil, r
	}

	// THE PREVIEW DISPATCHES OVER AN ABSENT JOURNAL, which is what it already
	// required above: a transition under way is the mutating run's to resume.
	if _, r := dispatchRetire(row, present, retirement.JournalFactAbsent, m); r != nil {
		return nil, r
	}

	plan, r := judgeRetireRequest(ctx, m, in, obs, identity, row, db)
	if r != nil {
		r.Reservation = "kept"

		return nil, r
	}

	return &retireIntentReport{Schema: retireSchema, Outcome: retireOutcomeReported, Would: "request",
		Variant: plan.variant, Survivor: plan.survivor, Nodes: plan.nodes, FailoverVerified: plan.failover,
		TransitionID: row.TransitionID, State: stateNothingRetire, Unlocked: unlockedReport}, nil
}

// unlockedReport is what a preview cannot answer for, said in its own answer:
// it takes no transaction lock and no identity exclusion, so every record it
// read may move before a request takes them, and only the request's own
// answer says what happened.
const unlockedReport = "this report was made without the transaction lock and without the identity exclusion, so the guard, " +
	"the configuration, the journal and the row it read may move before a request takes them"

// judgeGuardDirTrust holds the guard directory to the ownership and mode the
// mutating path requires of it, read-only: a directory anyone else may write
// is one whose record anyone else may replace.
func judgeGuardDirTrust() *retireRefusal {
	info, err := os.Lstat(activePath())
	if err != nil {
		return retireUnknown(retireReasonGuard, fmt.Sprintf("examine %s: %v", activePath(), err), "")
	}

	if err := requireTrustedDir(activePath(), info, 0o700); err != nil {
		return retireUnknown(retireReasonGuard, err.Error(), "")
	}

	return nil
}

// dispatchRequest holds the row to the one cell a request proceeds from: this
// host's own reservation, under this run, beside no journal.
func dispatchRetire(row state.Retirement, present bool, journal retirement.JournalFact, m retireMode,
) (retirement.Dispatch, *retireRefusal) {
	fact := retirement.RowAbsent
	if present {
		fact = rowFactOf(row, m.retiringHost)
	}

	d := retirement.DispatchFor(fact, journal)

	switch d {
	case retirement.DispatchAdopt:
	case retirement.DispatchResume, retirement.DispatchAdvanceRow:
		// A TRANSITION ALREADY UNDER WAY is resumed, and the request document
		// this run carries is not judged again: what it would judge, the
		// journal decided before anything stopped.
		return d, nil
	case retirement.DispatchRequest:
		return d, retireRefuse(retireReasonReservation, "this host holds no reservation; `billet server retire --reserve` writes "+
			"one before any report is collected", "")
	case retirement.DispatchCompleteRow, retirement.DispatchDone:
		// The journal's own boundary refuses a `done` journal before any
		// configuration is read; this keeps the dispatch total.
		return d, retireUnknown(retireReasonPhase, "this host's retirement is at done; the tail that completes the ledger "+
			"row, acknowledges it and clears the guard's marker is not in this binary yet", "")
	case retirement.DispatchRefusedReserved:
		return d, retireRefuse(retireReasonReserved, fmt.Sprintf("this deployment's retirement is reserved by %s (run %s, state %s, "+
			"since %s); one controller retires at a time", row.Retiring, row.Run, row.State, row.ReservedAt), "")
	case retirement.DispatchRefusedRetired:
		return d, retireRefuse(retireReasonRetired, retiredSentence(row), "")
	default:
		return d, retireUnknown(retireReasonReservation, fmt.Sprintf("the ledger's row is at %s for this host beside a journal "+
			"that is %s, which no step of the protocol produces (%s)", row.State, journal, d),
			"the runbook in docs/operating/upgrades.md")
	}

	if row.Run != m.run {
		return d, retireRefuse(retireReasonReserved, fmt.Sprintf("the reservation is run %s's, and this converge is %s; the same "+
			"host adopts it by reserving again", row.Run, m.run), "")
	}

	// THE SURVIVOR IS THE RESERVATION'S: the row is what the two controllers
	// contended for, and a request naming another survivor would record a
	// journal the row does not describe.
	if row.Survivor != m.survivorHost {
		return d, retireRefuse(retireReasonReserved, fmt.Sprintf("the reservation names %s as the survivor and this request names "+
			"%s; reserve again to name another", row.Survivor, m.survivorHost), "")
	}

	return d, nil
}

// judgeRetireRequest decides eligibility in the specified order, so the
// fixture for each refusal is reached: the round and the reports bound and
// fresh; the local eligibility; the variant and the rendering; the survivor;
// this host's ledger; the nodes and the equality chain; the entry predicates;
// the host preconditions.
func judgeRetireRequest(ctx context.Context, m retireMode, in *retireInput, obs *installedConfigObservation, identity string,
	row state.Retirement, db *state.DB,
) (*retirePlan, *retireRefusal) {
	cfg := obs.cfg
	now := retireNow()

	if r := bindRetireInput(in, m, row.ReservedAt, now, m.reportMaxAge); r != nil {
		return nil, r
	}

	if m.installedSHA == "" {
		return nil, retireRefuse(retireReasonCombination, "a request needs --installed-sha256, the digest the role read before it", "")
	}

	if obs.sha256 != m.installedSHA {
		return nil, retireRefuse(retireReasonConfig, "the installed configuration moved since the role read it (its digest is "+
			"not --installed-sha256)", "converge again")
	}

	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return nil, retireRefuse(retireReasonBackend, "a retirement is defined for a PostgreSQL active-passive pair, and this "+
			"deployment's ledger is "+string(cfg.Server.LedgerBackend()), "")
	}

	if cfg.Server.Controllers != config.ControllersActivePassive {
		return nil, retireRefuse(retireReasonControllers, "a retirement is defined for an active-passive pair, and this "+
			"deployment's controllers are "+string(cfg.Server.Controllers), "")
	}

	plan := &retirePlan{identity: identity, row: row, cfg: cfg, installed: obs.sha256, failover: m.failoverVerified}

	if r := judgeVariant(m, in, obs, plan); r != nil {
		return nil, r
	}

	if r := judgeSurvivor(m, in, cfg, identity, plan); r != nil {
		return nil, r
	}

	registrations, r := judgeOwnLedger(ctx, db, identity)
	if r != nil {
		return nil, r
	}

	if r := judgeNodes(m, in, identity, registrations, plan); r != nil {
		return nil, r
	}

	if r := judgeEntryPredicates(ctx, in, cfg); r != nil {
		return nil, r
	}

	if r := judgeHostPreconditions(ctx, m, cfg, plan, now); r != nil {
		return nil, r
	}

	return plan, nil
}

// retireOpenLedgerFor opens the ledger for a request: read-only for a dry run,
// which writes nothing anywhere, and the operator's open otherwise.
func retireOpenLedgerFor(ctx context.Context, cfg *config.Config, environmentFile string, dryRun bool) (*state.DB, *retireRefusal) {
	if !dryRun {
		return retireOpenLedger(ctx, cfg, environmentFile)
	}

	dsn, err := ledgerDSNFrom(cfg, environmentFile)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, err.Error(), "")
	}

	db, err := openStateInspect(ctx, cfg, dsn)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, "open the ledger for the report: "+err.Error(), "")
	}

	return db, nil
}

// judgeVariant decides server-only or retained-node and holds the rendering
// to the installed configuration: the same mapping minus the four keys, a
// document the loader admits, the same node state directory, the same node
// identity.
func judgeVariant(m retireMode, in *retireInput, obs *installedConfigObservation, plan *retirePlan) *retireRefusal {
	cfg := obs.cfg

	switch {
	case m.serverOnly && in.Desired != nil:
		return retireRefuse(retireReasonCombination, "--server-only and a rendering in the input are two variants; a request "+
			"has one", "")
	case m.serverOnly && cfg.Node != nil:
		return retireRefuse(retireReasonVariant, "--server-only, and the installed configuration has a node section; a "+
			"retained-node installation is never turned server-only by an inventory edit", "")
	case m.serverOnly:
		plan.variant = retirement.VariantServerOnly

		return nil
	case in.Desired == nil:
		return retireRefuse(retireReasonVariant, "the input carries no rendering and the request is not --server-only", "")
	case cfg.Node == nil:
		return retireRefuse(retireReasonVariant, "the input carries a rendering, and the installed configuration has no node "+
			"section to keep; a server-only host is retired with --server-only", "")
	}

	plan.variant = retirement.VariantRetainedNode
	plan.rendering = []byte(*in.Desired)

	if len(plan.rendering) > retirement.MaxStageBytes {
		return retireRefuse(retireReasonDesired, fmt.Sprintf("the rendering is longer than %d bytes", retirement.MaxStageBytes), "")
	}

	if path, err := serverlessMappingDiff(obs.body, plan.rendering); err != nil {
		return retireRefuse(retireReasonDesired, err.Error(), "")
	} else if path != "" {
		return retireRefuse(retireReasonDesired, fmt.Sprintf("the rendering is not the installed configuration minus %s: the "+
			"first difference is at %s", strings.Join(serverlessDroppedKeys, ", "), path), "")
	}

	rendered, err := config.Parse("the serverless rendering", plan.rendering)
	if err != nil {
		return retireRefuse(retireReasonConfig, "the serverless rendering is not a configuration the loader admits: "+
			err.Error(), "")
	}

	if rendered.Node == nil {
		return retireRefuse(retireReasonVariant, "the serverless rendering has no node section", "")
	}

	if rendered.Node.StateDir != cfg.Node.StateDir {
		return retireRefuse(retireReasonStateDir, fmt.Sprintf("the rendering's node.state_dir is %s and the installed %s; a "+
			"retirement is not an implicit state migration", rendered.Node.StateDir, cfg.Node.StateDir), "")
	}

	problem, unknown := sameNodeIdentity(obs, &configObservationLite{body: plan.rendering, cfg: rendered})

	switch {
	case unknown != "":
		return retireUnknown(retireReasonConfig, unknown, "")
	case problem != "":
		return retireRefuse(retireReasonDesired, problem, "")
	}

	return checkRenderingCredentials(rendered)
}

// checkRenderingCredentials loads the node credentials the rendering names
// THROUGH THE NODE'S OWN LOADER and builds the client TLS configuration the
// node builds, so what is admitted here is what a start admits: the loader's
// own rules about a key reached through a symlink or readable by the group,
// and the construction's own verification of the leaf against the trust store
// (its chain, its validity and its client-auth usage). A narrower pair of
// checks admitted a certificate and key that matched beside an authority that
// could not verify them, which is a start that fails after the archive.
//
// A FAILED READ IS COULD-NOT-TELL and a bundle that does not hold is a
// refusal, because the first says nothing about the credentials and the
// second says they are not ones a node starts with.
//
// WHAT THIS IS NOT: the provider and storage checks `billet check` performs
// (a reachable Docker daemon, a Ceph cluster) are not re-run here; they touch
// the world and a request that refused on a transient one would be worse than
// one that did not ask.
func checkRenderingCredentials(rendered *config.Config) *retireRefusal {
	if rendered.Node == nil || rendered.Node.TLS == nil {
		return nil
	}

	tlsCfg := rendered.Node.TLS

	bundle, err := wirecert.LoadBundle(tlsCfg.CertPath, tlsCfg.KeyPath, tlsCfg.CAPath)

	switch {
	case errors.Is(err, wirecert.ErrCredentialPolicy):
		// A RULE REFUSED, not a read that failed: the node would refuse these
		// credentials at its next start, and only an operator changes that.
		return retireRefuse(retireReasonConfig, "the node's credentials are not ones it reads: "+err.Error(), "")
	case err != nil:
		return retireUnknown(retireReasonConfig, "read the node's credentials as the node reads them: "+err.Error(), "")
	}

	if _, err := wirecert.ClientTLS(bundle); err != nil {
		return retireRefuse(retireReasonConfig, fmt.Sprintf("the node's credentials at %s, %s and %s are not ones it can start "+
			"with: %v", tlsCfg.CertPath, tlsCfg.KeyPath, tlsCfg.CAPath, err), "")
	}

	return nil
}

// serverlessMappingDiff decodes both documents as YAML mappings, drops the
// four server keys from the installed one and answers the first path at
// which the two differ, or nothing.
func serverlessMappingDiff(installed, rendering []byte) (string, error) {
	var a, b map[string]any

	if err := yaml.Unmarshal(installed, &a); err != nil {
		return "", fmt.Errorf("the installed configuration is not a YAML mapping: %w", err)
	}

	if err := yaml.Unmarshal(rendering, &b); err != nil {
		return "", fmt.Errorf("the rendering is not a YAML mapping: %w", err)
	}

	if a == nil || b == nil {
		return "", errors.New("a document is empty")
	}

	for _, key := range serverlessDroppedKeys {
		delete(a, key)
	}

	return diffPath(a, b, ""), nil
}

// diffPath is the first path at which two decoded documents differ, in key
// order, or nothing.
func diffPath(a, b any, path string) string {
	ma, aok := a.(map[string]any)
	mb, bok := b.(map[string]any)

	if aok && bok {
		keys := map[string]struct{}{}
		for k := range ma {
			keys[k] = struct{}{}
		}

		for k := range mb {
			keys[k] = struct{}{}
		}

		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}

		sort.Strings(sorted)

		for _, k := range sorted {
			va, inA := ma[k]
			vb, inB := mb[k]

			switch {
			case !inA:
				return path + "." + k + " (only in the rendering)"
			case !inB:
				return path + "." + k + " (only in the installed configuration)"
			}

			if p := diffPath(va, vb, path+"."+k); p != "" {
				return p
			}
		}

		return ""
	}

	la, aok := a.([]any)
	lb, bok := b.([]any)

	if aok && bok {
		if len(la) != len(lb) {
			return fmt.Sprintf("%s (%d items against %d)", path, len(la), len(lb))
		}

		for i := range la {
			if p := diffPath(la[i], lb[i], fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}

		return ""
	}

	if !reflect.DeepEqual(a, b) {
		if path == "" {
			return "the document"
		}

		return path
	}

	return ""
}

// judgeSurvivor holds the survivor's report to what a running survivor is:
// bound to this configuration, a PostgreSQL active-passive controller of this
// deployment, its server active and running its executable, its authority the
// one this host holds with no rotation in progress, no retirement of its own;
// and its status report bound to this deployment.
func judgeSurvivor(m retireMode, in *retireInput, cfg *config.Config, identity string, plan *retirePlan) *retireRefusal {
	if m.survivorFlagged {
		return retireRefuse(retireReasonSurvivorFlagged, fmt.Sprintf("%s is flagged for retirement itself; a survivor may not "+
			"be", m.survivorHost), "")
	}

	env := in.Survivor
	who := env.Host
	doc := env.inspect

	checks := []*retireRefusal{
		doc.requireBool(retireReasonSurvivor, who, true, "config_binding"),
		doc.requireStr(retireReasonSurvivor, who, string(config.StatePostgres), "installed_config", "ledger_backend"),
		doc.requireStr(retireReasonSurvivor, who, string(config.ControllersActivePassive), "installed_config", "controllers"),
		doc.requireStr(retireReasonSurvivorBinding, who, identity, "host", "deployment_id"),
		doc.requireStr(retireReasonSurvivor, who, "active", "services", "server", "active_state"),
		doc.requireBool(retireReasonSurvivor, who, true, "services", "server", "same_as_executable"),
		doc.requireBool(retireReasonAuthority, who, false, "host", "authority", "rotation_in_progress"),
		doc.requireNull(retireReasonSurvivorRetiring, who, "host", "retirement"),
	}

	for _, r := range checks {
		if r != nil {
			return r
		}
	}

	if env.status.m == nil {
		return retireRefuse(retireReasonSurvivorBinding, "the survivor's report carries no status document", "")
	}

	if r := env.status.requireBool(retireReasonSurvivorBinding, who, true, "deployment", "bound"); r != nil {
		return r
	}

	if r := env.status.requireStr(retireReasonSurvivorBinding, who, identity, "deployment", "id"); r != nil {
		return r
	}

	// THE AUTHORITY: the survivor's current certificate's DER equals this
	// host's, parsed from the PEM the report carries and never from its
	// reported fingerprint.
	survivorPEM, st, why := doc.str("host", "authority", "current", "pem")

	switch st {
	case fieldUnknown:
		return retireUnknown(retireReasonAuthority, fmt.Sprintf("the report of %s could not observe its authority: %s", who, why), "")
	case fieldPresent:
	default:
		return retireRefuse(retireReasonAuthority, "the report of "+who+" carries no current authority certificate", "")
	}

	survivorDER, err := derSHA256OfPEM(survivorPEM)
	if err != nil {
		return retireRefuse(retireReasonAuthority, fmt.Sprintf("the report of %s carries an authority certificate that does not "+
			"parse: %v", who, err), "")
	}

	snap, err := wirecert.SnapshotAuthority(cfg.Server.IdentityDir)

	switch {
	case err != nil:
		return retireUnknown(retireReasonAuthority, "this host's authority: "+err.Error(), "")
	case snap.Current == nil:
		return retireRefuse(retireReasonAuthority, "this host holds no node-wire authority", "")
	case snap.Rotating():
		return retireRefuse(retireReasonAuthority, "a rotation is in progress on this host; a retirement waits for `billet ca retire`", "")
	}

	own := sha256.Sum256(snap.Current.Raw)
	ownHex := hex.EncodeToString(own[:])

	if ownHex != survivorDER {
		return retireRefuse(retireReasonAuthority, fmt.Sprintf("the survivor %s serves the authority %s and this host %s; the "+
			"pair does not share one authority", who, survivorDER, ownHex), "")
	}

	plan.survivor = retirement.JournalSurvivor{Host: who, Deployment: identity, CASHA256: ownHex}

	return nil
}

// derSHA256OfPEM parses one PEM certificate and answers its DER digest.
func derSHA256OfPEM(text string) (string, error) {
	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not one PEM certificate")
	}

	if strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("more than one PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(cert.Raw)

	return hex.EncodeToString(sum[:]), nil
}

// judgeOwnLedger reads this host's ledger once: bound to this identity, and
// its registrations are the node set.
func judgeOwnLedger(ctx context.Context, db *state.DB, identity string) ([]rollout.Registration, *retireRefusal) {
	snapshot, err := rollout.New(db).StatusSnapshot(ctx)
	if err != nil {
		return nil, retireUnknown(retireReasonLedger, "read this host's ledger: "+err.Error(), "")
	}

	switch {
	case snapshot.Binding == "":
		return nil, retireRefuse(retireReasonUnbound, "this host's ledger is bound to no deployment", "")
	case snapshot.Binding != identity:
		return nil, retireRefuse(retireReasonTrust, fmt.Sprintf("this host's ledger is bound to deployment %s and its identity "+
			"is %s", snapshot.Binding, identity), "")
	}

	return snapshot.Registrations, nil
}

// judgeNodes reconciles the ledger's registrations with the node reports
// (every registration exactly one report, every report a registration), and
// holds each node to the equality chain and its endpoint to the address rule.
func judgeNodes(m retireMode, in *retireInput, identity string, registrations []rollout.Registration, plan *retirePlan) *retireRefusal {
	byName := map[string][]*retireEnvelope{}

	for _, env := range in.envelopes()[1:] {
		if env == in.Survivor {
			continue
		}

		name, st, why := env.inspect.str("host", "node_effective_name")

		switch st {
		case fieldUnknown:
			return retireUnknown(retireReasonNode, fmt.Sprintf("the report of %s could not resolve its node's name: %s", env.Host, why), "")
		case fieldPresent:
		default:
			return retireRefuse(retireReasonNodeUnknown, fmt.Sprintf("the report of %s names no node (host.node_effective_name), "+
				"and every node report is a node-bearing host's", env.Host), "")
		}

		byName[name] = append(byName[name], env)
	}

	known := map[string]rollout.Registration{}
	for _, reg := range registrations {
		known[reg.Name] = reg
	}

	for name := range byName {
		if _, ok := known[name]; !ok {
			return retireRefuse(retireReasonNodeUnknown, fmt.Sprintf("the report of %s names the node %q, which this ledger "+
				"holds no registration for", byName[name][0].Host, name), "")
		}
	}

	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}

	sort.Strings(names)

	own, err := hostInterfaceAddresses()
	if err != nil {
		return retireUnknown(retireReasonEndpoint, "this host's addresses: "+err.Error(), "")
	}

	survivorAddrs, r := addressesOf(in.Survivor)
	if r != nil {
		return r
	}

	shared, err := parseSharedAddresses(m.sharedAddresses)
	if err != nil {
		return retireRefuse(retireReasonCombination, err.Error(), "")
	}

	for _, name := range names {
		envs := byName[name]

		switch len(envs) {
		case 0:
			return retireRefuse(retireReasonNodeMissing, fmt.Sprintf("the ledger registers the node %q and the input carries no "+
				"report of it; an unreachable node blocks the request", name), "")
		case 1:
		default:
			return retireRefuse(retireReasonNodeDuplicate, fmt.Sprintf("the node %q is reported by %s and %s", name, envs[0].Host,
				envs[1].Host), "")
		}

		node, r := judgeNodeChain(m, in, envs[0], name, identity, known[name], survivorAddrs, own, shared)
		if r != nil {
			return r
		}

		plan.nodes = append(plan.nodes, node)
	}

	return nil
}

// addressesOf reads a report's host.addresses.
func addressesOf(env *retireEnvelope) ([]hostAddress, *retireRefusal) {
	v, st, why := env.inspect.get("host", "addresses")

	switch st {
	case fieldUnknown:
		return nil, retireUnknown(retireReasonEndpoint, fmt.Sprintf("the report of %s could not observe its addresses: %s", env.Host, why), "")
	case fieldPresent:
	default:
		return nil, retireRefuse(retireReasonEndpoint, "the report of "+env.Host+" carries no host.addresses; its billet "+
			"predates the retirement's inspector", "")
	}

	items, ok := v.([]any)
	if !ok {
		return nil, retireRefuse(retireReasonEndpoint, "the report of "+env.Host+" carries host.addresses that is not a list", "")
	}

	var out []hostAddress

	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, retireRefuse(retireReasonEndpoint, "the report of "+env.Host+" carries an address that is not an object", "")
		}

		addr, ok := obj["address"].(string)
		if !ok {
			return nil, retireRefuse(retireReasonEndpoint, "the report of "+env.Host+" carries an address without its spelling", "")
		}

		out = append(out, hostAddress{Address: addr})
	}

	return out, nil
}

// parseSharedAddresses parses --shared-address operands.
func parseSharedAddresses(spellings []string) ([]netip.Addr, error) {
	var out []netip.Addr

	for _, s := range spellings {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("--shared-address %q is not an address", s)
		}

		if a.Zone() != "" {
			return nil, fmt.Errorf("--shared-address %q carries a zone", s)
		}

		out = append(out, a.Unmap())
	}

	return out, nil
}

// judgeNodeChain is the equality chain for one node, through the one endpoint
// representation, then the address rule on its one endpoint.
func judgeNodeChain(m retireMode, in *retireInput, env *retireEnvelope, name, identity string, reg rollout.Registration,
	survivorAddrs, own []hostAddress, shared []netip.Addr,
) (retirement.JournalNode, *retireRefusal) {
	who := env.Host
	doc := env.inspect

	var node retirement.JournalNode

	if r := doc.requireBool(retireReasonNode, who, true, "config_binding"); r != nil {
		return node, r
	}

	desired, ok := in.DesiredNodes[who]
	if !ok {
		return node, retireRefuse(retireReasonDesired, "the input carries no desired configuration for the node host "+who, "")
	}

	if r := doc.requireStr(retireReasonReceipt, who, "present", "host", "endpoint_receipt", "presence"); r != nil {
		return node, r
	}

	receipt := func(member string) (string, *retireRefusal) {
		s, st, why := doc.str("host", "endpoint_receipt", "receipt", member)

		switch st {
		case fieldPresent:
			return s, nil
		case fieldUnknown:
			return "", retireUnknown(retireReasonReceipt, fmt.Sprintf("the report of %s could not observe its receipt's %s: %s", who, member, why), "")
		default:
			return "", retireRefuse(retireReasonReceipt, fmt.Sprintf("the report of %s carries a receipt without %s", who, member), "")
		}
	}

	equal := func(member, have, want, against string) *retireRefusal {
		if have == want {
			return nil
		}

		return retireRefuse(retireReasonReceipt, fmt.Sprintf("the node %s's %s is %q and its %s %q; the chain does not hold", who,
			member, have, against, want), "")
	}

	for _, c := range []struct {
		member, against string
		path            []string
	}{
		{"deployment", "deployment identity", nil},
		{"node", "effective node name", nil},
	} {
		got, r := receipt(c.member)
		if r != nil {
			return node, r
		}

		want := identity
		if c.member == "node" {
			want = name
		}

		if r := equal("receipt."+c.member, got, want, c.against); r != nil {
			return node, r
		}
	}

	if r := doc.requireStr(retireReasonReceipt, who, identity, "host", "registration", "deployment"); r != nil {
		return node, r
	}

	if r := doc.requireStr(retireReasonReceipt, who, name, "host", "registration", "node"); r != nil {
		return node, r
	}

	installedSHA, r := receipt("installed_sha256")
	if r != nil {
		return node, r
	}

	if r := doc.requireStr(retireReasonReceipt, who, installedSHA, "installed_config", "sha256"); r != nil {
		return node, r
	}

	if r := equal("receipt.installed_sha256", installedSHA, desired.SHA256, "desired configuration's digest"); r != nil {
		return node, r
	}

	invocation, r := receipt("invocation_id")
	if r != nil {
		return node, r
	}

	if r := doc.requireStr(retireReasonReceipt, who, invocation, "host", "registration", "invocation_id"); r != nil {
		return node, r
	}

	if r := doc.requireStr(retireReasonReceipt, who, invocation, "services", "node", "invocation_id"); r != nil {
		return node, r
	}

	incarnation, r := receipt("incarnation")
	if r != nil {
		return node, r
	}

	if r := doc.requireStr(retireReasonReceipt, who, incarnation, "host", "registration", "incarnation"); r != nil {
		return node, r
	}

	if r := equal("receipt.incarnation", incarnation, reg.Incarnation, "ledger registration's incarnation"); r != nil {
		return node, r
	}

	// THE ONE ENDPOINT, through the one representation: the receipt's two,
	// the registration's, the installed configuration's and the desired one.
	installedEP, r := receipt("installed_endpoint")
	if r != nil {
		return node, r
	}

	ep, err := endpoint.ParseCanonical(installedEP)
	if err != nil {
		return node, retireRefuse(retireReasonReceipt, fmt.Sprintf("the node %s's receipt endpoint %q: %v", who, installedEP, err), "")
	}

	effectiveEP, r := receipt("effective_endpoint")
	if r != nil {
		return node, r
	}

	registrationEP, st, why := doc.str("host", "registration", "endpoint")
	if st != fieldPresent {
		if st == fieldUnknown {
			return node, retireUnknown(retireReasonReceipt, fmt.Sprintf("the report of %s could not observe its registration's "+
				"endpoint: %s", who, why), "")
		}

		return node, retireRefuse(retireReasonReceipt, "the report of "+who+" carries no registration endpoint", "")
	}

	configuredEP, st, why := doc.str("host", "installed_endpoint")
	if st != fieldPresent {
		if st == fieldUnknown {
			return node, retireUnknown(retireReasonReceipt, fmt.Sprintf("the report of %s could not observe its installed "+
				"endpoint: %s", who, why), "")
		}

		return node, retireRefuse(retireReasonReceipt, "the report of "+who+" carries no installed endpoint", "")
	}

	for _, other := range []struct{ member, text string }{
		{"receipt.effective_endpoint", effectiveEP}, {"host.registration.endpoint", registrationEP},
		{"host.installed_endpoint", configuredEP}, {"the desired endpoint", desired.Endpoint},
	} {
		o, err := endpoint.ParseCanonical(other.text)
		if err != nil || !o.Equal(ep) {
			return node, retireRefuse(retireReasonReceipt, fmt.Sprintf("the node %s dials %s and its %s is %q; the chain does not hold",
				who, ep.String(), other.member, other.text), "")
		}
	}

	if r := judgeNodeEndpoint(ep, who, m.survivorHost, survivorAddrs, own, shared, m.failoverVerified); r != nil {
		return node, r
	}

	return retirement.JournalNode{Name: name, Incarnation: incarnation, Endpoint: ep.String()}, nil
}

// judgeNodeEndpoint is the address rule, dispatched in order: a zone or an
// unspecified address refuses always; loopback is admitted exactly on the
// survivor's own node; a non-loopback address among this host's own refuses;
// one among the survivor's and not shared is admitted; a shared address, one
// held by neither controller, or a DNS spelling is admitted only under the
// operator's failover assertion.
func judgeNodeEndpoint(ep endpoint.Endpoint, nodeHost, survivorHost string, survivorAddrs, own []hostAddress,
	shared []netip.Addr, failoverVerified bool,
) *retireRefusal {
	asserted := func(what string) *retireRefusal {
		if failoverVerified {
			return nil
		}

		return retireRefuse(retireReasonEndpoint, fmt.Sprintf("the node %s dials %s, %s, whose survival of this controller's "+
			"removal billet cannot establish; --endpoint-failover-verified records the operator's assertion that it does",
			nodeHost, ep.String(), what), "")
	}

	if ep.Host.Kind == endpoint.HostDNS {
		return asserted("a DNS name")
	}

	addr := ep.Host.Addr.Unmap()

	switch {
	case addr.Zone() != "":
		return retireRefuse(retireReasonEndpoint, fmt.Sprintf("the node %s dials %s, a zone-scoped address", nodeHost, ep.String()), "")
	case addr.IsUnspecified():
		return retireRefuse(retireReasonEndpoint, fmt.Sprintf("the node %s dials %s, the unspecified address, which the kernel routes "+
			"locally", nodeHost, ep.String()), "")
	case addr.IsLoopback():
		if nodeHost == survivorHost {
			return nil
		}

		return retireRefuse(retireReasonEndpoint, fmt.Sprintf("the node %s dials loopback (%s), which reaches the survivor only on the "+
			"survivor's own host", nodeHost, ep.String()), "")
	}

	holds := func(addrs []hostAddress) bool {
		for _, a := range addrs {
			if p, err := netip.ParseAddr(a.Address); err == nil && p.Unmap() == addr {
				return true
			}
		}

		return false
	}

	if holds(own) {
		return retireRefuse(retireReasonEndpoint, fmt.Sprintf("the node %s dials %s, an address of the retiring controller", nodeHost,
			ep.String()), "")
	}

	for _, s := range shared {
		if s == addr {
			return asserted("a shared address")
		}
	}

	if holds(survivorAddrs) {
		return nil
	}

	return asserted("an address held by neither controller")
}

// judgeEntryPredicates holds this host's own node, when it has one, to what
// the later phases will need: its file unchanged since its start, and a
// KillMode under which a stop proves something.
func judgeEntryPredicates(ctx context.Context, in *retireInput, cfg *config.Config) *retireRefusal {
	if cfg.Node == nil {
		return nil
	}

	if r := in.Self.inspect.requireBool(retireReasonNodeChanged, in.Self.Host, false, "services", "node",
		"config_changed_since_start"); r != nil {
		r.Next = "converge that change first"

		return r
	}

	obs, problem := observeUnit(ctx, endpointInspector(), nodeUnit)
	if problem != "" {
		return retireUnknown(retireReasonUnit, "this host's node unit: "+problem, "")
	}

	switch obs.KillMode {
	case "":
		return retireUnknown(retireReasonUnit, "systemd answered no KillMode for "+nodeUnit+", so the later stop's premise "+
			"cannot be judged", "")
	case "mixed", "control-group":
		return nil
	default:
		return retireRefuse(retireReasonPolicy, fmt.Sprintf("%s has KillMode=%s, under which a stop proves nothing about the "+
			"processes that remain; only mixed or control-group is retired", nodeUnit, obs.KillMode), "")
	}
}

// judgeHostPreconditions is what the rename and the rewrite will need: one
// rename on one mount, the retirement directory not inside the identity, no
// stale stage, no node path through the identity directory, the backup
// service quiescent.
func judgeHostPreconditions(ctx context.Context, m retireMode, cfg *config.Config, plan *retirePlan, now time.Time) *retireRefusal {
	identityDir, err := filepath.EvalSymlinks(cfg.Server.IdentityDir)
	if err != nil {
		return retireUnknown(retireReasonIdentity, "resolve the identity directory: "+err.Error(), "")
	}

	// A DRY RUN CREATES NOTHING: the retirement directory is created by the
	// request that will write into it, and a dry run over a host that has
	// none compares the mount of the parent the directory would be made in.
	destination := retirement.RetiredDir()

	if !m.dryRun {
		if err := retirement.EnsureRetiredDir(); err != nil {
			return retireUnknown(retireReasonStage, err.Error(), "")
		}
	} else if _, err := os.Lstat(destination); errors.Is(err, fs.ErrNotExist) {
		destination = retirement.Root
	} else if err != nil {
		return retireUnknown(retireReasonStage, "examine "+destination+": "+err.Error(), "")
	}

	retired, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return retireUnknown(retireReasonStage, "resolve the retirement directory: "+err.Error(), "")
	}

	if underOrEqual(identityDir, retired) {
		return retireRefuse(retireReasonNested, fmt.Sprintf("the retirement directory %s lies under the identity directory %s, "+
			"which cannot be renamed into its own descendant", retired, identityDir), "")
	}

	entries, err := readMountinfo()
	if err != nil {
		return retireUnknown(retireReasonMount, err.Error(), "")
	}

	_, identityIsMount, err := mountOf(entries, identityDir)
	if err != nil {
		return retireUnknown(retireReasonMount, err.Error(), "")
	}

	if identityIsMount {
		return retireRefuse(retireReasonMount, fmt.Sprintf("the identity directory %s is a mount point, and a mount point is not "+
			"renamed", identityDir), "")
	}

	parentMount, _, err := mountOf(entries, filepath.Dir(identityDir))
	if err != nil {
		return retireUnknown(retireReasonMount, err.Error(), "")
	}

	retiredMount, _, err := mountOf(entries, retired)
	if err != nil {
		return retireUnknown(retireReasonMount, err.Error(), "")
	}

	if parentMount != retiredMount {
		return retireRefuse(retireReasonMount, fmt.Sprintf("the identity directory's parent is on mount %d and the retirement "+
			"directory on mount %d; the archive is one rename on one mount", parentMount, retiredMount), "")
	}

	if _, presence, err := retirement.ReadStage(); presence != retirement.StageFileAbsent {
		if presence == retirement.StageFilePresent {
			return retireRefuse(retireReasonStage, fmt.Sprintf("a staged configuration exists at %s beside no journal; move it "+
				"to an audit location by hand", retirement.StagePath()), "the audit-move runbook in docs/operating/upgrades.md")
		}

		return retireUnknown(retireReasonStage, err.Error(), "")
	}

	for _, p := range nodePathsOf(cfg) {
		traverses, unknown, err := walkTraverses(p.path, identityDir)

		switch {
		case err != nil:
			return retireRefuse(retireReasonNodePath, fmt.Sprintf("%s (%s): %v", p.name, p.path, err), "")
		case unknown != "":
			return retireUnknown(retireReasonNodePath, fmt.Sprintf("%s (%s): %s", p.name, p.path, unknown), "")
		case traverses:
			return retireRefuse(retireReasonNodePath, fmt.Sprintf("%s (%s) resolves through the identity directory %s, which the "+
				"archive moves; the node would lose it at the rename", p.name, p.path, identityDir), "")
		}
	}

	backup, problem := observeUnit(ctx, endpointInspector(), backupServiceUnit)
	if problem != "" {
		return retireUnknown(retireReasonBackup, backupServiceUnit+": "+problem, "")
	}

	if backup.LoadState != "not-found" && (backup.ActiveState != "inactive" || (backup.MainPID != "" && backup.MainPID != "0")) {
		return retireRefuse(retireReasonBackup, fmt.Sprintf("%s is %s (pid %s); a retirement waits for the backup to finish",
			backupServiceUnit, orUnknownWord(backup.ActiveState), orUnknownWord(backup.MainPID)), "")
	}

	plan.archive = filepath.Join(retirement.RetiredDir(), "identity-"+now.UTC().Format("2006-01-02T15:04:05Z"))

	return nil
}

// nodePath is one path a node section names, with its effective default.
type nodePath struct{ name, path string }

// nodePathsOf enumerates every path the node section names, defaults
// applied by the parse, empties skipped.
func nodePathsOf(cfg *config.Config) []nodePath {
	if cfg.Node == nil {
		return nil
	}

	n := cfg.Node

	var out []nodePath

	add := func(name, path string) {
		if path != "" {
			out = append(out, nodePath{name, path})
		}
	}

	add("node.state_dir", n.StateDir)
	add("node.lock_dir", n.LockDir)

	if n.TLS != nil {
		add("node.tls.cert", n.TLS.CertPath)
		add("node.tls.key", n.TLS.KeyPath)
		add("node.tls.ca", n.TLS.CAPath)
	}

	if n.Ceph != nil {
		add("node.ceph.conf_path", n.Ceph.ConfPath)
		add("node.ceph.keyring_path", n.Ceph.KeyringPath)
	}

	if n.Firecracker != nil {
		add("node.firecracker.binary_path", n.Firecracker.BinaryPath)
		add("node.firecracker.jailer_path", n.Firecracker.JailerPath)
		add("node.firecracker.kernel_image", n.Firecracker.KernelImage)
		add("node.firecracker.kernel_dir", n.Firecracker.KernelDir)
		add("node.firecracker.chroot_base", n.Firecracker.ChrootBase)
	}

	if n.Cache != nil {
		add("node.cache.tls_cert", n.Cache.TLSCert)
		add("node.cache.tls_key", n.Cache.TLSKey)
	}

	return out
}

// applyRetireIntent records the decision, in the order every remainder is a
// row of the table: the guard's marker (so the guard is released by nobody
// from here on), the stage (a retained node's exact bytes), the journal's
// intent, the row's intent, the status.
func applyRetireIntent(ctx context.Context, m retireMode, root *txLock, dir *os.File, shape claimShape, db *state.DB,
	plan *retirePlan,
) (any, *retireRefusal) {
	now := retireNow()

	if shape.Guard.Transition == nil {
		record := shape.Guard
		record.Transition = &guardTransition{Kind: transitionRetirement, ID: plan.row.TransitionID}

		if err := rewriteGuardRecord(root, dir, record); err != nil {
			return nil, retireUnknown(retireReasonMarker, "write the guard's marker: "+err.Error(), "")
		}
	} else {
		// A MARKER THIS INVOCATION CAN SEE IS NOT YET A MARKER THAT SURVIVES A
		// POWER LOSS: an earlier invocation may have renamed the record and
		// died before its flushes, and everything below rests on the marker
		// being there when the host comes back.
		if err := syncDirFD(dir); err != nil {
			return nil, retireUnknown(retireReasonMarker, err.Error(), "")
		}

		if err := syncDirFD(root.dir); err != nil {
			return nil, retireUnknown(retireReasonMarker, err.Error(), "")
		}
	}

	j := retirement.Journal{
		Schema: retirement.JournalSchema, Phase: retirement.PhaseIntent, Variant: plan.variant,
		Deployment: plan.identity, Retiring: m.retiringHost, Survivor: plan.survivor,
		Backend: string(config.StatePostgres), Controllers: string(config.ControllersActivePassive),
		IdentityDir: plan.cfg.Server.IdentityDir, Archive: plan.archive,
		InstalledSHA256: plan.installed, Config: "absent",
		Nodes: plan.nodes, EndpointFailoverVerified: plan.failover,
		Locator: retirement.JournalLocator{Backend: string(config.StatePostgres), DSNEnv: plan.cfg.Server.LedgerDSNEnv(),
			EnvironmentFile: m.environmentFile, IdentityDir: plan.cfg.Server.IdentityDir, Archive: plan.archive},
		Provenance: retirement.Provenance{ReservingHolder: plan.row.Run, TransitionID: plan.row.TransitionID,
			Reservation: plan.row.ReservedAt, Deployment: plan.identity, Retiring: m.retiringHost, Survivor: plan.survivor.Host},
		Ownership: retirement.Ownership{Owner: m.run},
	}

	if plan.variant == retirement.VariantRetainedNode {
		if err := retirement.WriteStage(plan.rendering); err != nil {
			return nil, retireUnknown(retireReasonStage, err.Error(), "")
		}

		j.StagedSHA256, j.Config = retirement.Digest(plan.rendering), "present"
	}

	if err := j.Write(now); err != nil {
		return nil, retireUnknown(retireReasonJournal, "write the journal's intent: "+err.Error(), "")
	}

	if err := db.AdvanceRetirementToIntent(ctx, plan.identity, m.retiringHost, plan.row.TransitionID, m.run, now); err != nil {
		return nil, retireUnknown(retireReasonLedger, "advance the row to intent: "+err.Error(), "")
	}

	if err := retirement.WriteStatus(retirement.PhaseIntent, plan.variant, now); err != nil {
		return nil, retireUnknown(retireReasonStatus, "publish the status: "+err.Error(), "")
	}

	return j, nil
}
