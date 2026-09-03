package deployarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// FenceReason is what a restore writes into the maintenance fence.
//
// EXACT AND STABLE, because ClearMaintenanceFence compares it: a restore clears
// only the fence a restore established, so it can never reopen a ledger somebody
// else — an Ansible host upgrade, say — had closed for their own operation.
const FenceReason = "billet local restore"

// RecoverFenceReason is what `billet local recover` writes instead.
//
// ITS OWN STRING, because ClearMaintenanceFence compares the reason exactly:
// a command clears only the fence it established, so it can never reopen a
// ledger somebody else closed. The consequence is worth stating rather than
// discovering — a crashed recover is finished or abandoned by `billet local
// recover`, not by `billet local restore`, and both diagnostics say which.
const RecoverFenceReason = "billet local recover"

// fenceReasonFor is the one an operation writes and clears.
//
// A FUNCTION RATHER THAN A METHOD ON Plan, because an abandon has an intent and
// no plan: it is undoing a run whose plan is gone.
func fenceReasonFor(intent Intent) string {
	if intent == ReplaceLedger {
		return RecoverFenceReason
	}

	return FenceReason
}

// journalFile is the durable record of a restore in progress.
const journalFile = "restore.journal"

// journalSchema is this build's journal format. A journal from a version that
// wrote a different one is refused rather than interpreted: what it lists is
// what an abandon DELETES.
//
// 2 ADDED THE INTENT AND THE SUPERSEDED SET'S DIGESTS, and 3 CHANGED WHAT THOSE
// DIGESTS ARE KEYED BY — full pathname to base name. Both are bumps rather than
// bookkeeping, and the second is the one worth stating: a format whose MEANING
// changes under a number it already used is worse than one that changes number,
// because every reader believes it understands the file. A schema-2 journal's
// keys are full paths, so this build would look up `billet.db`, find nothing,
// and call the operator's own superseded ledger unaccounted for — refusing the
// abandon permanently, over a record it had just declared it understood.
//
// A schema-1 journal carries neither field at all, so an abandon reading one has
// no proof of which ledger it is meant to put back and no way to tell a
// restore's record from a recovery's. Accepting either silently would leave
// exactly the deployment-losing path these fields exist to close, in the file
// that authorises it. Pre-1.0 is allowed to break an on-disk format; it is not
// allowed to break it confusingly, so the refusal names what to do.
const journalSchema = 3

// The phases a journal passes through. WHAT THEY DECIDE IS WHETHER A RE-RUN
// PUBLISHES AGAIN, so the distinction is not bookkeeping: a recovery whose
// publication finished has already moved the operator's ledger aside, and
// re-planning one would find that destination occupied and refuse — a dead end
// reached through the command whose job is ending one.
const (
	// phasePublishing: files are still going in. A re-run resumes the
	// publication, which is idempotent by construction.
	phasePublishing = "publishing"
	// phasePublished: every file is in place and the caller has work left to do
	// behind the fence. A re-run must NOT publish again.
	phasePublished = "published"
	// phaseFinished: the caller is done and the fence is coming down. The
	// journal outlives the fence by one step so that a crash between them leaves
	// something that says which side of it this directory is on — and an abandon
	// refuses to undo a recovery that reached here.
	phaseFinished = "finished"
)

// journal is what survives a crash mid-restore.
//
// IT IS DELIBERATELY SMALL, because the publication does most of the work
// itself: every install is no-clobber and every decision is idempotent — absent
// installs, identical is already done, different refuses — so re-running the
// command re-derives the same plan and continues from wherever it stopped.
//
// What the journal adds is the three things that reasoning cannot recover:
// which archive this directory is part-way through (so a second, DIFFERENT
// backup cannot be interleaved into it), that the empty preflight ledger was
// already removed, and which paths this run created — which is the only list an
// abandon is allowed to delete from.
type journal struct {
	Schema int    `json:"schema"`
	Phase  string `json:"phase,omitempty"`
	// Intent is which operation wrote this, in Intent.String()'s words.
	//
	// RECORDED RATHER THAN INFERRED FROM THE FENCE, because the fence can be
	// absent in states the journal outlives — Finish takes it down one step
	// before removing the journal — and because two readers were guessing from
	// what they could see. It is what lets a diagnostic name the abandon that
	// will actually do something instead of the one that clears a fence and
	// returns.
	Intent string `json:"intent,omitempty"`
	// SupersededDigests is every file a recover moved aside, keyed by the BASE
	// NAME it has to come back under, and it is the only proof an abandon has
	// that it put THOSE files back.
	//
	// THE BASE NAME AND NOT THE PATH, because the two runs that write and read
	// this are separate invocations of the command and each builds the directory
	// from its own config: a trailing separator, a symlinked parent or a `.`
	// component makes an equal directory an unequal string, and every recorded
	// file would then read as one that never came back. That refusal is safe and
	// it is still wrong, and the next thing anybody does with a check that
	// refuses correct state is delete it. Within one state directory the base
	// name is unambiguous, which is all this has to be.
	//
	// THE WHOLE SET, NOT THE LEDGER. Proving billet.db is the right one does not
	// prove the ledger is right: SQLite REPLAYS a -wal it finds beside a
	// database, so a substituted log moved back alongside a correct ledger is the
	// same corruption arriving through the file that was not checked. And without
	// any of it, "a regular file called billet.db is there" is what authorises
	// removing the journal and lifting the fence — satisfied by an empty file
	// somebody dropped in while the directory was fenced, after which the next
	// control plane initialises it as a fresh capacity record and the
	// deployment's own is gone.
	//
	// RECORDED BEFORE THE FIRST RENAME, so a crash leaves a claim about files
	// that did not move rather than moved files nothing describes.
	SupersededDigests map[string]string `json:"superseded_digests,omitempty"`
	ArchiveDir        string            `json:"archive_dir"`
	ManifestSHA       string            `json:"manifest_sha256"`
	DeploymentID      string            `json:"deployment_id"`
	StartedAt         string            `json:"started_at"`
	Actor             string            `json:"actor"`
	Created           []string          `json:"created"`
	RemovedFiles      []string          `json:"removed_files,omitempty"`
}

// RestoreRequest is what Execute acts on.
type RestoreRequest struct {
	Plan Plan

	// InstallAppKey publishes the App private key at a path that must not
	// already exist, creating it exactly once and never replacing anything.
	//
	// INJECTED RATHER THAN WRITTEN HERE. `billet github-app create` already owns
	// that publication — a sibling reservation, an os.Link that fails rather
	// than replaces, and a recovery path that never declares a credential lost
	// while its bytes are still in memory — and four review rounds went into it.
	// A restore installing the same kind of file must use the same code, not a
	// second implementation of the same idea.
	InstallAppKey func(path string, pem []byte) error

	// Now is the clock, so a test can pin the journal.
	Now func() time.Time
	// Actor is who ran this, recorded in the journal for whoever finds it.
	Actor string
}

// Result is what a restore did.
type Result struct {
	Installed []Action
	Skipped   []Action
	Removed   []string
	// Resumed says a journal from an earlier interrupted run was picked up.
	Resumed bool
	// Strays are staging files a publication could not remove. NOT a failure —
	// the destination is installed and correct — but never swallowed either: a
	// second copy of a restored ledger that nothing mentions is one nobody
	// finds, which is the rule the App-key installer already follows.
	Strays []string
	// Superseded names what a recover moved aside. It is the only surviving
	// record of the work that operation failed, so it is reported rather than
	// left for somebody to notice.
	Superseded []string
	// Unfinished says the publication succeeded and the directory is STILL
	// fenced, with its journal in place, because the caller has something left to
	// do behind that fence. Only a recovery reaches it; call Finish afterwards.
	Unfinished bool
}

// Execute publishes an archive into a target state directory.
//
// THE EXCLUSION IS THREE THINGS AND NONE OF THEM IS SUFFICIENT ALONE:
//
//  1. The state-directory lock proves no CONTROL PLANE holds this directory.
//     It proves nothing about an operator command, which opens through
//     OpenAdmin deliberately without it.
//  2. The maintenance fence closes the ledger to every new handle AND to
//     handles that are already open — Tx and View consult it on entry — which
//     is the only thing that reaches an admin command already in flight.
//  3. The writer barrier proves that a transaction which began BEFORE the fence
//     has finished. The fence is checked when a transaction starts, so a handle
//     that got past it a moment earlier is still free to commit.
//
// AND NONE OF THE THREE REACHES ANOTHER MACHINE. Restoring this archive on a
// second host, or under a second path, produces two authoritative controllers
// sharing one identity, one CA and one App credential with divergent ledgers.
// That fence is the operator's to establish and the command's to insist on
// before calling this.
//
// PUBLICATION NEVER REPLACES THE STATE DIRECTORY. Files go in one at a time,
// so <stateDir>/billet.lock keeps the inode this call is holding — staging a
// directory and renaming it into place would leave the restorer holding the OLD
// lock while a control plane happily locks the new one.
func Execute(ctx context.Context, req RestoreRequest) (Result, error) {
	if len(req.Plan.Refusals) > 0 {
		return Result{}, errors.New(
			"deployarchive: this restore was refused and must not be executed")
	}

	if err := req.Plan.Intent.valid(); err != nil {
		return Result{}, err
	}

	if req.InstallAppKey == nil {
		return Result{}, errors.New("deployarchive: a restore needs an App key installer")
	}

	if req.Now == nil {
		return Result{}, errors.New("deployarchive: a restore needs a clock")
	}

	stateDir := req.Plan.Target.StateDir

	// The directory has to exist before it can be locked, and 0700 because it is
	// about to hold a CA private key.
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("deployarchive: create %s: %w", stateDir, err)
	}

	if err := os.Chmod(stateDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("deployarchive: tighten %s: %w", stateDir, err)
	}

	lock, err := state.LockStateDir(stateDir)
	if err != nil {
		return Result{}, err
	}

	// THE AUTHORITY LOCK TOO, because this WRITES the same five files `billet ca
	// rotate` mutates — and rotate and retire take only that lock, not this
	// directory's. Without it a `ca retire` running alongside a restore can
	// delete the ca-previous pair this restore has just published, after which
	// the restore reports success and every node that has not renewed can no
	// longer verify the control plane.
	//
	// THE ORDER IS DIRECTORY THEN AUTHORITY, and it is the same order a backup
	// takes them in (its ledger handle may already hold the directory lock before
	// Write asks for the authority), so the two cannot deadlock against each
	// other.
	authority, err := wirecert.LockAuthority(stateDir)
	if err != nil {
		return Result{}, errors.Join(err, lock.Release())
	}

	res, runErr := executeLocked(ctx, req)

	return res, errors.Join(runErr, authority.Release(), lock.Release())
}

func executeLocked(ctx context.Context, req RestoreRequest) (Result, error) {
	stateDir := req.Plan.Target.StateDir

	reason := fenceReasonFor(req.Plan.Intent)

	fenced, err := state.WriteMaintenanceFence(stateDir, reason)
	if err != nil {
		return Result{}, err
	}

	// A FENCE THIS CALL RAISED COMES DOWN IF NOTHING WAS PUBLISHED — AND ONLY IF
	// NOTHING WAS PUBLISHED BY ANY EARLIER RUN EITHER.
	//
	// Everything between here and the journal can refuse: the barrier can time
	// out, the re-derived plan can disagree with the printed one. On a directory
	// nothing has touched, each of those leaves the deployment exactly as it was,
	// and leaving it fenced would take a healthy control plane offline over an
	// operation that changed nothing.
	//
	// BUT A JOURNAL ALREADY THERE MEANS AN EARLIER RUN PUBLISHED SOMETHING, and
	// its fence may have gone missing since. Raising one and then clearing it on
	// a refusal would leave a half-restored directory startable — a control plane
	// would find whichever pieces landed and MINT the rest, which is the exact
	// catastrophe this command exists to prevent. So the journal is read first,
	// under the locks, and its presence pins the fence up.
	//
	// A JOURNAL THIS CALL CANNOT READ COUNTS AS PRESENT. "Could not tell" is
	// never "there is nothing here", least of all when the answer decides
	// whether a deployment may be started.
	prior, journalErr := InProgress(stateDir)
	priorJournal := prior.Present || journalErr != nil

	undoFence := func(cause error) (Result, error) {
		if !fenced || priorJournal {
			return Result{}, cause
		}

		return Result{}, errors.Join(cause, state.ClearMaintenanceFence(stateDir, reason))
	}

	if err := state.WriterBarrier(ctx, stateDir); err != nil {
		return undoFence(err)
	}

	// THE PLAN IS RE-DERIVED HERE, INSIDE THE EXCLUSION, AND THAT IS THE ONLY
	// PLAN ACTED ON.
	//
	// The one the caller handed in was computed with NO lock — deliberately,
	// because --dry-run must be able to report on a live deployment without
	// touching it — and then printed for a person to read. Minutes can pass, and
	// an operator command holding an OpenAdmin handle can commit in that window.
	// Acting on the old answer would mean deleting a ledger on the strength of a
	// PeekLedger from before it had rows in it, or skipping a credential as
	// "already present" whose bytes have since changed. The barrier proves the
	// writers are finished; it says nothing about what they wrote.
	fresh, err := planFor(ctx, req.Plan.Archive, req.Plan.Target, req.Plan.Intent)
	if err != nil {
		return undoFence(err)
	}

	if len(fresh.Refusals) > 0 {
		return undoFence(fmt.Errorf(
			"deployarchive: %s changed between this restore being planned and being run, and it "+
				"is no longer safe: %s. Nothing was published; run the restore again to see the "+
				"current plan", stateDir, renderRefusals(fresh.Refusals)))
	}

	// AND IT MUST BE THE SAME PLAN, not merely one that is still permitted.
	// Refusing only on refusals lets the operation WIDEN silently: a preflight
	// ledger appearing between the plan and the run turns an Install into a
	// ReplaceEmptyLedger, which deletes a file, and the person who read the plan
	// approved no such thing. A difference in either direction means the target
	// moved, and the honest answer is to show them the new plan rather than act
	// on a decision they did not make.
	//
	// LedgerSidecars is deliberately not compared: it is observational, and the
	// executor removes sidecars by name at the moment it acts precisely because
	// what the planner saw is stale by then.
	diff, invalid := actionsDiffer(req.Plan.Actions, fresh.Actions)

	switch {
	case invalid != "":
		// NOT A RACE, AND MUST NOT BE REPORTED AS ONE. A duplicate or empty
		// action entry is a planner invariant that has failed; telling an
		// operator their target moved would send them looking at their host for
		// something that is wrong in billet.
		return undoFence(fmt.Errorf(
			"deployarchive: this restore plan is not valid (%s). That is a defect in billet "+
				"rather than anything about %s; nothing was published", invalid, stateDir))
	case diff != "":
		return undoFence(fmt.Errorf(
			"deployarchive: %s changed between this restore being planned and being run (%s). "+
				"Nothing was published; run the restore again to see the current plan",
			stateDir, diff))
	}

	req.Plan = fresh

	j, resumed, err := openJournal(req)
	if err != nil {
		return undoFence(err)
	}

	res := Result{Resumed: resumed}

	if err := publish(&res, req, j); err != nil {
		// THE FENCE STAYS UP. A half-restored directory must not become
		// startable: a control plane opening it would find whichever pieces did
		// land, and the pieces that did not are the ones that make it this
		// deployment. The journal stays too, so a re-run resumes and an abandon
		// knows what to remove.
		return res, err
	}

	// A RECOVERY IS NOT FINISHED HERE, AND THE FENCE MUST NOT COME DOWN YET.
	//
	// The ledger now in place is the ARCHIVE's, and its admission row is whatever
	// it was when the backup was taken — open, almost always. The deployment this
	// recovery is putting back has nodes still holding compute that ledger has
	// never heard of, so between this point and the caller re-sealing it, a
	// control plane that started would advertise capacity and admit work. The
	// fence is the only thing that stops one, so it stays up and the caller calls
	// Finish once the seal is durable. A caller that never gets that far leaves a
	// fenced directory, which is the safe direction: nothing starts.
	if req.Plan.Intent == ReplaceLedger {
		// RECORDED BEFORE RETURNING, because this is the fact that makes the
		// caller's next attempt safe: a recovery that crashed after this point has
		// already moved the operator's ledger aside, and a re-run that planned
		// afresh would find that destination occupied and refuse. The phase is
		// what lets the command skip straight to finishing.
		if err := j.setPhase(stateDir, phasePublished); err != nil {
			return res, err
		}

		res.Unfinished = true

		return res, nil
	}

	// THE JOURNAL GOES BEFORE THE FENCE. Between the two the directory is
	// complete and still closed, which is the harmless order; the other way
	// round leaves a moment where a control plane could start on a directory
	// billet still considers unfinished.
	if err := j.remove(stateDir); err != nil {
		return res, err
	}

	if err := state.ClearMaintenanceFence(stateDir, reason); err != nil {
		return res, err
	}

	return res, nil
}

// Finish takes a directory Execute deliberately left closed and opens it.
//
// ONLY A RECOVERY REACHES THIS, and only after its caller has made the restored
// deployment safe to start — which for `billet local recover` means sealing the
// ledger the archive brought back, whose admission row is the one it had when
// the backup was taken. Execute cannot do that itself: sealing means writing to
// the ledger, the ledger is fenced, and the only handle that crosses a fence
// takes the directory lock Execute is holding. So the sequence is Execute,
// caller seals, Finish — and every instant in between is one where the fence
// still refuses a control plane.
func Finish(ctx context.Context, plan Plan) error {
	if err := plan.Intent.valid(); err != nil {
		return err
	}

	stateDir := plan.Target.StateDir

	lock, err := state.LockStateDir(stateDir)
	if err != nil {
		return err
	}

	return errors.Join(finishLocked(ctx, plan), lock.Release())
}

// requireSealed refuses to lift a fence over a ledger that would take work.
//
// UNDER THE CALLER'S OWN LOCK, THROUGH state.PeekAdmission. The two ordinary
// openers are both wrong here and for opposite reasons — OpenAdmin honours the
// fence this is about to lift, and OpenMaintenance crosses it but takes the
// directory lock the caller is already holding — so asking before the lock was
// the first shape, and it MIGRATED the ledger before anything had established
// the operation was this caller's, and left a window between the answer and the
// lock in which admission could change. This reads the row where it is acted on.
func requireSealed(ctx context.Context, stateDir string) error {
	a, err := state.PeekAdmission(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("deployarchive: read %s's admission before lifting its fence: %w",
			stateDir, err)
	}

	// THE MODE IS COMPARED, NOT ASKED WHETHER IT IS "SEALED". Admission.Sealed()
	// answers true for a row billet could not read, which is the right answer to
	// "may I admit work" and the wrong one here: this needs POSITIVE proof that a
	// person closed this deployment, and an unrecognised mode beside an operator
	// provenance would supply it.
	//
	// THE LEDGER'S CHECK CONSTRAINT MAKES THAT UNREACHABLE TODAY, which is said
	// here so nobody folds this back into Sealed() believing the two are the same
	// question. They are not, and the constraint is the only thing between them —
	// a schema that ever widens that column would turn "billet could not tell"
	// into "a person sealed this" at the one place that lifts the fence.
	switch {
	case a.Mode != state.AdmissionSealed:
		return fmt.Errorf(
			"deployarchive: %s holds the recovered ledger and its admission reads %q rather "+
				"than sealed, so billet will not lift the fence: a control plane starting on it "+
				"would admit work while its nodes still hold compute this ledger has never "+
				"heard of. Seal it and run this again", stateDir, a.Mode)
	case a.Provenance != state.ProvenanceOperator:
		return fmt.Errorf(
			"deployarchive: %s is sealed by %s, and a recovered deployment needs an operator's "+
				"seal — a lifecycle seal is cleared by the next `billet local up`, which would "+
				"reopen it by restarting the services", stateDir, a.Provenance)
	}

	return nil
}

func finishLocked(ctx context.Context, plan Plan) error {
	stateDir := plan.Target.StateDir

	// THE BARRIER AGAIN, because the caller opened the ledger between Execute and
	// this: a transaction of its own could still be settling, and the journal
	// must not be removed while anything is mid-write behind the fence.
	if err := state.WriterBarrier(ctx, stateDir); err != nil {
		return err
	}

	// AND THE JOURNAL MUST BE THIS OPERATION'S OWN. This is an exported entry
	// point, so it cannot assume its caller just ran the Execute that raised this
	// fence: handed a plan for one archive while the directory holds a journal for
	// ANOTHER, an unconditional removal would delete the record of somebody else's
	// half-published restore and then open the directory for a control plane to
	// start on it — which is the exact catastrophe the journal and the fence exist
	// to prevent, reached through the function that is supposed to end one safely.
	raw, err := os.ReadFile(JournalPath(stateDir))

	var (
		j     journal
		found bool
	)

	switch {
	case errors.Is(err, os.ErrNotExist):
		// Nothing to remove — this call is finishing a run whose journal is
		// already gone, which is the state a crash between the two steps below
		// leaves. The fence is still this operation's to clear:
		// ClearMaintenanceFence compares the reason, so it can only lift one this
		// intent raised.
	case err != nil:
		return fmt.Errorf("deployarchive: read %s: %w", JournalPath(stateDir), err)
	default:
		found = true

		if err := json.Unmarshal(raw, &j); err != nil {
			return fmt.Errorf(
				"deployarchive: %s does not parse (%w), and billet will not open a directory it "+
					"cannot tell is finished", JournalPath(stateDir), err)
		}

		if err := j.understood(stateDir); err != nil {
			return err
		}

		if plan.Archive == nil || j.ManifestSHA != plan.Archive.manifestDigest() {
			return fmt.Errorf(
				"deployarchive: %s records a restore from %s, which is not the archive this "+
					"operation published; billet will not remove it or lift the fence over it",
				JournalPath(stateDir), j.ArchiveDir)
		}

		// AND THE SAME OPERATION. The archive alone does not separate a restore
		// from a recovery of it, and the two leave different things behind — a
		// recovery's journal names a superseded ledger a restore has no concept
		// of. The fence check below says the same thing from the other side and
		// can be absent; this one cannot.
		if j.Intent != plan.Intent.String() {
			return fmt.Errorf(
				"deployarchive: %s was written by a %s and this is finishing a %s; billet will "+
					"not remove another operation's journal or lift its fence",
				JournalPath(stateDir), j.Intent, plan.Intent)
		}

		// AND THE PUBLICATION MUST HAVE FINISHED. Lifting the fence is what makes
		// a directory startable, and a journal still reading "publishing" says
		// files are missing from it — an exported entry point that took the fence
		// down over that would hand a control plane a deployment with holes in it.
		if j.phase() == phasePublishing {
			return fmt.Errorf(
				"deployarchive: %s records a publication that had not finished, so billet will "+
					"not lift the fence over %s. Run the operation again to finish it, or "+
					"--abandon to undo it", JournalPath(stateDir), stateDir)
		}

		// THE FENCE MUST BE THIS OPERATION'S TOO, AND IT IS ASKED BEFORE ANYTHING
		// IS REMOVED. ClearMaintenanceFence checks it at the end, which is too
		// late: a restore's plan handed a recovery's journal for the same archive
		// would already have deleted it by then, leaving a fenced directory whose
		// only record of what to undo is gone.
		switch found, fenced, err := state.MaintenanceFenceReason(stateDir); {
		case err != nil:
			return err
		case fenced && found != fenceReasonFor(plan.Intent):
			return fmt.Errorf(
				"deployarchive: %s is fenced for %q, and this is finishing %q; billet will not "+
					"remove a journal or lift a fence belonging to another operation",
				stateDir, found, fenceReasonFor(plan.Intent))
		}
	}

	// PROVED HERE RATHER THAN ASSUMED OF THE CALLER, and under the same lock and
	// in the same breath as the clear below. The whole reason a recovery leaves
	// its fence up is that the ledger the archive brought back carries the
	// admission row it had when the backup was taken — open, almost always — and
	// this function is what takes that fence down. An exported entry point that
	// merely documented "seal first" would open exactly the window the fence
	// exists to close, and no test of the caller can show that it did not.
	if plan.Intent == ReplaceLedger {
		if err := requireSealed(ctx, stateDir); err != nil {
			return err
		}
	}

	// AND THE JOURNAL OUTLIVES THE FENCE BY ONE STEP. Removing it first is what
	// makes a failed clear a dead end: the directory is then fenced with nothing
	// left to say the publication had finished, so a re-run plans afresh, finds
	// the superseded ledger's destination occupied and refuses. Marking it
	// finished before the fence comes down means a crash on either side leaves a
	// state the next run recognises — and it is what an abandon reads to refuse
	// undoing a recovery that is complete.
	if found {
		if err := j.setPhase(stateDir, phaseFinished); err != nil {
			return err
		}
	}

	if err := state.ClearMaintenanceFence(stateDir, fenceReasonFor(plan.Intent)); err != nil {
		return err
	}

	// THE UNLINK IS THE LAST THING THAT CAN FAIL, AND ITS DIRECTORY SYNC IS NOT
	// ALLOWED TO. The fence is already durably down, so both outcomes of this
	// unlink are safe — the journal is gone, or a crash brings it back and the
	// next run recognises phase "finished" and removes it. Reporting a failed
	// sync as an error sent an operator to re-run a command that would then plan
	// a SECOND supersede against a destination the first one is holding, which is
	// a dead end reached by following billet's own instruction.
	if err := os.Remove(JournalPath(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deployarchive: remove %s: %w", JournalPath(stateDir), err)
	}

	//nolint:errcheck // see above: after a durable fence clear and a successful
	// unlink, neither outcome of this sync is unsafe, and reporting it as a
	// failure is what created the dead end.
	syncDir(stateDir)

	return nil
}

// understood refuses a journal this build cannot act on.
//
// SCHEMA AND PHASE ARE BOTH CLOSED SETS, and reading past either is how a
// downgrade acts on a record it does not understand: a newer binary's journal
// carrying the same manifest digest and a phase this one has never heard of was
// treated as "not published", and a phase it happens to share was acted on
// wholesale. An absent phase is the schema-1 spelling of "publishing" and is the
// one thing that is not refused.
func (j *journal) understood(stateDir string) error {
	if j.Schema != journalSchema {
		return fmt.Errorf(
			"deployarchive: %s is journal schema %d and this billet writes %d, so billet will "+
				"not act on it: it is the record that says which files may be deleted and which "+
				"ledger must come back, and a version that wrote a different format may have "+
				"recorded neither. Reconcile %s by hand — the ledger is whichever billet.db or "+
				"billet.db.superseded-* file holds this deployment's capacity record — then "+
				"remove that journal and the maintenance fence beside it",
			JournalPath(stateDir), j.Schema, journalSchema, stateDir)
	}

	switch j.Phase {
	case "", phasePublishing, phasePublished, phaseFinished:
	default:
		return fmt.Errorf(
			"deployarchive: %s says its phase is %q, which this billet does not recognise",
			JournalPath(stateDir), j.Phase)
	}

	switch j.Intent {
	case RestoreFresh.String(), ReplaceLedger.String():
		return nil
	default:
		return fmt.Errorf(
			"deployarchive: %s says it was written by %q, which is not an operation this billet "+
				"has", JournalPath(stateDir), j.Intent)
	}
}

// actionsDiffer names the first difference between two plans' actions.
//
// TWO ANSWERS, BECAUSE THEY MEAN DIFFERENT THINGS. The first is a legitimate
// difference — the target moved between the plan being printed and being run.
// The second is an invalid plan, which is a defect in billet, and reporting one
// as the other sends an operator to inspect a host that is fine.
//
// ENTRY, PATH AND DISPOSITION — everything that decides what happens to a file.
// Compared as a set keyed by entry, because the ORDER is the executor's to
// impose (see publicationRank) rather than the planner's to promise.
func actionsDiffer(was, now []Action) (string, string) {
	// AN EMPTY OR DUPLICATE ENTRY IS REPORTED RATHER THAN INDEXED. Both would
	// collapse two actions into one map slot, and the one that survived would
	// decide the comparison — so a changed action could hide behind a repeated
	// key. Production emits exactly one action per non-empty entry; anything
	// else is a bug in the planner, and this is a comparison whose job is to
	// notice things.
	index := func(in []Action, which string) (map[string]Action, string) {
		out := make(map[string]Action, len(in))

		for _, a := range in {
			if a.Entry == "" {
				return nil, fmt.Sprintf("the %s plan has an action with no archive entry (%s)",
					which, a.Path)
			}

			if _, dup := out[a.Entry]; dup {
				return nil, fmt.Sprintf("the %s plan names %s twice", which, a.Entry)
			}

			out[a.Entry] = a
		}

		return out, ""
	}

	before, bad := index(was, "printed")
	if bad != "" {
		return "", bad
	}

	after, bad := index(now, "current")
	if bad != "" {
		return "", bad
	}

	// SORTED, so the difference reported is the same one every run. Map order is
	// unspecified, and a diagnostic that names a different item each time is one
	// an operator cannot compare against the last.
	keys := make([]string, 0, len(before)+len(after))
	for entry := range before {
		keys = append(keys, entry)
	}

	for entry := range after {
		if _, ok := before[entry]; !ok {
			keys = append(keys, entry)
		}
	}

	sort.Strings(keys)

	for _, entry := range keys {
		a, had := before[entry]
		b, has := after[entry]

		switch {
		case had && !has:
			return fmt.Sprintf("%s is no longer part of the plan", a.What), ""
		case !had && has:
			return fmt.Sprintf("%s is now part of the plan and was not", b.What), ""
		case a.Path != b.Path:
			return fmt.Sprintf("%s would now go to %s rather than %s", a.What, b.Path, a.Path), ""
		case a.Disposition != b.Disposition:
			return fmt.Sprintf("%s was %q and is now %q", a.What, a.Disposition, b.Disposition), ""
		}
	}

	return "", ""
}

// renderRefusals joins refusals into one sentence, for an error rather than a
// report. The command layer renders them properly; this is for the case where
// the world moved after a plan was already printed.
func renderRefusals(refusals []lifeops.Refusal) string {
	out := make([]string, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, r.What)
	}

	return strings.Join(out, "; ")
}

// onPublish observes each item just before it is published. Nil in production.
var onPublish func(a Action) error

// onPublished fires after the LAST item and before the journal and the fence
// come down. Nil in production.
//
// A SEPARATE HOOK BECAUSE THAT WINDOW IS ITS OWN CASE: a run stopped there has
// installed everything and is still unfinished, so the resume's plan has nothing
// to install and must clear both marks anyway.
var onPublished func() error

// publish installs each item in an order the executor owns.
func publish(res *Result, req RestoreRequest, j *journal) error {
	actions := slices.Clone(req.Plan.Actions)

	// THE ORDER IS THE CRASH ARGUMENT AND IT IS ENFORCED HERE, not inherited
	// from whatever sequence the planner happened to append in. It mirrors how
	// an authority is CREATED: the key leads its certificate, so an interruption
	// between them leaves the half-initialised state billet refuses loudly
	// rather than a certificate whose key belongs to something else; and the
	// marker is last, because its whole job is to make a LATER absence mean
	// loss — written first, an interruption would leave a directory claiming an
	// authority that was never installed, which is the one state
	// ErrAuthorityLost cannot be talked out of.
	//
	// THE PREVIOUS PAIR ORDERS THE OTHER WAY, and the reason is that it is
	// OPTIONAL where the current pair is required. Half of the current pair must
	// refuse loudly, so its key leads. Half of the previous pair must be INERT —
	// wirecert publishes ca-previous.crt first for exactly that reason, so a
	// certificate with no key beside it means "a rotation was started and is not
	// committed" and a control plane presents with the current authority instead
	// of refusing. Publishing them in the opposite order here would put a state
	// on disk that no rotation can produce. It also means an interrupted restore
	// leaves a public certificate rather than a private key in a deployment that
	// is not yet whole.
	sort.SliceStable(actions, func(i, k int) bool {
		return publicationRank(actions[i]) < publicationRank(actions[k])
	})

	for _, a := range actions {
		if a.Disposition == AlreadyPresent {
			res.Skipped = append(res.Skipped, a)

			continue
		}

		// A TEST HOOK, nil in production. An interruption partway through
		// publishing several credential files is the case this whole journal
		// exists for, and it cannot be staged from outside the package: the
		// window is between two syscalls. It fires BEFORE the journal note, so
		// stopping at index i models a crash that finished item i-1 completely.
		if onPublish != nil {
			if err := onPublish(a); err != nil {
				return err
			}
		}

		if a.Disposition == ReplaceEmptyLedger {
			if err := removeEmptyLedger(res, req, j, a); err != nil {
				return err
			}
		}

		if a.Disposition == SupersedeLedger {
			if err := supersedeLedger(res, req, j, a); err != nil {
				return err
			}
		}

		// RECORDED BEFORE IT IS CREATED. A crash between the two leaves a path
		// in the journal that was never made, which an abandon tolerates; the
		// other order leaves a credential on disk that nothing knows this run
		// put there.
		if err := j.note(req.Plan.Target.StateDir, a.Path); err != nil {
			return err
		}

		stray, err := installOne(req, a)
		if stray != "" {
			res.Strays = append(res.Strays, stray)
		}

		if err != nil {
			return err
		}

		res.Installed = append(res.Installed, a)
	}

	if onPublished != nil {
		return onPublished()
	}

	return nil
}

// publicationRank orders the publication.
func publicationRank(a Action) int {
	switch a.Entry {
	case EntryIdentity:
		return 0
	case EntryLedger:
		return 1
	case AuthorityEntry("ca.key"):
		return 2
	case AuthorityEntry("ca.crt"):
		return 3
	case AuthorityEntry("ca-previous.crt"):
		return 4
	case AuthorityEntry("ca-previous.key"):
		return 5
	case AuthorityEntry("authority-created"):
		return 6
	case EntryAppKey:
		return 7
	default:
		// An entry nothing here ranks goes last rather than first: whatever it
		// is, it is not one of the ordering constraints above.
		return 8
	}
}

// installOne publishes a single item.
func installOne(req RestoreRequest, a Action) (string, error) {
	body, ok := req.Plan.Archive.Entry(a.Entry)

	switch {
	case a.Entry == EntryLedger:
		// THE MANIFEST RECORD TRAVELS WITH THE COPY. Open verified the snapshot
		// by pathname, and this reopens that pathname; handing copyFile what the
		// manifest says pins the bytes that land to the bytes that were checked.
		rec, found := req.Plan.Archive.Manifest.Record(EntryLedger)
		if !found {
			return "", fmt.Errorf("deployarchive: this backup's manifest does not describe %s",
				EntryLedger)
		}

		return copyFile(req.Plan.Archive.LedgerPath(), a.Path, rec)
	case a.Entry == EntryAppKey:
		// THROUGH THE INJECTED INSTALLER, which creates the destination exactly
		// once and fails rather than replacing. See RestoreRequest.
		if !ok {
			return "", fmt.Errorf("deployarchive: this backup has no %s", EntryAppKey)
		}

		if err := req.InstallAppKey(a.Path, body); err != nil {
			return "", err
		}

		return "", syncDir(filepath.Dir(a.Path))
	case !ok:
		return "", fmt.Errorf("deployarchive: this backup has no %s", a.Entry)
	}

	// The CA certificate is not a secret and the key is; both go in at 0600,
	// because the state directory is 0700 and a wider mode on a restored file
	// buys nothing but a way to be wrong.
	if err := writeSmall(a.Path, body); err != nil {
		return "", err
	}

	return "", syncDir(filepath.Dir(a.Path))
}

// ledgerSidecarSuffixes are the files SQLite keeps beside a WAL database.
var ledgerSidecarSuffixes = []string{"-wal", "-shm"}

// LedgerSidecarPaths names what SQLite keeps beside a ledger.
//
// EXPORTED SO THE SUFFIXES HAVE ONE HOME. The command layer has to hand these
// back to the service account after a privileged restore, and a second copy of
// "-wal" and "-shm" somewhere else is a file a control plane cannot write and
// nothing explains.
func LedgerSidecarPaths(ledgerPath string) []string {
	out := make([]string, 0, len(ledgerSidecarSuffixes))
	for _, suffix := range ledgerSidecarSuffixes {
		out = append(out, ledgerPath+suffix)
	}

	return out
}

// removeEmptyLedger deletes the ledger a preflight created, with its sidecars.
//
// THE ONE DESTRUCTIVE STEP, and the planner has already proved it is safe: no
// deployment identity, no authority, and every table a deployment writes to
// empty. THE SIDECARS GO WITH IT — a -wal left beside a restored billet.db is
// replayed into a database it was never a journal for, which is corruption
// rather than a stale file.
//
// THE SUFFIXES ARE FIXED HERE RATHER THAN TAKEN FROM THE PLAN, and that is a
// correctness fix rather than tidiness. What sidecars exist at PLAN time is not
// what exists at EXECUTE time: PeekLedger opens the database to prove it is
// empty, and SQLite deletes the -wal and -shm when that connection closes — so
// a list built during planning is reliably EMPTY by the time it would be acted
// on, and anything that recreates one in between would survive. Mutation found
// this: deleting the sidecar handling entirely left the test green.
func removeEmptyLedger(res *Result, req RestoreRequest, j *journal, a Action) error {
	targets := []string{a.Path}

	for _, suffix := range ledgerSidecarSuffixes {
		if _, err := os.Lstat(a.Path + suffix); err == nil {
			targets = append(targets, a.Path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deployarchive: inspect %s: %w", a.Path+suffix, err)
		}
	}

	if err := j.noteRemoval(req.Plan.Target.StateDir, targets); err != nil {
		return err
	}

	for _, path := range targets {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deployarchive: remove the empty preflight ledger %s: %w", path, err)
		}

		res.Removed = append(res.Removed, path)
	}

	return syncDir(filepath.Dir(a.Path))
}

// supersedeLedger moves a populated ledger aside so the archive's can take its
// place.
//
// RENAMED, NEVER UNLINKED, and that is the difference between this and
// removeEmptyLedger one function up. That one deletes a ledger the planner has
// proved is an empty preflight artifact; this one moves a ledger holding an
// operator's own capacity record — the only surviving record of the jobs they
// have just accepted losing. Nothing here is entitled to destroy it.
//
// THE SIDECARS GO WITH IT, under the same name, because a -wal left beside the
// restored billet.db is replayed into a database it was never a journal for.
// That is corruption rather than a stale file, and it is why they move rather
// than being left where they are.
//
// IDEMPOTENT ON A RESUME. The destination name is derived from the ARCHIVE
// rather than from the clock, so a second pass computes the same one — and an
// already-moved ledger leaves nothing at the source, which os.Rename reports as
// absent and this treats as done. A blind rename on the second pass would move
// the ledger this run had just installed on top of the first pass's copy.
//
// THE SIDECARS MOVE FIRST AND THE LEDGER LAST, and that ORDER is the crash
// argument — the same shape the authority's publication uses. The ledger's
// presence is what makes a re-plan choose this function at all: with the ledger
// moved first, a crash before its -wal followed leaves the source with no
// ledger, so the re-plan reads it as an ordinary INSTALL and puts the archive's
// database down beside a write-ahead log belonging to a different one. SQLite
// replays that, which is corruption rather than a stale file. Moving the ledger
// last means every interrupted state still has a ledger at the source, so the
// re-plan comes back here and finishes the set.
//
// AND THE ORDER HAS TO BE MADE DURABLE, WHICH IS A SEPARATE FACT. Renames are
// metadata operations and nothing orders two of them, so one sync at the end
// bought nothing: a power loss could persist the LEDGER's rename and lose the
// -wal's, which is precisely the state the paragraph above says cannot happen —
// no ledger at the source, a stale write-ahead log at the canonical name, and a
// re-plan that reads it as an install. The sync between the two is what makes
// the argument true rather than intended.
func supersedeLedger(res *Result, req RestoreRequest, j *journal, a Action) error {
	if req.Plan.Superseded == "" {
		return errors.New("deployarchive: a supersede has nowhere to move the old ledger to")
	}

	var moves [][2]string

	for i, sidecar := range LedgerSidecarPaths(a.Path) {
		moves = append(moves, [2]string{sidecar, LedgerSidecarPaths(req.Plan.Superseded)[i]})
	}

	moves = append(moves, [2]string{a.Path, req.Plan.Superseded})

	// THE WHOLE SET'S DIGESTS, TAKEN BEFORE ANY OF IT MOVES AND WHILE EACH FILE
	// IS STILL AT ITS OWN NAME. This is what an abandon compares what it put back
	// against, so it has to describe the operator's ledger AND the log SQLite
	// would replay into it — and it is recorded once, because a resumed pass
	// finds the sources already gone.
	//
	// A CHECKPOINT CAN STILL LAND AFTER THIS. The fence stops transactions and
	// the writer barrier proves none is mid-write, but neither reaches a
	// connection that was already open and then CLOSES: SQLite checkpoints a WAL
	// into the database on the last close, which rewrites the file this has just
	// hashed. Nothing in userspace can hold that off — the lock an admin handle
	// skips is the one that would. What it costs is bounded and it is bounded the
	// safe way: the abandon refuses a ledger whose digest no longer matches,
	// keeping the fence and the journal and naming the file, rather than opening
	// a deployment over the wrong one.
	if j.SupersededDigests == nil {
		digests := map[string]string{}

		for _, move := range moves {
			switch _, err := os.Lstat(move[0]); {
			case errors.Is(err, os.ErrNotExist):
				continue
			case err != nil:
				return fmt.Errorf("deployarchive: inspect %s: %w", move[0], err)
			}

			sha, _, err := digestFile(move[0])
			if err != nil {
				return err
			}

			digests[filepath.Base(move[0])] = sha
		}

		j.SupersededDigests = digests

		if err := j.save(req.Plan.Target.StateDir); err != nil {
			return err
		}
	}

	for i, move := range moves {
		// THE LEDGER IS LAST, AND IT DOES NOT MOVE UNTIL EVERY SIDECAR BEFORE IT
		// IS DURABLE. See the ordering argument above.
		if i == len(moves)-1 {
			if err := syncDir(filepath.Dir(a.Path)); err != nil {
				return err
			}
		}

		switch _, err := os.Lstat(move[0]); {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("deployarchive: inspect %s: %w", move[0], err)
		}

		// THE DESTINATION MUST BE FREE. os.Rename REPLACES, and the one thing
		// that must never happen here is a second run writing over the ledger the
		// first run preserved — which holds the jobs an operator accepted losing.
		switch _, err := os.Lstat(move[1]); {
		case err == nil:
			return fmt.Errorf(
				"deployarchive: %s already exists, and billet will not write over a ledger it "+
					"moved aside — that file is the only record of the work this recovery "+
					"failed. Move it somewhere else and run this again", move[1])
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("deployarchive: inspect %s: %w", move[1], err)
		}

		// RECORDED BEFORE THE MOVE, for the reason every other publication here
		// records first: a crash between the two leaves a journal entry naming a
		// file that was never created, which an abandon tolerates — where the
		// other order leaves a ledger somewhere nothing knows to look.
		if err := j.note(req.Plan.Target.StateDir, move[1]); err != nil {
			return err
		}

		if err := os.Rename(move[0], move[1]); err != nil {
			return fmt.Errorf("deployarchive: move %s aside: %w", move[0], err)
		}

		res.Superseded = append(res.Superseded, move[1])
	}

	return syncDir(filepath.Dir(a.Path))
}

// openJournal picks up an interrupted run or starts one.
//
// A JOURNAL FROM A DIFFERENT ARCHIVE IS A REFUSAL. Half of one backup and half
// of another is not a deployment, and the pieces would each verify on their own.
func openJournal(req RestoreRequest) (*journal, bool, error) {
	stateDir := req.Plan.Target.StateDir
	manifestSHA := req.Plan.Archive.manifestDigest()

	path := filepath.Join(stateDir, journalFile)

	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, fmt.Errorf("deployarchive: read %s: %w", path, err)
		}

		j := &journal{
			Schema:       journalSchema,
			Intent:       req.Plan.Intent.String(),
			ArchiveDir:   req.Plan.Archive.Dir,
			ManifestSHA:  manifestSHA,
			DeploymentID: req.Plan.Archive.Manifest.DeploymentID,
			StartedAt:    req.Now().UTC().Format(time.RFC3339),
			Actor:        req.Actor,
		}

		if err := j.save(stateDir); err != nil {
			return nil, false, err
		}

		return j, false, nil
	}

	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, false, fmt.Errorf(
			"deployarchive: %s records a restore in progress and does not parse (%w). It lists "+
				"what an abandon would delete, so billet will not guess at it — resolve that "+
				"file by hand", path, err)
	}

	// ONE VALIDATOR FOR BOTH CLOSED SETS. The schema was checked here from the
	// start; the PHASE was not, and setPhase would then have overwritten a marker
	// this build had not understood with one of its own.
	if err := j.understood(stateDir); err != nil {
		return nil, false, fmt.Errorf(
			"%w. It lists what an abandon would delete, so billet will not interpret a record "+
				"it does not understand — resolve that file by hand", err)
	}

	// AND IT MUST BE THE SAME OPERATION. A restore resuming a recovery's journal
	// would inherit a Created list naming a superseded ledger it has no concept
	// of, and an abandon would then act on it as its own.
	if j.Intent != req.Plan.Intent.String() {
		return nil, false, fmt.Errorf(
			"deployarchive: %s records an interrupted %s of this archive and this is a %s. "+
				"Finish or abandon that one first — `billet local %s --from %s`",
			path, j.Intent, req.Plan.Intent, j.Intent, j.ArchiveDir)
	}

	if j.ManifestSHA != manifestSHA {
		return nil, false, fmt.Errorf(
			"deployarchive: %s records a restore already in progress from %s, and this one is "+
				"from %s. Half of one backup beside half of another is not a deployment — finish "+
				"or abandon that restore first (`billet local restore --from %s` to resume, "+
				"`--abandon` to undo it)", path, j.ArchiveDir, req.Plan.Archive.Dir, j.ArchiveDir)
	}

	return &j, true, nil
}

// note records a path this run is about to create.
func (j *journal) note(stateDir, path string) error {
	if slices.Contains(j.Created, path) {
		return nil
	}

	j.Created = append(j.Created, path)

	return j.save(stateDir)
}

// noteRemoval records the files this run is about to delete.
func (j *journal) noteRemoval(stateDir string, paths []string) error {
	changed := false

	for _, p := range paths {
		if !slices.Contains(j.RemovedFiles, p) {
			j.RemovedFiles = append(j.RemovedFiles, p)
			changed = true
		}
	}

	if !changed {
		return nil
	}

	return j.save(stateDir)
}

// save rewrites the journal durably.
//
// REPLACED RATHER THAN APPENDED, and written through a temporary so a crash
// mid-write leaves the previous list rather than a truncated one — the list is
// what an abandon deletes from, and a half-written one is a list that has lost
// entries.
func (j *journal) save(stateDir string) error {
	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("deployarchive: encode the restore journal: %w", err)
	}

	body = append(body, '\n')

	path := filepath.Join(stateDir, journalFile)

	// A RANDOM TEMPORARY NAME, NOT `<journal>.new`, AND NOTHING IS UNLINKED BY
	// PATHNAME. The predictable name had to be cleared before each write, and an
	// unconditional `os.Remove` of a path this package does not own is exactly
	// what the App-key rules forbid: `github.private_key_path` is an operator's
	// string, and nothing stops it naming that file. CreateTemp uses O_EXCL with
	// a name nothing can predict, so there is no collision to clear and no delete
	// to get wrong.
	f, err := os.CreateTemp(stateDir, ".billet-journal-*")
	if err != nil {
		return fmt.Errorf("deployarchive: stage the restore journal in %s: %w", stateDir, err)
	}

	tmp := f.Name()

	// Best effort on every failure path: a temporary left behind is litter, and
	// this one is billet's own file with a name nothing else can hold.
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	// EXPLICIT, because CreateTemp's 0600 is the umask's to reduce and this file
	// records the paths an abandon may delete.
	if err := f.Chmod(entryMode); err != nil {
		return fmt.Errorf("deployarchive: set the mode on %s: %w", tmp, err)
	}

	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("deployarchive: write %s: %w", tmp, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("deployarchive: flush %s: %w", tmp, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("deployarchive: close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("deployarchive: install %s: %w", path, err)
	}

	return syncDir(stateDir)
}

func (j *journal) remove(stateDir string) error {
	path := filepath.Join(stateDir, journalFile)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deployarchive: remove %s: %w", path, err)
	}

	return syncDir(stateDir)
}

// manifestDigest identifies an archive by the bytes of its manifest.
//
// Read once, by Open, from the bytes it validated. See Archive.manifestSHA.
func (a *Archive) manifestDigest() string { return a.manifestSHA }

// Name is what this archive is called wherever it is stored beside others.
//
// THE INSTANT IT WAS TAKEN, AND A DIGEST, because the instant alone is not
// unique. CreatedAt is RFC 3339 to the SECOND, so two backups of one deployment
// taken within the same second — a script running them back to back — produce
// the same name, and the second one's upload is then refused by the no-clobber
// write it collides with. The digest makes the name unique while keeping it
// STABLE for one archive, which is what lets a resumed upload and a resumed
// recovery both recompute the name they already used.
//
// Short, because it is a filename and a key an operator types: eight hex
// characters over a manifest that already carries every entry's digest.
func (a *Archive) Name() string {
	name := strings.ReplaceAll(a.Manifest.CreatedAt, ":", "-")

	if sha := a.manifestSHA; len(sha) >= manifestNameDigits {
		return name + "-" + sha[:manifestNameDigits]
	}

	return name
}

// manifestNameDigits is how much of the manifest digest goes into a name.
const manifestNameDigits = 8

// AbandonResult is what an abandon did.
type AbandonResult struct {
	Removed []string
	// Kept names a path the journal recorded but that no longer holds what this
	// restore put there, so it was left alone.
	Kept []string
	// Restored names a ledger a recover had moved aside and this put back. It is
	// the operator's own capacity record, so an abandon that did not return it
	// would leave the deployment with none.
	Restored []string
}

// Abandon undoes an interrupted restore, and only what that restore created.
//
// THE JOURNAL IS THE WHOLE AUTHORITY FOR WHAT MAY BE DELETED. Nothing is removed
// because it looks like it came from the archive, because it is in the way, or
// because the directory ought to be empty — only paths this run recorded before
// creating them.
//
// AND EACH ONE IS RE-READ FIRST. A path that no longer holds the bytes the
// archive carries is somebody else's file now, and is kept and named. That
// matters most for the App key: GitHub issues it once, so the only key an
// abandon may delete is one it can prove is a duplicate of the copy still
// sitting in the archive.
func Abandon(ctx context.Context, a *Archive, t Target, intent Intent) (AbandonResult, error) {
	if err := intent.valid(); err != nil {
		return AbandonResult{}, err
	}

	if a == nil {
		return AbandonResult{}, errors.New(
			"deployarchive: abandoning a restore needs the backup it was from, so billet can " +
				"prove each file it removes is a copy of one the backup still holds")
	}

	lock, err := state.LockStateDir(t.StateDir)
	if err != nil {
		return AbandonResult{}, err
	}

	// The authority lock for the same reason Execute takes it, and in the same
	// order: this REMOVES authority files, and a concurrent rotation reading them
	// would see half a generation.
	authority, err := wirecert.LockAuthority(t.StateDir)
	if err != nil {
		return AbandonResult{}, errors.Join(err, lock.Release())
	}

	res, runErr := abandonLocked(ctx, a, t, intent)

	return res, errors.Join(runErr, authority.Release(), lock.Release())
}

func abandonLocked(ctx context.Context, a *Archive, t Target,
	intent Intent,
) (AbandonResult, error) {
	// THE FENCE THIS OPERATION'S OWN COMMAND WROTE. WriteMaintenanceFence refuses
	// to replace a fence carrying another reason — which is the rule that stops a
	// restore reopening a ledger an upgrade closed — so an abandon that assumed
	// the restore reason could not undo a recovery at all.
	reason := fenceReasonFor(intent)
	path := filepath.Join(t.StateDir, journalFile)

	// THE JOURNAL IS READ AND OWNERSHIP ESTABLISHED BEFORE ANY FENCE IS WRITTEN,
	// and that ORDER is the whole of this paragraph. Fencing first and refusing
	// afterwards leaves the WRONG operation's fence standing over somebody else's
	// record — priorJournal makes it permanent, deliberately, because a fence
	// raised over a published journal must not come down on a refusal — and the
	// command that owns the journal then cannot fence the directory at all,
	// because WriteMaintenanceFence will not replace another reason. Two commands,
	// neither able to proceed, produced by the check that exists to send an
	// operator to the right one.
	//
	// NOTHING HERE NEEDS THE FENCE. The journal is a plain file, this call holds
	// the directory lock and the authority lock, and no billet writes a journal
	// without them.
	//
	// AND A REFUSAL LEAVES THE DIRECTORY EXACTLY AS IT FOUND IT, which is the
	// property to keep rather than an omission. Every state billet itself can
	// produce that has a journal in it also has a FENCE: Execute writes the fence
	// first and leaves it standing on every failure, and the one window where a
	// journal outlives its fence is the step inside Finish — after requireSealed,
	// so the deployment there is complete and sealed and a control plane starting
	// on it admits nothing. Raising a fence on the way to refusing is what the
	// previous shape did, and it deadlocked both commands. Raising a THIRD reason
	// nobody owns would be the same trap with an extra step: a fence's only exit
	// is the operation that wrote it, so one written by a refusal has no exit at
	// all.
	raw, journalErr := os.ReadFile(path)

	var j journal

	switch {
	case errors.Is(journalErr, os.ErrNotExist):
	case journalErr != nil:
		return AbandonResult{}, fmt.Errorf("deployarchive: read %s: %w", path, journalErr)
	default:
		if err := json.Unmarshal(raw, &j); err != nil {
			return AbandonResult{}, fmt.Errorf(
				"deployarchive: %s does not parse (%w); it lists what this would delete, so "+
					"billet will not guess at it", path, err)
		}

		if err := j.understood(t.StateDir); err != nil {
			return AbandonResult{}, err
		}

		if sha := a.manifestDigest(); j.ManifestSHA != sha {
			return AbandonResult{}, fmt.Errorf(
				"deployarchive: %s records a restore from %s, and --from names a different "+
					"backup. Point it at the one that was interrupted so billet can prove what "+
					"it removes", path, j.ArchiveDir)
		}

		// AND IT MUST BE THIS OPERATION'S. understood proves the journal names AN
		// operation billet has; it does not prove it names THIS one, and the two
		// abandons do different things — a restore's never reaches the ledger
		// put-back or the proof that guards it. The fence reason usually
		// separates them and cannot be relied on to: Finish takes the fence down
		// one step before removing the journal, so a journal that outlives its
		// fence would let a restore's abandon act on a recovery's record and skip
		// every check that belongs to it.
		if j.Intent != intent.String() {
			return AbandonResult{}, fmt.Errorf(
				"deployarchive: %s was written by a %s and this is `billet local %s --abandon`. "+
					"Run `billet local %s --abandon --from %s`, which is the command that owns "+
					"it", path, j.Intent, intent, j.Intent, j.ArchiveDir)
		}

		// A FINISHED OPERATION IS NOT AN INTERRUPTED ONE, and undoing it would be
		// the worst thing this command can do: every file is in place, the
		// deployment is sealed, and the journal is here only because the fence
		// came down first and its removal did not. An abandon on that state
		// removes the restored credentials and puts the ledger the operator chose
		// to replace back.
		if j.phase() == phaseFinished {
			return AbandonResult{}, fmt.Errorf(
				"deployarchive: %s records an operation that FINISHED — every file is published "+
					"and this directory is no longer fenced — so there is nothing to abandon. "+
					"Re-run the command without --abandon to clear the record it left behind",
				path)
		}
	}

	// THE FENCE STAYS UP UNTIL THE END, for the same reason it does during a
	// restore: this directory is incomplete while files are being removed from
	// it, and a control plane starting on it would mint whatever is missing. A
	// journal already here means an earlier run published something, and a fence
	// raised over that must not come down on a refusal.
	priorJournal := j.Schema != 0

	fenced, err := state.WriteMaintenanceFence(t.StateDir, reason)
	if err != nil {
		return AbandonResult{}, err
	}

	undoFence := func(cause error) (AbandonResult, error) {
		if !fenced || priorJournal {
			return AbandonResult{}, cause
		}

		return AbandonResult{}, errors.Join(cause,
			state.ClearMaintenanceFence(t.StateDir, reason))
	}

	if err := state.WriterBarrier(ctx, t.StateDir); err != nil {
		return undoFence(err)
	}

	if !priorJournal {
		// NOT AN ERROR, AND THE FENCE COMES DOWN. A directory with no journal has
		// no interrupted restore in it, and leaving it fenced would be this
		// command closing a ledger it found open.
		return AbandonResult{}, state.ClearMaintenanceFence(t.StateDir, reason)
	}

	var res AbandonResult

	// WHAT A RECOVER MOVED ASIDE IS NOT SOMETHING THIS RUN CREATED, whatever the
	// journal says. The journal records it so a crash can be reasoned about, and
	// the file at that name is the operator's OWN ledger — the only record of the
	// work the recovery failed. It is put back below, after the ledger this run
	// installed has been removed, and never deleted.
	moved := supersededPaths(a, t)

	// REVERSE ORDER, so the directory passes back through the same states it
	// came forward through: the marker goes before the authority it witnesses,
	// and the identity goes last.
	for i := len(j.Created) - 1; i >= 0; i-- {
		created := j.Created[i]

		if slices.Contains(moved, created) {
			continue
		}

		outcome, err := abandonOne(a, t, created)
		if err != nil {
			return res, err
		}

		switch outcome {
		case abandonKept:
			res.Kept = append(res.Kept, created)
		case abandonRemoved:
			res.Removed = append(res.Removed, created)
		case abandonNeverCreated:
			// The journal recorded it and the run died before creating it, which
			// is the direction that ordering is deliberately biased in. There is
			// nothing to remove and nothing to keep, and reporting it as removed
			// would name a file that never existed.
		}
	}

	if err := putSupersededBack(a, t, j.SupersededDigests, &res); err != nil {
		return res, err
	}

	// AND A RECOVERY'S ABANDON MUST LEAVE A LEDGER BEHIND. Everything above is
	// about not writing over one; this is the other half, and it is what the
	// fence coming down actually promises. A recover only ever runs against a
	// deployment that HAD a ledger, so an abandon that ends with none has either
	// removed the operator's or failed to put it back, and lifting the fence over
	// that hands a control plane a directory where it will mint a fresh, empty
	// one. A restore's abandon is exempt: it can legitimately leave a directory
	// with no ledger, because there was none before it ran.
	if intent == ReplaceLedger {
		if err := requireLedgerRestored(t.StateDir, j.SupersededDigests); err != nil {
			return res, err
		}
	}

	if err := (&j).remove(t.StateDir); err != nil {
		return res, err
	}

	return res, state.ClearMaintenanceFence(t.StateDir, reason)
}

// requireLedgerRestored refuses to open a deployment whose capacity record is
// not the one this operation moved aside.
//
// A REGULAR FILE IS NOT ENOUGH, AND NEITHER IS A PRESENT ONE. Both were tried
// and both are the same mistake at different depths: "something is there" is
// satisfied by a symlink, and "a regular file is there" is satisfied by an empty
// one somebody dropped in while the directory was fenced — after which the next
// control plane initialises it as a fresh, empty ledger and the deployment's own
// capacity record is gone, past a command whose whole job was putting it back.
// What proves it is the DIGEST the supersede recorded before it moved anything.
//
// AN EMPTY digest MEANS NOTHING WAS SUPERSEDED, which is a real state: a run
// that failed before supersedeLedger got that far never moved the operator's
// ledger, so it is still at its own name and untouched. There is nothing to
// compare it against and nothing to prove.
func requireLedgerRestored(stateDir string, digests map[string]string) error {
	if err := requireLedgerPresent(stateDir); err != nil {
		return err
	}

	if len(digests) == 0 {
		return nil
	}

	ledger := LedgerPath(stateDir)

	// THE SET MUST BE EXACTLY WHAT WAS MOVED ASIDE, both ways round. Checking
	// only the recorded files leaves the other direction open, and it is the one
	// that corrupts: a -wal that appears at a superseded name AFTER the digests
	// were taken has nothing to compare against, gets moved back because its
	// canonical name is free, and SQLite replays it into a ledger it was never a
	// journal for. So a file present here and absent from the record is refused
	// exactly as a mismatched one is.
	for _, path := range append([]string{ledger}, LedgerSidecarPaths(ledger)...) {
		want, recorded := digests[filepath.Base(path)]

		switch _, err := os.Lstat(path); {
		case errors.Is(err, os.ErrNotExist):
			if recorded {
				return unrestoredLedger(stateDir, path,
					"it was moved aside by this recovery and is not back")
			}

			continue
		case err != nil:
			return fmt.Errorf("deployarchive: inspect %s: %w", path, err)
		}

		if !recorded {
			return unrestoredLedger(stateDir, path,
				"this recovery never moved a file of that name aside, so billet cannot say "+
					"which database it belongs to")
		}

		found, _, err := digestFile(path)
		if err != nil {
			return fmt.Errorf("deployarchive: read %s to prove this abandon put the right "+
				"ledger back: %w", path, err)
		}

		if found != want {
			return unrestoredLedger(stateDir, path,
				"it is not the file this recovery moved aside")
		}
	}

	return nil
}

// unrestoredLedger is the refusal for a ledger set an abandon cannot vouch for.
func unrestoredLedger(stateDir, path, why string) error {
	return fmt.Errorf(
		"deployarchive: %s cannot be accounted for (%s), so billet will not lift the fence: a "+
			"control plane starting on the wrong ledger loses this deployment's capacity "+
			"record, and SQLite REPLAYS a write-ahead log it finds beside a database. %s is "+
			"still fenced and its journal is intact; look for the matching "+
			"billet.db.superseded-* files and reconcile them by hand. (A control-plane "+
			"connection that was open when this recovery started and closed during it "+
			"checkpoints the log into the database, which changes it after billet recorded what "+
			"it looked like — if that is what happened, what is here IS correct and only needs "+
			"the journal and the fence removed.)",
		path, why, stateDir)
}

// requireLedgerPresent refuses a ledger path that is absent or is not a file.
//
// A REGULAR FILE, NOT AN ENTRY WITH THE RIGHT NAME. A symlink or a directory at
// that path satisfies "something is there" and is not a ledger a control plane
// can open, so accepting either would let the fence come down over the same
// empty-deployment outcome this exists to refuse.
func requireLedgerPresent(stateDir string) error {
	switch info, err := os.Lstat(LedgerPath(stateDir)); {
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf(
			"deployarchive: %s is not a regular file (%s), so billet will not lift the fence "+
				"over it: a ledger is a file a control plane opens, not a link to one. %s is "+
				"still fenced and its journal is intact",
			LedgerPath(stateDir), info.Mode().Type(), stateDir)
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf(
			"deployarchive: this abandon left no ledger at %s, so billet will not lift the "+
				"fence: a control plane starting there would mint a fresh, empty one and this "+
				"deployment's capacity record would be gone. %s is still fenced and its journal "+
				"is intact; look for a billet.db.superseded-* file beside it",
			LedgerPath(stateDir), stateDir)
	default:
		return fmt.Errorf("deployarchive: inspect %s: %w", LedgerPath(stateDir), err)
	}
}

// supersededPaths are what a recover moved this deployment's ledger to.
//
// RECOMPUTED RATHER THAN READ BACK, because the name is derived from the
// ARCHIVE's own timestamp: every run of the same recovery computes the same one,
// which is what makes a resume recognise the rename it already did instead of
// moving a second ledger aside under a second name.
func supersededPaths(a *Archive, t Target) []string {
	moved := LedgerPath(t.StateDir) + supersededSuffix(a)

	return append([]string{moved}, LedgerSidecarPaths(moved)...)
}

// putSupersededBack undoes a recover's rename.
//
// WITHOUT THIS AN ABANDONED RECOVER LEAVES NO LEDGER AT ALL: the loop above
// removes the one this run installed, having proved it is the archive's copy,
// and the operator's own is sitting under a name nothing looks at — in a
// directory whose fence is about to come down.
//
// A LEDGER AND ITS SIDECARS ARE NOT INDEPENDENT FILES, so this moves the set as
// one thing or not at all. Deciding each one separately meant that a ledger the
// abandon could not remove — one that is no longer this run's, so it is
// correctly KEPT — could still have the superseded -wal renamed alongside it,
// because that name happened to be free. SQLite replays a write-ahead log it
// finds beside a database, and that one belongs to a different database:
// corruption, produced by the command whose job is undoing damage.
//
// THE LEDGER MOVES FIRST HERE, WHICH IS THE OPPOSITE OF supersedeLedger, AND
// BOTH ORDERS ARE CHOSEN BY WHAT A RESUMED RUN LOOKS AT. Going forward the
// ledger's presence at the SOURCE is what makes a re-plan come back to
// supersedeLedger, so it moves last. Coming back the ledger's presence at the
// CANONICAL name is what says the set is home, so it moves first — and with the
// fence held, that is a durable progress marker a retry reads correctly. The
// other order was tried and is exactly wrong: a crash after one sidecar had been
// put back left a canonical -wal beside a superseded ledger, which the
// foreign-log guard then read as a collision, refused, and — because a refusal
// was a silent "keep" — let the abandon lift the fence over a directory with no
// billet.db in it at all.
//
// AND A STATE THIS CANNOT RESOLVE IS AN ERROR, not a quiet nil. What follows a
// successful return is the journal being removed and the fence coming down, so
// "I decided not to move anything" and "this directory is ready to start" must
// never be the same answer.
//
// THE DESTINATIONS ARE ALL CHECKED BEFORE ANYTHING MOVES. A -wal sitting at the
// canonical name while the ledger is still superseded is a journal for the
// database this abandon has just removed, and attaching the operator's ledger to
// it is the corruption above by a different route.
//
// THAT CHECK IS A SNAPSHOT AND os.Rename REPLACES, so what bounds it is stated
// rather than assumed. The LEDGER's destination cannot appear between the two:
// billet.db is created by sql.Open inside openDir, and openDir consults the
// maintenance fence BEFORE it opens anything, so no billet can create one while
// this fence stands — and the one opener that crosses a fence takes the
// directory lock this call is holding. What CAN appear is a -wal or a -shm from
// a reader whose connection predates the fence, and replacing one of those with
// the file this deployment's own ledger was using is the correct outcome rather
// than a loss. A no-replace rename would close even that, and it is renameat2 on
// Linux and renamex_np on macOS — a build-tag surface and a syscall pair for a
// window whose only reachable content is a derived file. If this is ever
// revisited, that is the fix, and the crash state it introduces (both names
// linked to one inode, if done through link/unlink) is the thing to get right.
func putSupersededBack(a *Archive, t Target, digests map[string]string,
	res *AbandonResult,
) error {
	ledger := LedgerPath(t.StateDir)
	back := append([]string{ledger}, LedgerSidecarPaths(ledger)...)
	moved := supersededPaths(a, t)

	movedThere, err := whichExist(moved)
	if err != nil {
		return err
	}

	backThere, err := whichExist(back)
	if err != nil {
		return err
	}

	if !slices.Contains(movedThere, true) {
		// Nothing was superseded, or the whole set has already been put back.
		return nil
	}

	// EVERY FILE ABOUT TO MOVE IS PROVED FIRST, and that is deliberately BEFORE
	// the rename rather than after it. Checking afterwards leaves the window
	// where an unrecorded file has already been put beside the ledger; and an
	// empty record with a superseded file present is not "nothing was
	// superseded", it is billet being unable to say what that file is — which is
	// the presence-as-proof mistake this whole area keeps producing, arriving
	// through the function that does the moving.
	for i, there := range movedThere {
		if !there {
			continue
		}

		want, recorded := digests[filepath.Base(back[i])]
		if !recorded {
			return unrestoredLedger(t.StateDir, moved[i],
				"this recovery never recorded moving a file of that name aside, so billet "+
					"cannot say which database it belongs to")
		}

		found, _, err := digestFile(moved[i])
		if err != nil {
			return fmt.Errorf("deployarchive: read %s to prove it is what this recovery moved "+
				"aside: %w", moved[i], err)
		}

		if found != want {
			return unrestoredLedger(t.StateDir, moved[i],
				"it is not the file this recovery moved aside")
		}
	}

	switch {
	case movedThere[0] && !backThere[0]:
		// The supersede completed and nothing has been put back yet, so ANY file
		// at a canonical sidecar name belongs to the database this abandon just
		// removed.
		for i := 1; i < len(back); i++ {
			if backThere[i] {
				return unresolvedSupersede(t.StateDir, back[i], moved)
			}
		}
	case !movedThere[0] && backThere[0]:
		// Either the supersede stopped before the ledger moved, or this put-back
		// has already moved it home. In both the ledger at the canonical name is
		// the operator's own and a sidecar beside it is its own; only a superseded
		// copy of the SAME sidecar is a state billet cannot explain.
		for i := 1; i < len(back); i++ {
			if movedThere[i] && backThere[i] {
				return unresolvedSupersede(t.StateDir, back[i], moved)
			}
		}
	default:
		// Two ledgers, or none with sidecars stranded.
		return unresolvedSupersede(t.StateDir, ledger, moved)
	}

	// THE LEDGER FIRST. See above: it is the marker a resumed run reads.
	if movedThere[0] {
		if err := os.Rename(moved[0], back[0]); err != nil {
			return fmt.Errorf("deployarchive: put %s back: %w", moved[0], err)
		}

		res.Restored = append(res.Restored, back[0])

		if err := syncDir(t.StateDir); err != nil {
			return err
		}
	}

	for i := 1; i < len(moved); i++ {
		if !movedThere[i] || backThere[i] {
			continue
		}

		if err := os.Rename(moved[i], back[i]); err != nil {
			return fmt.Errorf("deployarchive: put %s back: %w", moved[i], err)
		}

		res.Restored = append(res.Restored, back[i])
	}

	return syncDir(t.StateDir)
}

// unresolvedSupersede is the refusal for a ledger set billet cannot reassemble.
//
// IT NAMES BOTH SIDES, because the operator is the only one who can decide: the
// files are all still there, the journal is still there, and the fence is still
// up, so nothing can start on the directory while they look.
func unresolvedSupersede(stateDir, blocking string, moved []string) error {
	return fmt.Errorf(
		"deployarchive: the ledger this recovery moved aside (%s) cannot be put back while %s "+
			"is there, because billet cannot tell which database that file belongs to and "+
			"SQLite replays a write-ahead log it finds beside one. NOTHING WAS MOVED, %s is "+
			"still fenced and its journal is intact. Decide which of those is the database you "+
			"want, move the other one aside by hand, and run this again",
		strings.Join(moved, ", "), blocking, stateDir)
}

// whichExist reports, for each path, whether something is there.
//
// LSTAT RATHER THAN STAT, and an error that is not "no such file" is returned
// rather than folded into absence: a path billet cannot look at is not a path it
// may rename over.
func whichExist(paths []string) ([]bool, error) {
	there := make([]bool, len(paths))

	for i, p := range paths {
		switch _, err := os.Lstat(p); {
		case err == nil:
			there[i] = true
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, fmt.Errorf("deployarchive: inspect %s: %w", p, err)
		}
	}

	return there, nil
}

// abandonOutcome is what happened to one recorded path.
//
// THREE-VALUED, because "removed" and "there was nothing there" are different
// facts and the command PRINTS them. Collapsing them made an abandon report that
// it had removed a file that was recorded and never created.
type abandonOutcome int

const (
	// abandonRemoved: the file was this run's and is gone.
	abandonRemoved abandonOutcome = iota
	// abandonKept: something is there and billet cannot prove it is a copy of
	// what the archive holds, so it was left exactly as it is.
	abandonKept
	// abandonNeverCreated: the journal recorded the path and the run died before
	// creating it.
	abandonNeverCreated
)

// abandonOne removes one recorded path, or keeps it and says so.
//
// NOTHING IS REMOVED THAT BILLET CANNOT PROVE IS A DUPLICATE of a file the
// archive still holds. That matters most for the App key: GitHub issues it once,
// so the only key an abandon may delete is one whose bytes are still sitting in
// the backup it came from.
func abandonOne(a *Archive, t Target, path string) (abandonOutcome, error) {
	entry, ok := entryForTargetPath(a, t, path)
	if !ok {
		// A path the journal recorded that this archive has no entry for. Not
		// removed: billet cannot prove what it is.
		return abandonKept, nil
	}

	info, err := os.Lstat(path)
	if err != nil {
		// ABSENCE IS AN OUTCOME HERE, NOT A FAILURE. The journal records a path
		// before creating it, so a run that died between the two leaves an entry
		// with no file — which is the direction that ordering is deliberately
		// biased in, and has to be survivable rather than reported as an error.
		if errors.Is(err, os.ErrNotExist) {
			return abandonNeverCreated, nil
		}

		return abandonKept, fmt.Errorf("deployarchive: inspect %s: %w", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return abandonKept, nil
	}

	if entry == EntryLedger {
		// The ledger is compared by digest rather than read into memory.
		sum, _, err := digestFile(path)
		if err != nil {
			return abandonKept, err
		}

		rec, found := a.Manifest.Record(EntryLedger)
		if !found || sum != rec.SHA256 {
			return abandonKept, nil
		}
	} else {
		have, err := readSmall(path)
		if err != nil {
			return abandonKept, nil //nolint:nilerr // unreadable is "keep it", not a failure of the abandon
		}

		want, _ := a.Entry(entry)
		if !bytes.Equal(have, want) {
			return abandonKept, nil
		}
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return abandonKept, fmt.Errorf("deployarchive: remove %s: %w", path, err)
	}

	if err := syncDir(filepath.Dir(path)); err != nil {
		return abandonRemoved, err
	}

	return abandonRemoved, nil
}

// entryForTargetPath maps a published path back to the archive entry it came
// from.
func entryForTargetPath(a *Archive, t Target, path string) (string, bool) {
	if path == t.AppKeyPath {
		return EntryAppKey, true
	}

	if path == filepath.Join(t.StateDir, "deployment-id") {
		return EntryIdentity, true
	}

	if path == filepath.Join(t.StateDir, "billet.db") {
		return EntryLedger, true
	}

	for _, name := range a.AuthorityNames() {
		if path == authorityTargetPath(t.StateDir, name) {
			return AuthorityEntry(name), true
		}
	}

	return "", false
}

// JournalPath is where an interrupted restore records itself, so a command can
// name it.
func JournalPath(stateDir string) string { return filepath.Join(stateDir, journalFile) }

// InProgress reports whether a state directory holds an interrupted restore,
// and which archive it was from.
func InProgress(stateDir string) (Progress, error) {
	raw, err := os.ReadFile(JournalPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Progress{}, nil
		}

		return Progress{}, fmt.Errorf("deployarchive: read %s: %w", JournalPath(stateDir), err)
	}

	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return Progress{Present: true}, fmt.Errorf("deployarchive: %s does not parse: %w",
			JournalPath(stateDir), err)
	}

	// A RECORD THIS BUILD DOES NOT UNDERSTAND IS PRESENT AND UNUSABLE, never
	// absent and never "still publishing". What this answer decides is whether a
	// recovery skips sealing, proving quiescence and publishing altogether, so
	// reading a newer binary's phase as one of ours is how a downgrade finishes an
	// operation it cannot see the shape of.
	if err := j.understood(stateDir); err != nil {
		return Progress{Present: true}, err
	}

	return Progress{
		Present:     true,
		ArchiveDir:  strings.TrimSpace(j.ArchiveDir),
		ManifestSHA: strings.TrimSpace(j.ManifestSHA),
		Intent:      j.Intent,
		Phase:       j.phase(),
	}, nil
}

// Progress is what a state directory's journal says about an operation that
// stopped part-way.
//
// IT ANSWERS BY MANIFEST RATHER THAN BY PATHNAME. An archive fetched from a
// bucket lands under a directory whose name a second run does not have to pick
// the same way, and the question a resume is asking is "is this the same
// backup", which the manifest digest answers and a pathname does not.
type Progress struct {
	// Present says a journal is there. A journal billet could not read counts as
	// present and comes back with an error: "could not tell" is never "there is
	// nothing here".
	Present bool
	// ArchiveDir is where the interrupted run read its archive from, which is
	// what a diagnostic names.
	ArchiveDir string
	// ManifestSHA identifies that archive.
	ManifestSHA string
	// Intent is which operation wrote it, in Intent.String()'s words. Recorded
	// rather than inferred from the fence, which can be gone while the journal
	// is still here.
	Intent string
	// Phase is how far it got. See phasePublishing and its neighbours.
	Phase string
}

// Finished reports whether the operation completed and only its record remains.
func (p Progress) Finished() bool { return p.Phase == phaseFinished }

// IsFrom reports whether this journal was written by the given operation.
func (p Progress) IsFrom(intent Intent) bool { return p.Present && p.Intent == intent.String() }

// Published reports whether the interrupted run had finished putting files down.
func (p Progress) Published() bool {
	return p.Phase == phasePublished || p.Phase == phaseFinished
}

// IsFor reports whether this journal belongs to the given archive.
func (p Progress) IsFor(a *Archive) bool {
	return p.Present && a != nil && p.ManifestSHA == a.manifestDigest()
}

// AbandonFinishesIt reports whether `<that operation> --abandon` would actually
// undo something here, which is what a diagnostic may name.
//
// THREE FACTS AND ALL OF THEM NARROW THE ANSWER. An abandon acts only on a
// journal it understands (so an absent or unreadable one is not it), only on one
// its OWN operation wrote (a restore's abandon clears a restore's fence and
// never reaches a recovery's put-back), and never on one that FINISHED. Naming
// the command without asking hands an operator something that reports success
// and moves nothing.
func (p Progress) AbandonFinishesIt(intent Intent) bool {
	return p.IsFrom(intent) && !p.Finished()
}

// phase is how far this journal got, treating a journal that names no phase as
// one still publishing — the safe reading, since it makes a re-run resume the
// publication rather than skip it.
func (j *journal) phase() string {
	if j.Phase == "" {
		return phasePublishing
	}

	return j.Phase
}

// setPhase records how far this operation has got, durably.
func (j *journal) setPhase(stateDir, phase string) error {
	if j.phase() == phase {
		return nil
	}

	j.Phase = phase

	return j.save(stateDir)
}
