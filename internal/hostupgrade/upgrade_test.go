package hostupgrade

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeHost records what was done to a machine, in order, and can be told to fail
// one step.
//
// THE ORDER IS THE SUBJECT. None of these operations can be exercised for real in
// a unit test — they stop services, replace binaries and migrate databases — but
// every defect in this area is a step in the wrong place, and a recorder makes
// that assertable.
type fakeHost struct {
	did []string
	// failAt makes one operation fail, so a test can put the failure exactly
	// where it wants it.
	failAt string
	// failRollbackAt makes an operation fail only while unwinding.
	failRollbackAt string
	rollingBack    bool

	recordedDigest  string
	recordedVersion string
}

func (h *fakeHost) record(what string) error {
	h.did = append(h.did, what)

	if h.rollingBack && what == h.failRollbackAt {
		return errors.New("the " + what + " step failed while unwinding")
	}

	if !h.rollingBack && what == h.failAt {
		return errors.New("the " + what + " step failed")
	}

	return nil
}

func (h *fakeHost) StopNode(context.Context) error   { return h.record("stop-node") }
func (h *fakeHost) StopServer(context.Context) error { return h.record("stop-server") }
func (h *fakeHost) HideBinary(context.Context) error { return h.record("hide-binary") }
func (h *fakeHost) PrepareImages(context.Context) error {
	return h.record("prepare-images")
}
func (h *fakeHost) InstallCandidate(context.Context) error {
	return h.record("install-candidate")
}
func (h *fakeHost) Migrate(context.Context) error     { return h.record("migrate") }
func (h *fakeHost) ProbeReady(context.Context) error  { return h.record("probe") }
func (h *fakeHost) ProveStable(context.Context) error { return h.record("prove-stable") }

func (h *fakeHost) PreserveCurrent(_ context.Context, _ string) error {
	return h.record("preserve")
}

// recordedDigest and recordedVersion are what RecordInstalled was actually told.
//
// CAPTURED, BECAUSE "IT WAS CALLED" IS NOT THE PROPERTY. Discarding the arguments
// left the ordering tests passing against a production path that called
// RecordInstalled("", "") — which does not record a manifest at all, it REMOVES
// the record, so a correctly upgraded host would have reported nothing.
func (h *fakeHost) RecordInstalled(_ context.Context, digest, release string) error {
	h.recordedDigest, h.recordedVersion = digest, release

	return h.record("record-installed")
}

func (h *fakeHost) Fence(_ context.Context, _ string) error { return h.record("fence") }

func (h *fakeHost) ClearFence(_ context.Context, _ string) error {
	if h.rollingBack {
		return h.record("clear-fence-rollback")
	}

	return h.record("clear-fence")
}

func (h *fakeHost) SnapshotLedger(_ context.Context, _ string) error {
	return h.record("snapshot")
}

func (h *fakeHost) RestoreLedger(_ context.Context, _ string) error {
	return h.record("restore-ledger")
}

func (h *fakeHost) RestorePreserved(_ context.Context, _ string) error {
	return h.record("restore-preserved")
}

func (h *fakeHost) StartServices(context.Context) error {
	if h.rollingBack {
		return h.record("start-services-rollback")
	}

	return h.record("start-services")
}

func newJournal(t *testing.T) *Journal {
	t.Helper()

	j := &Journal{
		Dir:          t.TempDir(),
		FromVersion:  "v0.3.26",
		ToVersion:    "v0.4.0",
		TargetDigest: strings.Repeat("a", 64),
		Step:         StepStaged,
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	if err := j.Write(); err != nil {
		t.Fatalf("write the journal: %v", err)
	}

	return j
}

func quiet() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func run(t *testing.T, j *Journal, h *fakeHost) error {
	t.Helper()

	// The fake flips into rollback mode when Run decides to unwind, which it
	// signals by calling RestoreLedger or RestorePreserved first. Rather than
	// guessing, the test wraps the failure itself.
	return Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()})
}

// A HEALTHY UPGRADE DOES EVERYTHING IN THE ORDER THAT MAKES EACH STEP SAFE.
//
// Every position here is load-bearing, and the comment on Run says why each is
// where it is. This asserts the whole sequence rather than a property of it,
// because the failure mode is a step MOVING — which no property-shaped assertion
// about "did it snapshot" would notice.
func TestAHealthyUpgradeRunsInOrder(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{}

	if err := run(t, j, h); err != nil {
		t.Fatalf("a healthy upgrade failed: %v", err)
	}

	want := []string{
		// Nothing has stopped yet: the candidate was staged and verified before
		// this run began.
		"preserve",
		// The node first, so compute drains while the control plane can still
		// record what happened to it.
		"stop-node",
		"stop-server",
		// Then the binary goes, so no operator command can enter through either
		// version while the swap is underway.
		"hide-binary",
		// A GENERATION THE CANDIDATE ACCEPTS IS PULLED WHILE NOTHING RUNS AND
		// BEFORE THE FENCE: a pull records in the cluster, not the ledger, and a
		// host whose new binary refuses every image it holds is not upgraded.
		"prepare-images",
		"fence",
		// The snapshot precedes the migration, because a migration is the one step
		// putting the old binary back cannot undo.
		"snapshot",
		"install-candidate",
		"migrate",
		// Readiness is proved before anything is committed.
		"probe",
		// WHICH MANIFEST PRODUCED THIS BINARY IS RECORDED AFTER THE COMMIT AND
		// BEFORE THE SERVICES START, which is the only window where both halves are
		// true: the binary it describes is in place, and nothing has registered yet
		// to report it. Earlier would describe a machine a rollback could still take
		// back; later would let the first registration after an upgrade name the
		// release it replaced.
		"record-installed",
		// And the fence opens only after the commit record exists.
		"clear-fence",
		"start-services",
		"prove-stable",
	}

	if !slices.Equal(h.did, want) {
		t.Errorf("the upgrade ran\n  %v\nwant\n  %v", h.did, want)
	}

	// WHAT IT RECORDED, not merely that it recorded. See fakeHost.RecordInstalled.
	if h.recordedDigest != j.TargetDigest || h.recordedVersion != j.ToVersion {
		t.Errorf("the upgrade recorded %q/%q as its provenance, want %q/%q",
			h.recordedDigest, h.recordedVersion, j.TargetDigest, j.ToVersion)
	}

	if j.Step != StepCommitted {
		t.Errorf("the journal records %s, want committed", j.Step)
	}
}

// THE SNAPSHOT PRECEDES THE MIGRATION, asserted on its own because it is the one
// ordering that decides whether a rollback is possible at all.
func TestTheLedgerIsSnapshottedBeforeItIsMigrated(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{}

	if err := run(t, j, h); err != nil {
		t.Fatalf("a healthy upgrade failed: %v", err)
	}

	snapshot := slices.Index(h.did, "snapshot")
	migrate := slices.Index(h.did, "migrate")

	if snapshot < 0 || migrate < 0 || snapshot > migrate {
		t.Errorf("snapshot at %d and migrate at %d; the old binary refuses a schema it has "+
			"never heard of, so a migration with no snapshot behind it cannot be undone",
			snapshot, migrate)
	}
}

// AND THE FENCE OPENS ONLY AFTER THE COMMIT RECORD.
//
// Opening it admits operator writes against the new ledger, and once that has
// happened restoring the snapshot would discard them. This is the ordering that
// makes "after the commit, never restore" true rather than aspirational.
func TestTheFenceOpensOnlyAfterTheCommitRecord(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{}

	// EVERY CLEARING IS RECORDED, NOT THE LAST ONE.
	//
	// The first version of this test kept only the most recent observation, which
	// made it pass against a build that opened the fence BEFORE the commit and
	// again afterwards — the earlier, unsafe clearing being overwritten by the
	// later, harmless one. That is the shape of an assertion satisfied by
	// something other than the thing under test; the mutation that moved the
	// clearing earlier went green.
	var clearedAt []Step

	watching := &watchingHost{fakeHost: h, journal: j, onClearFence: func(s Step) {
		clearedAt = append(clearedAt, s)
	}}

	if err := Run(t.Context(), Request{Journal: j, Host: watching, Log: quiet()}); err != nil {
		t.Fatalf("a healthy upgrade failed: %v", err)
	}

	if len(clearedAt) == 0 {
		t.Fatal("the fence was never cleared, so nothing can write to the ledger")
	}

	for i, step := range clearedAt {
		if step != StepCommitted {
			t.Errorf("the fence was cleared (occurrence %d of %d) while the journal recorded "+
				"%s; it may only open once the commit record exists, because after that "+
				"nothing may restore", i+1, len(clearedAt), step)
		}
	}
}

type watchingHost struct {
	*fakeHost
	journal      *Journal
	onClearFence func(Step)
}

func (h *watchingHost) ClearFence(ctx context.Context, reason string) error {
	h.onClearFence(h.journal.Step)

	return h.fakeHost.ClearFence(ctx, reason)
}

// A FAILURE UNWINDS EXACTLY WHAT WAS REACHED, LEDGER FIRST.
//
// The old binary refuses a schema it has never heard of, so restoring the binary
// before the ledger produces a control plane that starts, refuses its own
// database, and is down with nothing installed that can read it.
func TestAFailedUpgradeRestoresTheLedgerBeforeTheBinary(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe"}

	err := runUnwinding(t, j, h)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed upgrade returned %v, want ErrRolledBack", err)
	}

	ledger := slices.Index(h.did, "restore-ledger")
	binary := slices.Index(h.did, "restore-preserved")

	if ledger < 0 || binary < 0 || ledger > binary {
		t.Fatalf("restore-ledger at %d and restore-preserved at %d; the ledger has to go "+
			"back first or the restored binary refuses its own database:\n  %v",
			ledger, binary, h.did)
	}

	if j.Step != StepRolledBack {
		t.Errorf("the journal records %s, want rolled_back", j.Step)
	}

	// AND THE RESTORED SERVICES ARE PROVED, because a rollback that put a binary
	// back and did not watch it come up has not restored anything.
	if !slices.Contains(h.did, "prove-stable") {
		t.Errorf("the rollback did not prove the restored services stay up:\n  %v", h.did)
	}
}

// A FAILURE BEFORE THE SNAPSHOT RESTORES NO LEDGER.
//
// There is nothing to restore from, and restoring from a file that does not exist
// is how a recovery turns a recoverable failure into a cordoned host.
func TestAFailureBeforeTheSnapshotDoesNotRestoreTheLedger(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "fence"}

	if err := runUnwinding(t, j, h); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed upgrade returned %v, want ErrRolledBack", err)
	}

	if slices.Contains(h.did, "restore-ledger") {
		t.Errorf("the rollback restored a ledger that was never snapshotted:\n  %v", h.did)
	}

	// The binary WAS hidden, so it does have to go back.
	if !slices.Contains(h.did, "restore-preserved") {
		t.Errorf("the rollback did not restore the binary it had hidden:\n  %v", h.did)
	}
}

// AN UNPROVABLE ROLLBACK IS ITS OWN OUTCOME AND LEAVES THE HOST CORDONED.
//
// Nothing about the machine is then known: it may be on either release, its
// ledger may be either schema, and its compute may or may not exist. Reporting it
// as a rollback would have a rollout return the host to service.
func TestAnUnprovableRollbackCordonsTheHost(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe", failRollbackAt: "restore-preserved"}

	err := runUnwinding(t, j, h)
	if !errors.Is(err, ErrCordoned) {
		t.Fatalf("an unprovable rollback returned %v, want ErrCordoned", err)
	}

	if errors.Is(err, ErrRolledBack) {
		t.Error("a cordoned host also reports itself rolled back; a rollout would return " +
			"it to service")
	}

	// THE JOURNAL IS LEFT AS IT WAS. The next run has to be able to see how far
	// this got, and overwriting it with a decision nothing proved is what would
	// stop that.
	if j.Step == StepRolledBack {
		t.Error("a cordoned host recorded a rollback decision it could not carry out")
	}
}

// AND A ROLLBACK WHOSE RESTORED SERVICES DO NOT COME BACK IS ALSO CORDONED,
// because the machine is then on a binary nothing has proved works.
func TestARollbackWhoseServicesDoNotComeBackIsCordoned(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe", failRollbackAt: "prove-stable"}

	if err := runUnwinding(t, j, h); !errors.Is(err, ErrCordoned) {
		t.Fatalf("a rollback whose services did not come back returned %v, want ErrCordoned",
			err)
	}
}

// A COMMITTED UPGRADE IS NEVER UNWOUND, and a crash after the commit retries the
// startup instead.
//
// The fence may already have opened and admitted operator writes, so restoring
// the snapshot would discard work committed against the new ledger.
func TestResumingACommittedUpgradeStartsRatherThanRollsBack(t *testing.T) {
	j := newJournal(t)

	if err := j.Advance(StepCommitted); err != nil {
		t.Fatalf("commit: %v", err)
	}

	h := &fakeHost{}

	if err := run(t, j, h); err != nil {
		t.Fatalf("resuming a committed upgrade failed: %v", err)
	}

	for _, unwound := range []string{"restore-ledger", "restore-preserved", "stop-node"} {
		if slices.Contains(h.did, unwound) {
			t.Errorf("resuming a committed upgrade ran %q:\n  %v", unwound, h.did)
		}
	}

	if !slices.Contains(h.did, "start-services") {
		t.Errorf("resuming a committed upgrade did not start the services:\n  %v", h.did)
	}
}

// A RESUMED RUN SKIPS WHAT THE JOURNAL SAYS IS DONE, so a recovery is the
// ordinary path rather than something an operator avoids because it redoes work.
func TestAResumedUpgradeSkipsCompletedSteps(t *testing.T) {
	j := newJournal(t)

	if err := j.Advance(StepSnapshotted); err != nil {
		t.Fatalf("advance: %v", err)
	}

	h := &fakeHost{}

	if err := run(t, j, h); err != nil {
		t.Fatalf("resuming failed: %v", err)
	}

	for _, done := range []string{"stop-node", "stop-server", "fence", "snapshot"} {
		if slices.Contains(h.did, done) {
			t.Errorf("a resumed upgrade repeated %q:\n  %v", done, h.did)
		}
	}

	for _, remaining := range []string{"install-candidate", "migrate", "probe"} {
		if !slices.Contains(h.did, remaining) {
			t.Errorf("a resumed upgrade skipped %q:\n  %v", remaining, h.did)
		}
	}
}

// A ROLLED-BACK UPGRADE IS FINISHED, NOT RESTARTED.
//
// The decision is recorded BEFORE the fence opens and the services come back, so
// this state means either "the rollback completed" or "it was recorded and the
// machine lost power one instruction later" — and the journal cannot tell them
// apart, deliberately, because a second durable step would have the same crash
// window one place along.
//
// AN EARLIER VERSION OF THIS TEST ASSERTED THE OPPOSITE and was wrong: it
// required a resumed run to touch nothing, which is exactly the behaviour that
// leaves a machine fenced and stopped after a crash between the record and the
// finish. It passed against a build that recorded the decision LAST, which is the
// ordering a review found to be a P0.
func TestResumingARolledBackUpgradeFinishesTheRollback(t *testing.T) {
	j := newJournal(t)
	j.Failure = "the candidate never became ready"

	if err := j.Advance(StepRolledBack); err != nil {
		t.Fatalf("advance: %v", err)
	}

	h := &fakeHost{rollingBack: true}

	err := run(t, j, h)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("resuming a rolled-back upgrade returned %v, want ErrRolledBack", err)
	}

	// IT BRINGS THE MACHINE BACK, which is what a crash between the record and the
	// finish leaves undone.
	for _, want := range []string{"clear-fence-rollback", "start-services-rollback"} {
		if !slices.Contains(h.did, want) {
			t.Errorf("resuming a rolled-back upgrade did not %q:\n  %v", want, h.did)
		}
	}

	// AND IT RESTARTS NOTHING. The machine is deliberately on its old release; a
	// resumed run that stopped services or installed the candidate again would be
	// re-running the upgrade that was already abandoned.
	for _, forbidden := range []string{"stop-node", "install-candidate", "migrate", "probe"} {
		if slices.Contains(h.did, forbidden) {
			t.Errorf("resuming a rolled-back upgrade ran %q:\n  %v", forbidden, h.did)
		}
	}
}

// AND THE DECISION IS RECORDED BEFORE THE FENCE OPENS.
//
// Clearing the fence admits operator writes against the restored ledger. A crash
// between the clear and the record leaves a journal that still says an upgrade is
// in progress — and `advance` then finds every step already reached and walks
// straight to committed, reporting a successful upgrade of a machine that is on
// its old binary.
func TestTheRollbackDecisionIsRecordedBeforeTheFenceOpens(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe"}

	var stepAtClear []Step

	watching := &rollbackWatcher{fakeHost: h, journal: j, onClearFence: func(s Step) {
		stepAtClear = append(stepAtClear, s)
	}}

	if err := Run(t.Context(), Request{Journal: j, Host: watching, Log: quiet()}); !errors.Is(
		err, ErrRolledBack) {
		t.Fatalf("Run: %v, want ErrRolledBack", err)
	}

	if len(stepAtClear) == 0 {
		t.Fatal("the fence was never cleared, so the restored ledger stays unwritable")
	}

	for i, step := range stepAtClear {
		if step != StepRolledBack {
			t.Errorf("the fence was cleared (occurrence %d of %d) while the journal recorded "+
				"%s; the rollback decision has to be durable first, or a crash here reports "+
				"a successful upgrade of a machine on its old binary",
				i+1, len(stepAtClear), step)
		}
	}
}

// rollbackWatcher reports the journal's step at each fence clearing during a
// rollback.
type rollbackWatcher struct {
	*fakeHost
	journal      *Journal
	onClearFence func(Step)
}

func (h *rollbackWatcher) RestoreLedger(ctx context.Context, from string) error {
	h.rollingBack = true

	return h.fakeHost.RestoreLedger(ctx, from)
}

func (h *rollbackWatcher) RestorePreserved(ctx context.Context, dir string) error {
	h.rollingBack = true

	return h.fakeHost.RestorePreserved(ctx, dir)
}

func (h *rollbackWatcher) ClearFence(ctx context.Context, reason string) error {
	h.onClearFence(h.journal.Step)

	return h.fakeHost.ClearFence(ctx, reason)
}

// runUnwinding runs an upgrade whose failure will trigger a rollback, flipping
// the fake into rollback mode at the moment the unwinding starts.
func runUnwinding(t *testing.T, j *Journal, h *fakeHost) error {
	t.Helper()

	return Run(t.Context(), Request{
		Journal: j,
		Host:    &flippingHost{fakeHost: h},
		Log:     quiet(),
	})
}

// flippingHost turns on the fake's rollback mode as soon as an unwinding
// operation is reached, so the recorded trace and the injected rollback failure
// both know which half of the run they are in.
type flippingHost struct{ *fakeHost }

func (h *flippingHost) RestoreLedger(ctx context.Context, from string) error {
	h.rollingBack = true

	return h.fakeHost.RestoreLedger(ctx, from)
}

func (h *flippingHost) RestorePreserved(ctx context.Context, dir string) error {
	h.rollingBack = true

	return h.fakeHost.RestorePreserved(ctx, dir)
}

func (h *flippingHost) ClearFence(ctx context.Context, reason string) error {
	if strings.Contains(reason, "rolled back") {
		h.rollingBack = true
	}

	return h.fakeHost.ClearFence(ctx, reason)
}

// AN UPGRADE INTERRUPTED WHILE ROLLING BACK CONTINUES ROLLING BACK.
//
// THIS IS THE WORST OUTCOME THIS FILE CAN PRODUCE, and the ordering that causes it
// reads as safe. A failure is recorded before the restoring starts, and the STEP
// still names the last forward step that completed — so a crash between restoring
// the ledger or the binary and recording `rolled_back` leaves a journal saying
// "migrated" about a machine that has already been put back.
//
// Resumed as an upgrade, every remaining step finds its work done, the probe
// passes against the RESTORED OLD binary, and the run records a COMMITTED upgrade
// to a release the machine is not running — then releases its claim, so nothing
// afterwards knows the record is false.
func TestAnInterruptedRollbackIsResumedAsARollback(t *testing.T) {
	h := &fakeHost{}
	j := newJournal(t)
	j.Step = StepMigrated

	// What Run records on its way into a rollback, and where a crash leaves it.
	if err := j.Fail(errors.New("the candidate would not come up")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	err := Run(t.Context(), Request{Journal: j, Host: h})
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("resuming an interrupted rollback returned %v, want ErrRolledBack", err)
	}

	if j.Step != StepRolledBack {
		t.Errorf("the journal says %s, want rolled_back; a resumed rollback that reports "+
			"a committed upgrade is a durable lie about which release this host runs",
			j.Step)
	}

	// AND IT RESTORED RATHER THAN INSTALLED. The steps after `migrated` are the
	// probe and the commit, and running them is exactly the false success.
	if !slices.Contains(h.did, "restore-preserved") {
		t.Errorf("the resumed rollback did not restore the preserved installation: %v", h.did)
	}

	if slices.Contains(h.did, "install-candidate") {
		t.Errorf("the resumed rollback installed the candidate: %v", h.did)
	}
}

// EVERY WAY INTO A ROLLBACK RECORDS THAT ONE STARTED.
//
// Resuming an interrupted rollback works by recognising a journal that has a
// FAILURE and no decision, so a path that entered a rollback without writing one
// is a path whose interruption cannot be told from an upgrade still in progress —
// and resuming that walks the remaining steps against a machine already restored,
// then records a committed upgrade to a release it is not running.
//
// The commit-record path was exactly that: it rolled back over a journal whose
// write had just failed, leaving nothing to say so. This is a STRUCTURAL check
// because the hazard is a new caller, not a wrong one — nothing in the types
// stops the next failure path calling rollBack directly, and the loss would be a
// host that reports a release it is not running.
func TestEveryRollbackIsEnteredThroughOnePlace(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "upgrade.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse upgrade.go: %v", err)
	}

	var callers []string

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "rollBack" {
				callers = append(callers, fn.Name.Name)
			}

			return true
		})
	}

	if len(callers) == 0 {
		t.Fatal("nothing calls rollBack; this gate is watching a name that moved")
	}

	for _, caller := range callers {
		// Run may reach it directly ONLY to resume one that beginRollback already
		// recorded, which is the branch keyed on a non-empty Failure.
		if caller != "beginRollback" && caller != "Run" {
			t.Errorf("%s calls rollBack directly; a rollback entered without recording a "+
				"failure first cannot be told from an upgrade in progress when it is "+
				"interrupted, and resuming it reports a committed upgrade to a release "+
				"the host is not running", caller)
		}
	}
}

// A COMMITTED UPGRADE RESUMED AFTER A CRASH STILL RECORDS WHICH MANIFEST
// PRODUCED IT.
//
// The commit record and the provenance record are two writes, and a crash
// between them leaves a journal at `committed` — which Run resumes by calling
// finish directly, skipping everything before it. Recording beside the commit
// therefore skipped it on exactly the machine that had just been upgraded, and
// that host reported no manifest until something upgraded it again. A rollout
// would then take it on its version alone, which is the evidence this whole
// change exists to stop relying on.
func TestACommittedUpgradeResumedAfterACrashStillRecordsItsManifest(t *testing.T) {
	h := &fakeHost{}
	j := newJournal(t)
	j.Step = StepCommitted

	if err := Run(t.Context(), Request{Journal: j, Host: h}); err != nil {
		t.Fatalf("resuming a committed upgrade: %v", err)
	}

	if !slices.Contains(h.did, "record-installed") {
		t.Errorf("a resumed committed upgrade did not record which manifest produced "+
			"it: %v", h.did)
	}

	// AND IT RECORDED THE MANIFEST THIS UPGRADE INSTALLED. An empty digest is not a
	// weaker record, it is an instruction to REMOVE one — so a host that had just
	// been upgraded would report nothing.
	if h.recordedDigest != j.TargetDigest || h.recordedVersion != j.ToVersion {
		t.Errorf("it recorded %q/%q, want %q/%q",
			h.recordedDigest, h.recordedVersion, j.TargetDigest, j.ToVersion)
	}

	// AND IT STILL RECORDED BEFORE THE SERVICES CAME BACK, because a registration
	// that happened first would name the release this upgrade replaced.
	record := slices.Index(h.did, "record-installed")
	start := slices.Index(h.did, "start-services")

	if record > start {
		t.Errorf("the manifest was recorded after the services started: %v", h.did)
	}
}

// A HOST THAT CANNOT GET AN IMAGE ITS CANDIDATE ACCEPTS ROLLS BACK, with the
// services restored and no ledger restored, because nothing had been fenced or
// snapshotted yet. Without this a candidate that refuses every generation on the
// cluster would be committed onto a host that launches nothing.
func TestAFailedImagePullRollsBackBeforeTheFence(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "prepare-images"}

	err := runUnwinding(t, j, h)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed image pull returned %v, want ErrRolledBack", err)
	}

	if slices.Contains(h.did, "fence") || slices.Contains(h.did, "restore-ledger") {
		t.Errorf("the pull failed before the fence, so nothing should be fenced or restored:\n  %v", h.did)
	}

	for _, want := range []string{"restore-preserved", "start-services-rollback", "prove-stable"} {
		if !slices.Contains(h.did, want) {
			t.Errorf("the rollback did not %s:\n  %v", want, h.did)
		}
	}
}
