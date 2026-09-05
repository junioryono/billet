package hostupgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Host is everything this package does to a machine.
//
// AN INTERFACE BECAUSE THE ORDER IS THE THING WORTH TESTING. Every one of these
// operations is a real, irreversible act on a real host, and none of them can be
// exercised in a unit test — but the SEQUENCE is where every defect in this area
// lives, and a fake makes the sequence assertable. The real implementation
// composes what already exists: lifeops for the services, internal/state for the
// fence and the snapshot, releasesource for the candidate.
//
// NOTHING HERE RETURNS PARTIAL SUCCESS. Each method either did its whole job or
// returns an error, because a recovery that has to reason about half-completed
// steps is a recovery nobody can write correctly.
type Host interface {
	// StopNode stops the node so its compute drains. UNBOUNDED: a drain waits for
	// the work already running for as long as it runs, and no elapsed time here
	// may end a job.
	StopNode(ctx context.Context) error

	// StopServer stops the control plane, after custody has settled.
	StopServer(ctx context.Context) error

	// PreserveCurrent copies the installed binary, units and configuration into
	// the recovery directory, so a rollback restores what was actually there
	// rather than what a package would reinstall.
	PreserveCurrent(ctx context.Context, dir string) error

	// HideBinary removes the installed executable so a concurrent operator
	// cannot enter through either version while the swap is underway.
	HideBinary(ctx context.Context) error

	// PrepareImages puts a guest generation the candidate's contract accepts in
	// the cluster for every image this host's microVM tiers boot, or does nothing
	// on a host that boots none. A host whose new binary refuses every image it
	// holds launches nothing until somebody pulls, which is not an upgraded host.
	PrepareImages(ctx context.Context) error

	// Fence makes already-open operator handles refuse transactions, and waits
	// for any transaction that began before it to finish.
	Fence(ctx context.Context, reason string) error

	// ClearFence reopens the ledger to ordinary callers.
	ClearFence(ctx context.Context, reason string) error

	// SnapshotLedger writes a complete, verified copy of the ledger.
	SnapshotLedger(ctx context.Context, dest string) error

	// RestoreLedger puts a snapshot back. Only ever called before the commit
	// record exists.
	RestoreLedger(ctx context.Context, from string) error

	// InstallCandidate puts the staged binary in place.
	InstallCandidate(ctx context.Context) error

	// RecordInstalled writes which release manifest produced the binary now in
	// place, bound to the bytes of that binary.
	//
	// WHAT A VERSION STRING CANNOT SAY. A node's registration names the version it
	// was BUILT as; two builds can share one and a moved tag makes them identical,
	// so a rollout comparing versions converges on evidence weaker than the
	// decision it is converging. This is the record that lets a host say which
	// bytes it is actually running.
	RecordInstalled(ctx context.Context, digest, version string) error

	// Migrate runs the candidate's migrations with it as the only ledger writer.
	Migrate(ctx context.Context) error

	// ProbeReady starts probe units that open what they inherited and announce
	// readiness, while polling nothing and accepting no workload.
	ProbeReady(ctx context.Context) error

	// StartServices installs the steady-state units and starts them.
	StartServices(ctx context.Context) error

	// RestorePreserved puts the preserved binary, units and configuration back.
	RestorePreserved(ctx context.Context, dir string) error

	// ProveStable reports whether the services stayed up long enough to believe.
	ProveStable(ctx context.Context) error
}

// Request is one machine's upgrade.
type Request struct {
	Journal *Journal
	Host    Host
	Log     *slog.Logger
}

// ErrRolledBack means the upgrade failed and the previous state was restored.
//
// A SENTINEL BECAUSE IT IS NOT THE SAME OUTCOME AS A FAILED ROLLBACK. This host
// is healthy on its old release and can be tried again; the other leaves it
// cordoned. A caller that could not tell them apart would either retry a machine
// nobody has looked at or cordon one that is fine.
var ErrRolledBack = errors.New("hostupgrade: the upgrade failed and the previous release was restored")

// ErrCordoned means the upgrade failed AND the rollback could not be proved.
//
// THE WORST OUTCOME, AND IT HAS TO BE ITS OWN. Nothing about this machine is
// known: it may be on either release, its ledger may be either schema, and its
// compute may or may not exist. The only safe act is to leave it alone with its
// recovery journal intact and tell a person.
var ErrCordoned = errors.New("hostupgrade: the upgrade failed and the rollback could not be proved")

// ErrUnsafeToRestore is what a step wraps when it failed in a way that leaves the
// candidate possibly still running, so that restoring the previous release and
// ledger under it would be worse than the failure.
//
// A ROLLBACK OVER A LIVE CANDIDATE IS THE ONE THING WORSE THAN A HUNG PROBE. The
// probe step found this: a candidate sent SIGKILL whose exit never arrived is one
// the kernel is holding, and a rollback that restored the ledger while that
// process still had it open would be corrupting the very thing the snapshot exists
// to protect. A step that returns this is cordoned with its journal intact; nothing
// is restored until a person has looked.
var ErrUnsafeToRestore = errors.New("hostupgrade: the candidate could not be proved gone")

// fenceReason is what the maintenance fence says while a transaction owns the
// ledger, and it is ONE string because the fence is cleared only under the
// reason it was written with. The commit and the rollback each used to name
// their own outcome here, so the ledger refused both as somebody else's fence,
// and every upgrade with a local ledger ended cordoned with its services stopped
// and the ledger fenced: found by the rollout rehearsal on 2026-09-05, the first
// time this code met a real fence rather than a fake that ignored the argument.
const fenceReason = "host upgrade"

// Run carries out one upgrade, or resumes one.
//
// THE ORDER IS NOT NEGOTIABLE and every step is placed by what would go wrong if
// it moved:
//
//   - Everything that can fail without consequence happens before anything stops.
//   - The node stops before the server, so compute drains while the control plane
//     is still there to record what happened to it.
//   - The binary is hidden before the ledger is touched, so a concurrent operator
//     command cannot enter through either version mid-swap.
//   - The snapshot is taken before the migration, because a migration is the one
//     step putting the old binary back cannot undo.
//   - Readiness is PROVED, not assumed from a process being alive.
//   - The commit record is written last, and after it nothing may restore.
//
// A RESUMED RUN SKIPS WHAT THE JOURNAL SAYS IS DONE. Each step is idempotent
// anyway, but skipping is what makes a resume cheap enough to be the ordinary
// recovery rather than something an operator avoids.
func Run(ctx context.Context, req Request) error {
	j := req.Journal

	log := req.Log
	if log == nil {
		log = slog.Default()
	}

	// A COMMITTED UPGRADE IS NEVER UNWOUND. The fence may already have opened and
	// admitted operator writes, so restoring the snapshot would discard work
	// committed against the new ledger. What a crash after the commit needs is the
	// startup retried, which is exactly what this does.
	if j.Step == StepCommitted {
		log.Info("resuming a committed upgrade; starting the services rather than "+
			"rolling anything back", "to", j.ToVersion)

		return finish(ctx, req, log)
	}

	// A ROLLED-BACK UPGRADE IS FINISHED, NOT REPORTED.
	//
	// The decision is recorded BEFORE the fence opens and the services come back,
	// so this state can mean either "the rollback completed" or "it was recorded
	// and the machine lost power one instruction later". The two are
	// indistinguishable from the journal and they must be, because the alternative
	// is a second durable step whose own crash window is the same problem again.
	//
	// So resuming here re-runs the finishing work, every part of which is
	// idempotent: clearing a fence that is gone, starting a service that is
	// running, and proving one that is already stable all cost nothing. A run that
	// finds nothing to do returns the same error it would have returned anyway.
	if j.Step == StepRolledBack {
		if err := finishRollback(ctx, req, log); err != nil {
			log.Error("this host was rolled back and could not be brought back up; it is "+
				"cordoned and its recovery journal is left in place",
				"dir", j.Dir, "error", err)

			return fmt.Errorf("%w: %s (finishing the rollback failed: %w)",
				ErrCordoned, j.Failure, err)
		}

		return fmt.Errorf("%w: %s", ErrRolledBack, j.Failure)
	}

	// AN INTERRUPTED ROLLBACK RESUMES AS A ROLLBACK, NOT AS AN UPGRADE.
	//
	// A failure is recorded on the journal BEFORE the restoring starts, and the
	// STEP still names the last forward step that completed — so a crash between
	// restoring the ledger or the binary and recording StepRolledBack leaves a
	// journal that says "migrated" about a machine that has been put back.
	//
	// Resuming that through `advance` is the worst outcome this file can produce:
	// every remaining step finds its work already done, the probe passes against
	// the RESTORED OLD binary, and the run records a committed upgrade to a release
	// the machine is not running and then releases its claim. Nothing afterwards
	// knows the record is false.
	//
	// Failure is written by beginRollback and nowhere else, which is why every way
	// into a rollback goes through it — a path that skipped it would be one whose
	// interruption is indistinguishable from an upgrade still in progress, and the
	// commit-record path WAS that path until a review round made this branch's
	// premise something the code guarantees rather than something it assumed.
	//
	// Re-entering costs nothing: every action in a rollback is a restore, and
	// restoring twice is restoring. It cannot discard operator writes either,
	// because those become possible only when the fence is cleared, which happens
	// after the decision is recorded — and reaching here means it was not.
	if j.Failure != "" {
		log.Warn("this upgrade was rolling back when it was interrupted; continuing the "+
			"rollback rather than the upgrade",
			"step", j.Step, "cause", j.Failure)

		return rollBack(ctx, req, log, errors.New(j.Failure))
	}

	if err := advance(ctx, req, log); err != nil {
		if errors.Is(err, ErrUnsafeToRestore) {
			return cordonWithoutRestoring(req, log, err)
		}

		log.Error("the upgrade failed; restoring the previous release",
			"from", j.FromVersion, "to", j.ToVersion, "step", j.Step, "error", err)

		return beginRollback(ctx, req, log, err)
	}

	if err := j.Advance(StepCommitted); err != nil {
		// THE COMMIT RECORD IS WHAT MAKES THE REST SAFE, so failing to write it is
		// not a success with a missing note. Rolling back is correct here: nothing
		// has opened the fence yet.
		return beginRollback(ctx, req, log,
			fmt.Errorf("the upgrade succeeded but its commit record could not be "+
				"written: %w", err))
	}

	log.Info("upgrade committed", "from", j.FromVersion, "to", j.ToVersion)

	return finish(ctx, req, log)
}

// advance walks the steps that have not been done.
func advance(ctx context.Context, req Request, log *slog.Logger) error {
	j, host := req.Journal, req.Host

	// AN EXTERNAL LEDGER HAS THREE STEPS FEWER, AND THEY ARE RECORDED AS
	// REACHED RATHER THAN LEFT OUT. billet copies no PostgreSQL database: the
	// fence is a file in the identity directory that reaches only local handles,
	// the snapshot is the one thing that cannot exist, and the migration happens
	// where the controller claim already puts it — when the candidate serves.
	// Advancing the journal through them keeps a resume reading the same record
	// the run wrote, and keeps Reached honest about what is behind the host.
	skipped := map[Step]bool{}
	if j.ExternalLedger() {
		skipped[StepFenced] = true
		skipped[StepSnapshotted] = true
		skipped[StepMigrated] = true
	}

	steps := []struct {
		step Step
		do   func(context.Context) error
		what string
	}{{
		// PRESERVED BEFORE ANYTHING STOPS, so a rollback restores what was actually
		// installed rather than what a package would reinstall — which is not the
		// same thing on a host anybody has ever edited a unit on.
		step: StepStopped,
		what: "stopping the services and hiding the installed binary",
		do: func(ctx context.Context) error {
			if err := host.PreserveCurrent(ctx, j.Dir); err != nil {
				return err
			}

			// THE NODE FIRST, so compute drains while the control plane is still
			// there to record what happened to it. Stopping the server first would
			// leave the node holding leases nothing is reading.
			if err := host.StopNode(ctx); err != nil {
				return err
			}

			if err := host.StopServer(ctx); err != nil {
				return err
			}

			return host.HideBinary(ctx)
		},
	}, {
		step: StepImaged,
		what: "pulling a guest generation the candidate accepts",
		do:   host.PrepareImages,
	}, {
		step: StepFenced,
		what: "fencing the ledger",
		do:   func(ctx context.Context) error { return host.Fence(ctx, fenceReason) },
	}, {
		step: StepSnapshotted,
		what: "snapshotting the ledger",
		do:   func(ctx context.Context) error { return host.SnapshotLedger(ctx, j.SnapshotPath()) },
	}, {
		step: StepInstalled,
		what: "installing the candidate",
		do:   host.InstallCandidate,
	}, {
		step: StepMigrated,
		what: "migrating the ledger with the candidate as its only writer",
		do:   host.Migrate,
	}, {
		step: StepProbed,
		what: "proving the candidate can open what it inherited",
		do:   host.ProbeReady,
	}}

	for _, s := range steps {
		if j.Reached(s.step) {
			log.Debug("already done", "step", s.step)

			continue
		}

		if skipped[s.step] {
			log.Info("upgrade step has nothing to do on an external ledger", "step", s.step)
		} else {
			log.Info("upgrade step", "step", s.step, "what", s.what)

			if err := s.do(ctx); err != nil {
				return fmt.Errorf("%s: %w", s.what, err)
			}
		}

		if err := j.Advance(s.step); err != nil {
			return fmt.Errorf("recording that %s completed: %w", s.what, err)
		}
	}

	return nil
}

// finish opens the fence and starts the real services.
//
// AFTER THE COMMIT RECORD, ALWAYS. Opening the fence admits operator writes
// against the new ledger, and once that has happened restoring the snapshot would
// discard them — so a crash anywhere in here retries the startup rather than the
// rollback, which is precisely what Run's committed branch does.
func finish(ctx context.Context, req Request, log *slog.Logger) error {
	j, host := req.Journal, req.Host

	// RECORDED HERE BECAUSE BOTH WAYS IN GO THROUGH HERE, and putting it beside the
	// commit did not. A crash between writing the commit record and writing this
	// one leaves a journal at StepCommitted, and Run resumes THAT by calling finish
	// directly — so the provenance write was skipped on exactly the machine that
	// had just been upgraded, which then reported no manifest until something
	// upgraded it again.
	//
	// BEFORE THE SERVICES START, which is the ordering that matters: the binary
	// this describes is already in place, and nothing has yet registered to report
	// it. Writing it after StartServices would let the first registration after an
	// upgrade name the release it replaced.
	//
	// A FAILURE HERE DOES NOT UNDO THE UPGRADE. The machine is committed and the
	// fence is about to open, so rolling back over a record that could not be
	// written would discard a good upgrade to fix a diagnostic. The host reports no
	// manifest, which is the answer every host gave before this existed.
	if err := host.RecordInstalled(ctx, j.TargetDigest, j.ToVersion); err != nil {
		log.Warn("this host could not record which release manifest produced it, so a "+
			"rollout cannot prove which bytes it is running; the upgrade itself is "+
			"committed and unaffected", "error", err)
	}

	// NO FENCE WAS RAISED ON AN EXTERNAL LEDGER, so none is cleared; the
	// candidate's own claim is what admits it.
	if !j.ExternalLedger() {
		if err := host.ClearFence(ctx, fenceReason); err != nil {
			return fmt.Errorf("hostupgrade: the upgrade committed but the ledger is still "+
				"fenced, so nothing can write to it: %w", err)
		}
	}

	if err := host.StartServices(ctx); err != nil {
		return fmt.Errorf("hostupgrade: the upgrade committed but the services did not "+
			"start: %w", err)
	}

	if err := host.ProveStable(ctx); err != nil {
		return fmt.Errorf("hostupgrade: the upgrade committed but the services did not "+
			"stay up: %w", err)
	}

	log.Info("upgraded", "from", j.FromVersion, "to", j.ToVersion)

	return nil
}

// beginRollback records that a rollback is starting and then starts it.
//
// EVERY WAY INTO A ROLLBACK GOES THROUGH HERE, and that is the point rather than
// tidiness. Run resumes an interrupted rollback by recognising a journal with a
// FAILURE recorded and no decision reached — so a path that entered rollBack
// without writing one is a path whose interruption is indistinguishable from an
// upgrade still in progress, and resuming it walks the remaining steps against a
// machine that has already been restored and records a committed upgrade to a
// release it is not running. The commit-record path was exactly that: it rolled
// back over a journal it had just failed to write, leaving nothing to say so.
//
// A FAILURE THAT CANNOT BE RECORDED IS ALREADY THE CORDONED CASE, because the
// next run would read a journal describing a state the machine has left — and
// this is the one situation where doing less is safer than restoring blind.
// cordonWithoutRestoring records the failure and stops, restoring nothing.
//
// THE JOURNAL KEEPS THE CAUSE AND THE STEP, so `--resume` knows where it was and
// an operator knows why nothing moved. A resume rolls back from here, which is the
// operator asserting the candidate is gone.
func beginRollback(ctx context.Context, req Request, log *slog.Logger, cause error) error {
	if failErr := req.Journal.Fail(cause); failErr != nil {
		return fmt.Errorf("%w: %w (and the journal could not record it: %w)",
			ErrCordoned, cause, failErr)
	}

	return rollBack(ctx, req, log, cause)
}

// cordonWithoutRestoring records the failure and stops, restoring nothing,
// because the candidate may still be running; the resume that follows is the
// operator asserting it is not.
func cordonWithoutRestoring(req Request, log *slog.Logger, cause error) error {
	j := req.Journal

	log.Error("the upgrade failed in a way that leaves the candidate possibly running; "+
		"nothing is restored and this host is cordoned with its recovery journal in place",
		"from", j.FromVersion, "to", j.ToVersion, "step", j.Step, "dir", j.Dir, "error", cause)

	if failErr := j.Fail(cause); failErr != nil {
		return fmt.Errorf("%w: %w (and the journal could not record it: %w)",
			ErrCordoned, cause, failErr)
	}

	return fmt.Errorf("%w: %w (nothing was restored, because the candidate may still be "+
		"running; look at it, then `billet host-upgrade --resume` rolls back)", ErrCordoned, cause)
}

// rollBack restores the previous release, and says which of the two failure
// outcomes this is.
//
// IT UNWINDS EXACTLY WHAT WAS REACHED. A ledger that was never snapshotted must
// not be restored from a file that does not exist, and a binary that was never
// hidden must not be "restored" over one that is already correct. The journal is
// what says which, and every branch here is keyed on it rather than on what the
// caller believes happened.
func rollBack(ctx context.Context, req Request, log *slog.Logger, cause error) error {
	j, host := req.Journal, req.Host

	cordon := func(step string, err error) error {
		log.Error("the rollback could not be completed; this host is cordoned and its "+
			"recovery journal is left in place",
			"step", step, "dir", j.Dir, "error", err, "cause", cause)

		return fmt.Errorf("%w: %w (rollback failed at %s: %w)", ErrCordoned, cause, step, err)
	}

	// THE LEDGER GOES BACK BEFORE THE BINARY DOES, because the old binary refuses
	// a schema it has never heard of. Restoring in the other order produces a
	// control plane that starts, refuses its own database, and is down with
	// nothing installed that can read it.
	// AND NOTHING IS RESTORED ON AN EXTERNAL LEDGER, because nothing was
	// snapshotted: the rollback boundary there is the candidate's start, and
	// what lies past it is the database's own backup.
	if j.Reached(StepSnapshotted) && !j.ExternalLedger() {
		if err := host.RestoreLedger(ctx, j.SnapshotPath()); err != nil {
			return cordon("restoring the ledger", err)
		}
	}

	if j.Reached(StepStopped) {
		if err := host.RestorePreserved(ctx, j.Dir); err != nil {
			return cordon("restoring the previous binary, units and configuration", err)
		}
	}

	// THE DECISION IS DURABLE BEFORE THE FENCE OPENS, and getting that backwards
	// was a P0 a review caught.
	//
	// Clearing the fence admits operator writes against the RESTORED ledger. If a
	// crash lands between the clear and this record, the next run reads a journal
	// that still says an upgrade is in progress — and `advance` finds every step
	// already reached, so it walks straight to StepCommitted and reports a
	// successful upgrade of a machine that is on its old binary. A crash one
	// instruction earlier is worse: recovery would restore the snapshot a second
	// time, discarding whatever was written after the fence opened.
	//
	// Recording first makes both harmless. A resumed run sees StepRolledBack and
	// finishes the rollback rather than starting anything.
	if err := j.Advance(StepRolledBack); err != nil {
		return cordon("recording the rollback decision", err)
	}

	if err := finishRollback(ctx, req, log); err != nil {
		return cordon(err.Error(), err)
	}

	log.Info("rolled back", "to", j.FromVersion, "cause", cause)

	return fmt.Errorf("%w: %w", ErrRolledBack, cause)
}

// finishRollback opens the fence and brings the restored services back.
//
// SEPARATE AND IDEMPOTENT, because it runs both from rollBack and from a resumed
// run that found StepRolledBack already recorded. Everything in it is safe to
// repeat: clearing a fence that is gone, starting a service that is running, and
// proving one that is already stable all cost nothing.
func finishRollback(ctx context.Context, req Request, log *slog.Logger) error {
	j, host := req.Journal, req.Host

	if j.Reached(StepFenced) && !j.ExternalLedger() {
		if err := host.ClearFence(ctx, fenceReason); err != nil {
			return fmt.Errorf("clearing the maintenance fence: %w", err)
		}
	}

	if j.Reached(StepStopped) {
		if err := host.StartServices(ctx); err != nil {
			return fmt.Errorf("starting the restored services: %w", err)
		}

		if err := host.ProveStable(ctx); err != nil {
			// THE OLD RELEASE WAS RESTORED AND DID NOT COME BACK, which is not a
			// rollback anybody may call successful — the machine is on a binary
			// nothing has proved works.
			return fmt.Errorf("proving the restored services stay up: %w", err)
		}
	}

	log.Info("the restored services are back", "version", j.FromVersion)

	return nil
}
