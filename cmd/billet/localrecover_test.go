package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
)

// `billet local recover` puts a deployment back over ITSELF — the case restore
// refuses, and the ordinary disaster shape: the controller is sick, the operator
// has yesterday's archive, and the ledger on disk is not one they want to keep.

// populateLedger puts a row in the target's ledger, so it is a live capacity
// record rather than the empty preflight one a restore may replace.
func populateLedger(t *testing.T, f backupFixture) string {
	t.Helper()

	db, err := state.OpenAdmin(t.Context(), f.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
		}})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	// EVERY RESERVATION NAMES A MACHINE, so a tier with no eligible host has no
	// capacity to escrow against and Reserve refuses. This registers one for the
	// same reason a real deployment has one.
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "recover-host", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "billet-2vcpu")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	return lease.ID
}

// releaseEverything makes the deployment quiet, which is what a recovery waits
// for.
func releaseEverything(t *testing.T, f backupFixture, leaseID string) {
	t.Helper()

	db, err := state.OpenAdmin(t.Context(), f.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
		}})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	lease, err := a.Lease(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// A recovery replaces a POPULATED ledger and moves the old one aside.
//
// THE OLD LEDGER IS NEVER DELETED. It is the only record of the work this
// operation failed, and an operator who has just accepted losing jobs needs to
// be able to say which they were.
func TestARecoveryReplacesThisDeploymentsLedgerAndKeepsTheOldOne(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	// The deployment carries on afterwards, which is what makes this a recovery
	// rather than a restore: the ledger on disk is no longer the archive's.
	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	before := digestOf(t, filepath.Join(f.stateDir, "billet.db"))

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local recover: %v", err)
	}

	rec, found := openArchive(t, archive).Manifest.Record(deployarchive.EntryLedger)
	if !found {
		t.Fatal("the archive declares no ledger")
	}

	// THE ARCHIVE'S LEDGER IS IN PLACE, asserted by its CONTENT rather than its
	// bytes: sealing the recovered deployment writes to it, so a byte comparison
	// against the archive would fail against a correct recovery. What must hold
	// is that the lease created AFTER the backup is not in it — which is the
	// whole reason this operation is dangerous and has to be accepted job by job.
	if leaseGone := leaseIsAbsent(t, f, lease); !leaseGone {
		t.Errorf("lease %s survived the recovery; the ledger in place is not the archive's",
			lease)
	}

	// AND THE ONE THAT WAS THERE IS BESIDE IT, not gone.
	//
	// COMPARED TO THE ARCHIVE'S rather than to a digest taken before the command
	// ran: sealing the deployment WRITES to the ledger, and SQLite checkpoints
	// its WAL on close, so the bytes legitimately move between those two moments.
	// What must hold is that the file moved aside is the deployment's own live
	// record and not a copy of the archive's.
	aside := supersededLedgerIn(t, f.stateDir)

	if got := digestOf(t, aside); got == rec.SHA256 {
		t.Errorf("%s is the ARCHIVE's ledger; the live one was not preserved", aside)
	}

	if before == rec.SHA256 {
		t.Fatal("the ledger on disk was already the archive's, so this proved nothing")
	}

	contents, err := state.PeekLedger(t.Context(), aside)
	if err != nil {
		t.Fatalf("PeekLedger(%s): %v", aside, err)
	}

	if !contents.Populated {
		t.Errorf("%s holds no deployment data, so it is not the live ledger", aside)
	}
}

// A recovery REFUSES a host that is not already this deployment.
//
// It is the mirror image of restore's refusal, and both halves matter: restore
// will not touch a commissioned deployment, and recover will not commission one.
// Without this, `recover` would be a restore that destroys whatever it finds.
func TestARecoveryRefusesAHostThatIsNotAlreadyThisDeployment(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	// A host that has never been commissioned.
	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	err := cmdLocalRecover(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a recovery ran against a host that is not this deployment")
	}

	if !strings.Contains(err.Error(), "billet local restore") {
		t.Errorf("the refusal does not name the operation that IS right here: %v", err)
	}

	// AND NOTHING WAS WRITTEN. A refusal that had installed an identity would
	// have commissioned the host on its way to saying no.
	if _, _, err := state.PeekDeploymentID(tgt.stateDir); err != nil {
		t.Fatalf("PeekDeploymentID: %v", err)
	}
}

// A recovery REFUSES another deployment's archive, whatever else is true.
//
// This is AdoptDeploymentID's invariant: relabelling a directory makes the
// compute it is already managing invisible to both installations.
func TestARecoveryRefusesAnotherDeploymentsArchive(t *testing.T) {
	stubLifecycleLock(t)

	other := newBackupFixture(t, true)
	archive := backupInto(t, other)

	mine := newBackupFixture(t, true)

	err := cmdLocalRecover(t.Context(), []string{
		"--config", mine.configPath, "--from", archive, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a recovery installed another deployment's archive")
	}

	id, _, peekErr := state.PeekDeploymentID(mine.stateDir)
	if peekErr != nil {
		t.Fatalf("PeekDeploymentID: %v", peekErr)
	}

	if id != mine.deployment {
		t.Errorf("this host is now deployment %s, want %s", id, mine.deployment)
	}
}

// A recovery REFUSES while the deployment is still holding work, unless the
// operator accepts losing it BY NAME.
//
// The restored ledger has no lease for compute created after the backup, so node
// recovery destroys those instances as orphans — and GitHub does not requeue a
// job that already started.
func TestARecoveryWillNotStrandRunningWorkUnasked(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	populateLedger(t, f)

	// A timeout, or this waits forever for work nothing is going to finish —
	// which is the correct production behaviour and an unusable test.
	err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
		"--timeout", "10ms",
	})
	if err == nil {
		t.Fatal("a recovery ran while the deployment was still holding a lease")
	}

	// THE LEDGER IS UNTOUCHED. A refusal that had already moved it aside would
	// have destroyed the record it was refusing to destroy.
	if _, statErr := os.Lstat(filepath.Join(f.stateDir, "billet.db")); statErr != nil {
		t.Errorf("the ledger is not where it was: %v", statErr)
	}

	if aside := supersededLedgerOrEmpty(f.stateDir); aside != "" {
		t.Errorf("a refused recovery moved the ledger to %s", aside)
	}

	// AND THE DEPLOYMENT IS SEALED, because the seal is what stops the work it
	// is waiting on from growing.
	assertSealed(t, f)
}

// With --accept-failing-jobs it proceeds, having named what it strands.
func TestARecoveryProceedsWhenTheJobsAreAcceptedByName(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	populateLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
		"--accept-failing-jobs",
	}); err != nil {
		t.Fatalf("billet local recover --accept-failing-jobs: %v", err)
	}

	if aside := supersededLedgerOrEmpty(f.stateDir); aside == "" {
		t.Error("the ledger that was there was not moved aside")
	}

	// AND ADMISSION IS STILL CLOSED. The nodes still hold compute the restored
	// ledger has never heard of; reopening here would admit new work on top of
	// it.
	assertSealed(t, f)
}

// --dry-run changes nothing, including the seal.
func TestARecoveryDryRunSealsNothingAndMovesNothing(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	populateLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--dry-run",
	}); err != nil {
		t.Fatalf("billet local recover --dry-run: %v", err)
	}

	if aside := supersededLedgerOrEmpty(f.stateDir); aside != "" {
		t.Errorf("--dry-run moved the ledger to %s", aside)
	}

	db, err := state.OpenAdmin(t.Context(), f.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	admission, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if admission.Sealed() {
		t.Error("--dry-run sealed the deployment")
	}
}

// And without the fleet-fencing assertion it does not run at all.
func TestARecoveryRefusesWithoutTheFleetFencingAssertion(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	populateLedger(t, f)

	err := cmdLocalRecover(t.Context(), []string{"--config", f.configPath, "--from", archive})
	if err == nil {
		t.Fatal("a recovery ran without --old-controller-fenced")
	}

	if aside := supersededLedgerOrEmpty(f.stateDir); aside != "" {
		t.Errorf("a refused recovery moved the ledger to %s", aside)
	}
}

// AN ABANDONED RECOVERY PUTS THE LEDGER BACK.
//
// Without it, an abandon removes the ledger the run installed — having proved it
// is the archive's copy — and leaves the operator's own under a name nothing
// looks at, in a directory whose fence is about to come down. That is a
// deployment with no capacity record at all, produced by the command whose whole
// job is undoing damage.
func TestAnAbandonedRecoveryPutsTheLedgerBack(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	// STOPPED AFTER THE LEDGER LANDED, which is the state an abandon exists for:
	// the rename happened, the archive's ledger is in place, and the run stopped
	// before the rest.
	//
	// STAGED THE WAY A REAL HOST PRODUCES IT rather than through a test hook the
	// production code would have to carry: the App key is published LAST, so a
	// missing key plus a directory billet cannot write is an interruption at
	// exactly that boundary — and a read-only /etc/billet is an ordinary thing to
	// find on a machine somebody is in the middle of repairing.
	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	if supersededLedgerOrEmpty(f.stateDir) == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("billet local recover --abandon: %v", err)
	}

	// THE LEDGER IS BACK AT ITS OWN NAME, and it is the live one.
	if aside := supersededLedgerOrEmpty(f.stateDir); aside != "" {
		t.Errorf("the abandon left the ledger at %s", aside)
	}

	contents, err := state.PeekLedger(t.Context(), filepath.Join(f.stateDir, "billet.db"))
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if !contents.Populated {
		t.Error("the ledger that came back holds no deployment data, so it is not the live one")
	}

	// AND THE DIRECTORY IS OPEN AGAIN, or nothing can start on it.
	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the abandon left the ledger fenced (%v)", err)
	}
}

// AN INTERRUPTED RECOVERY RESUMES, which it could not before.
//
// The first attempt leaves the directory FENCED — deliberately, so nothing can
// start on a half-recovered deployment — and quiescing opens the ledger through
// OpenAdmin, which honours that fence. A retry that quiesced again would
// therefore be refused by its own first attempt, and the documented resume would
// be unreachable. This drives both attempts through the command.
func TestAnInterruptedRecoveryResumes(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	// FENCED, which is the state the resume has to survive.
	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Fatalf("the interrupted recovery did not leave the ledger fenced: %v", err)
	}

	// Let the App key land this time.
	if err := os.Chmod(filepath.Dir(f.keyPath), 0o700); err != nil {
		t.Fatalf("restore write permission: %v", err)
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("the resumed recovery: %v", err)
	}

	// AND IT FINISHED: the fence is down, nothing is left mid-flight, and the
	// deployment is sealed.
	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the resumed recovery left the ledger fenced (%v)", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the resumed recovery left its journal behind (%v)", err)
	}

	assertSealed(t, f)
}

// THE FENCE OUTLIVES THE PUBLICATION, until the seal is taken.
//
// Clearing it first and sealing afterwards leaves an interval in which a control
// plane can start on the ledger the archive brought back — whose admission row is
// the one it had when the backup was taken, which is open. The fence is the only
// thing that refuses one, so the executor leaves it up and the command takes it
// down only after the seal is durable. Driven through the EXECUTOR, because that
// is whose contract this is.
func TestTheFenceOutlivesThePublicationUntilTheSealIsTaken(t *testing.T) {
	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	})
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("PlanRecover refused: %v", plan.Refusals)
	}

	res, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !res.Unfinished {
		t.Fatal("a recovery's publication reported itself finished; the fence would come down " +
			"before anything could seal the ledger it just restored")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the fence came down with the publication: %v", err)
	}

	// AND Finish PROVES THE SEAL RATHER THAN TRUSTING ITS CALLER. Called here,
	// with the archive's own open admission row in place, it must refuse — an
	// exported entry point that only documented "seal first" would open exactly
	// the window the fence exists to close.
	if err := deployarchive.Finish(t.Context(), plan); err == nil {
		t.Fatal("Finish lifted the fence over an OPEN ledger")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish lifted the fence anyway: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish removed the journal anyway: %v", err)
	}

	sealThroughTheFence(t, f.stateDir)

	// AND Finish TAKES IT DOWN, which is the other half.
	if err := deployarchive.Finish(t.Context(), plan); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("Finish left the ledger fenced (%v)", err)
	}
}

// sealThroughTheFence takes the operator seal a recovery's caller owes, using
// the one handle that crosses a fence.
func sealThroughTheFence(t *testing.T, stateDir string) {
	t.Helper()

	db, err := state.OpenMaintenance(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the restored ledger to seal it: %v", err)
	}

	defer db.Close()

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Expect: current.Generation, Provenance: state.ProvenanceOperator,
		Reason: "test", Actor: "test", KeepExisting: true,
	}); err != nil {
		t.Fatalf("seal: %v", err)
	}
}

// Finish REFUSES a journal that is not the one its own archive wrote.
//
// It is an exported entry point that unlinks a journal and lifts a fence, so it
// cannot assume its caller just ran the Execute that raised that fence. Handed a
// plan for one archive over a directory holding another's journal, an
// unconditional removal would delete the record of somebody else's half-published
// restore and then open the directory for a control plane to start on — the exact
// catastrophe the journal and the fence exist to prevent, reached through the
// function whose job is ending one safely.
func TestFinishRefusesAnotherOperationsJournal(t *testing.T) {
	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	})
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if _, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// A SECOND ARCHIVE, whose plan names the same directory. Finish must not act
	// on the first one's journal.
	other := newBackupFixture(t, true)
	otherArchive := backupInto(t, other)

	stranger, err := deployarchive.Open(t.Context(), otherArchive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// SEALED FIRST, or this proves nothing about the journal at all: requireSealed
	// runs too and would refuse on its own, so an unsealed ledger makes the
	// assertion below pass without the mismatch branch ever being reached.
	sealThroughTheFence(t, f.stateDir)

	wrong := plan
	wrong.Archive = stranger

	err = deployarchive.Finish(t.Context(), wrong)
	if err == nil {
		t.Fatal("Finish removed a journal belonging to a different archive")
	}

	if !strings.Contains(err.Error(), stranger.Dir) &&
		!strings.Contains(err.Error(), "not the archive this operation published") {
		t.Errorf("Finish refused for some reason other than the journal mismatch: %v", err)
	}

	// AND IT LEFT BOTH IN PLACE, which is the half that matters: a journal
	// removed and a fence lifted would make a half-published directory startable.
	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish removed the journal anyway: %v", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish lifted the fence anyway: %v", err)
	}

	// The real one still works.
	if err := deployarchive.Finish(t.Context(), plan); err != nil {
		t.Fatalf("Finish with the right archive: %v", err)
	}
}

// stopAfterTheLedger makes the App-key publication — which is ranked last —
// fail, so a run stops with the ledger already superseded and installed.
func stopAfterTheLedger(t *testing.T, f backupFixture) {
	t.Helper()

	if err := os.Remove(f.keyPath); err != nil {
		t.Fatalf("clear the App key: %v", err)
	}

	dir := filepath.Dir(f.keyPath)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("make %s read-only: %v", dir, err)
	}

	// PUT BACK, or the temporary directory cannot be removed: unlinking a file
	// needs write permission on the directory holding it, so a test that left
	// this would fail its own cleanup.
	t.Cleanup(func() {
		//nolint:errcheck // best effort; the temp directory's own cleanup reports a failure
		_ = os.Chmod(dir, 0o700)
	})
}

// leaseIsAbsent reports whether the live ledger has forgotten a lease.
//
// THE PROPERTY THIS WHOLE OPERATION IS DANGEROUS FOR: a restored ledger has no
// lease for compute created after the backup, so node recovery treats those
// instances as orphans and destroys them.
func leaseIsAbsent(t *testing.T, f backupFixture, leaseID string) bool {
	t.Helper()

	db, err := state.OpenAdmin(t.Context(), f.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
		}})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	_, err = a.Lease(t.Context(), leaseID)

	return errors.Is(err, alloc.ErrLeaseNotFound)
}

// assertSealed proves admission is closed.
func assertSealed(t *testing.T, f backupFixture) {
	t.Helper()

	db, err := state.OpenAdmin(t.Context(), f.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	admission, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if !admission.Sealed() {
		t.Error("the deployment is not sealed")
	}
}

// supersededLedgerIn finds the ledger a recovery moved aside, and fails if
// there is not exactly one.
func supersededLedgerIn(t *testing.T, stateDir string) string {
	t.Helper()

	found := supersededLedgerOrEmpty(stateDir)
	if found == "" {
		t.Fatalf("no superseded ledger in %s", stateDir)
	}

	return found
}

func supersededLedgerOrEmpty(stateDir string) string {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "billet.db.superseded-") {
			return filepath.Join(stateDir, e.Name())
		}
	}

	return ""
}

// digestOf hashes a file, so a ledger can be compared without reading a
// multi-megabyte database into a test's memory.
func digestOf(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

func openArchive(t *testing.T, dir string) *deployarchive.Archive {
	t.Helper()

	a, err := deployarchive.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return a
}

// A RESTORE'S FENCE IS NOT A RECOVERY IN PROGRESS.
//
// The resume path exists so a recovery interrupted after it sealed and quiesced
// does not have to seal and quiesce again behind its own fence. What decides it
// is this predicate, and the two facts it used to ask — a journal naming this
// archive, and a fence of some kind — are both satisfied by an interrupted
// `billet local restore --from` the same archive, which took no seal and proved
// nothing quiet. So the reason is part of the question.
func TestARestoresFenceIsNotARecoveryToResume(t *testing.T) {
	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("open the archive: %v", err)
	}

	// THE JOURNAL IS IDENTIFIED BY ITS MANIFEST, so the staged one has to carry
	// the real digest or the predicate answers "not this archive" and every case
	// below passes for the wrong reason.
	manifest, err := os.ReadFile(filepath.Join(archive, deployarchive.EntryManifest))
	if err != nil {
		t.Fatalf("read the archive manifest: %v", err)
	}

	sum := sha256.Sum256(manifest)

	stage := func(intent string) {
		journal := fmt.Appendf(nil,
			`{"schema":3,"intent":%q,"archive_dir":%q,"manifest_sha256":%q}`,
			intent, a.Dir, hex.EncodeToString(sum[:]))
		if err := os.WriteFile(deployarchive.JournalPath(f.stateDir), journal, 0o600); err != nil {
			t.Fatalf("stage a %s journal: %v", intent, err)
		}
	}

	for _, tc := range []struct {
		intent string
		reason string
		want   recoveryStage
	}{
		// A RECOVERY'S OWN JOURNAL, which only its own fence makes resumable.
		{intent: "recover", reason: deployarchive.FenceReason, want: recoverFresh},
		{intent: "recover", reason: "ansible host upgrade", want: recoverFresh},
		{intent: "recover", reason: deployarchive.RecoverFenceReason, want: recoverResume},
		// A RESTORE'S JOURNAL FOR THE SAME ARCHIVE is not this operation's, and
		// the fence beside it decides nothing. Without the intent guard the last
		// of these resumes on somebody else's record.
		{intent: "restore", reason: deployarchive.FenceReason, want: recoverFresh},
		{intent: "restore", reason: deployarchive.RecoverFenceReason, want: recoverFresh},
	} {
		stage(tc.intent)

		if err := os.WriteFile(state.MaintenanceFencePath(f.stateDir),
			[]byte(tc.reason+"\n"), 0o600); err != nil {
			t.Fatalf("stage the %q fence: %v", tc.reason, err)
		}

		switch got, err := recoveryStageOf(f.stateDir, a); {
		case err != nil:
			t.Fatalf("recoveryStageOf on a %s journal under the %q fence: %v",
				tc.intent, tc.reason, err)
		case got != tc.want:
			t.Errorf("recoveryStageOf on a %s journal under the %q fence = %v, want %v",
				tc.intent, tc.reason, got, tc.want)
		}
	}
}

// AN ABANDON PUTS BACK A SIDECAR THE LEDGER NEVER FOLLOWED.
//
// supersedeLedger moves the SIDECARS FIRST, so a run that dies between a -wal
// and the ledger leaves the operator's billet.db exactly where it was and its
// write-ahead log under the superseded name. An abandon that asked only "is
// there a superseded ledger" answered "nothing was superseded", removed the
// journal and lifted the fence — and the ledger is then reopened without a log
// holding committed transactions. SQLite loses them silently, which is the one
// outcome this command exists to prevent.
func TestAnAbandonedRecoveryPutsBackASidecarTheLedgerNeverFollowed(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	// RE-STAGED ONE RENAME EARLIER. The run above completed the supersede; this
	// is the state it passes through on the way there — the -wal moved, the
	// ledger not yet — and it is reachable by putting the ledger back under its
	// own name and leaving a superseded log behind it.
	ledger := deployarchive.LedgerPath(f.stateDir)

	if err := os.Remove(ledger); err != nil {
		t.Fatalf("clear the installed ledger: %v", err)
	}

	if err := os.Rename(aside, ledger); err != nil {
		t.Fatalf("put the operator's ledger back at its own name: %v", err)
	}

	wal := []byte("committed transactions this ledger has not checkpointed")
	supersedeSidecar(t, f.stateDir, aside+"-wal", wal)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("billet local recover --abandon: %v", err)
	}

	// THE LOG IS BACK BESIDE ITS OWN DATABASE.
	back, err := os.ReadFile(ledger + "-wal")
	if err != nil {
		t.Fatalf("the abandon left the write-ahead log under the superseded name: %v", err)
	}

	if !bytes.Equal(back, wal) {
		t.Error("the file put back beside the ledger is not the log that was moved aside")
	}

	if _, err := os.Lstat(aside + "-wal"); !os.IsNotExist(err) {
		t.Errorf("the superseded write-ahead log is still there too (%v), so SQLite now has "+
			"two", err)
	}
}

// AN ABANDON WILL NOT ATTACH A LEDGER TO A LOG THAT IS NOT ITS OWN.
//
// The set moves as one thing. A -wal sitting at the canonical name while the
// superseded ledger is being put back belongs to the database this abandon has
// just removed, and SQLite replays a log it finds beside a database — so
// attaching the operator's ledger to it is corruption produced by the command
// whose job is undoing damage. Everything is kept and named instead.
func TestAnAbandonedRecoveryRefusesToAttachAForeignLog(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	ledger := deployarchive.LedgerPath(f.stateDir)

	// THE INPUT putSupersededBack ACTUALLY SEES. The abandon removes the ledger
	// this run installed and does NOT remove its sidecars, so a log left beside
	// that database is still at the canonical name when the superseded ledger is
	// about to move back on top of it. Staged by hand because SQLite deletes a
	// -wal beside a database it closes cleanly, so the state cannot be reached by
	// leaving one there and waiting.
	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the installed ledger: %v", err)
	}

	foreign := []byte("a journal for the database the abandon has just removed")
	if err := os.WriteFile(ledger+"-wal", foreign, 0o600); err != nil {
		t.Fatalf("stage the foreign log: %v", err)
	}

	// IT REFUSES, and that is the whole point: "I decided not to move anything"
	// and "this directory is ready to start" must never be the same answer. An
	// abandon that returned nil here would go on to remove the journal and lift
	// the fence over a directory with no billet.db in it at all.
	err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	})
	if err == nil {
		t.Fatal("the abandon reported success over a ledger set it could not reassemble")
	}

	if !strings.Contains(err.Error(), ledger+"-wal") {
		t.Errorf("the refusal does not name the file that blocked it: %v", err)
	}

	// NOTHING MOVED, and the fence and journal are both still there for whoever
	// looks.
	if _, err := os.Lstat(aside); err != nil {
		t.Errorf("the refused abandon moved the superseded ledger anyway: %v", err)
	}

	still, err := os.ReadFile(ledger + "-wal")
	if err != nil {
		t.Fatalf("read the log left at the canonical name: %v", err)
	}

	if !bytes.Equal(still, foreign) {
		t.Error("the log at the canonical name was replaced")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon removed the journal: %v", err)
	}
}

// AN ABANDON RESUMES ITS OWN PUT-BACK RATHER THAN READING IT AS A COLLISION.
//
// The put-back moves the ledger FIRST and the sidecars after, so a crash in the
// middle leaves the operator's billet.db home and a superseded -wal still out.
// The other order left a canonical -wal beside a superseded LEDGER, which the
// foreign-log guard above reads as a collision — and with a refusal that was a
// silent "keep", the retry lifted the fence over a directory with no ledger.
func TestAnAbandonResumesItsOwnPutBack(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	ledger := deployarchive.LedgerPath(f.stateDir)

	// THE STATE A CRASH MID-PUT-BACK LEAVES: the ledger is home and its own
	// write-ahead log is still under the superseded name.
	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the installed ledger: %v", err)
	}

	if err := os.Rename(aside, ledger); err != nil {
		t.Fatalf("put the ledger home: %v", err)
	}

	wal := []byte("committed transactions this ledger has not checkpointed")
	supersedeSidecar(t, f.stateDir, aside+"-wal", wal)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("the abandon resuming its own put-back: %v", err)
	}

	back, err := os.ReadFile(ledger + "-wal")
	if err != nil {
		t.Fatalf("the log was not put back beside its ledger: %v", err)
	}

	if !bytes.Equal(back, wal) {
		t.Error("the file beside the ledger is not the log that was moved aside")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the finished abandon left the ledger fenced (%v)", err)
	}
}

// AN ABANDON WILL NOT OPEN A DEPLOYMENT WITH NO LEDGER.
//
// Everything else here is about not writing OVER a ledger. This is the other
// half, and it is what the fence coming down actually promises: a recover only
// ever runs against a deployment that had one, so ending with none means the
// operator's was removed or never put back — and a control plane starting there
// mints a fresh, empty capacity record.
func TestAnAbandonRefusesToOpenADeploymentWithNoLedger(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	// BOTH LEDGERS GONE — an operator who moved the superseded one somewhere
	// else, and the installed one removed by the abandon itself. The put-back has
	// nothing to do and the directory has no capacity record.
	if err := os.Remove(aside); err != nil {
		t.Fatalf("move the superseded ledger away: %v", err)
	}

	err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	})
	if err == nil {
		t.Fatal("the abandon lifted the fence over a directory with no ledger")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}
}

// A RECOVERY THAT ALREADY PUBLISHED IS FINISHED, NOT PUBLISHED AGAIN.
//
// This is the ordinary finalization crash: every file is down, the operator's
// ledger is aside, and the run stopped before sealing. A retry that planned
// afresh would decide on a SECOND supersede — the restored ledger is no longer
// byte-identical to the archive's — and find the first one's destination
// occupied, which supersedeLedger refuses because that file is the only record
// of the work the recovery failed. The command billet's own diagnostic tells the
// operator to re-run would then have no way forward, so the journal records how
// far it got and the retry skips straight to finishing.
func TestARecoveryThatAlreadyPublishedIsFinishedRatherThanRepublished(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	})
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	// STOPPED EXACTLY WHERE THE CRASH LANDS: the executor publishes and returns
	// Unfinished, and nothing seals or lifts the fence.
	res, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !res.Unfinished {
		t.Fatal("the publication did not report itself unfinished")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the publication did not move the old ledger aside")
	}

	before := digestOf(t, aside)

	// AND THE SEAL LANDED, which is what makes this the interesting crash. The
	// caller's next steps are the writer barrier, the journal and the fence, and
	// a failure in any of them leaves the restored ledger DIFFERENT from the
	// archive's — sealing writes to it — so a retry that planned afresh would
	// decide on a second supersede.
	sealThroughTheFence(t, f.stateDir)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("the retry after a published recovery: %v", err)
	}

	// IT FINISHED, and it did not move anything a second time.
	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the retry left the ledger fenced (%v)", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the retry left its journal behind (%v)", err)
	}

	if now := digestOf(t, aside); now != before {
		t.Error("the retry wrote over the ledger the first attempt moved aside")
	}

	assertSealed(t, f)
}

// AN ABANDON REFUSES AN OPERATION THAT FINISHED.
//
// Finish marks the journal before it lifts the fence, so a crash between the two
// leaves a complete, sealed deployment with a journal still in it. Undoing that
// is the worst thing this command can do: it removes the restored credentials
// and puts back the ledger the operator deliberately replaced.
func TestAnAbandonRefusesAnOperationThatFinished(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	})
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	// A COMPLETE PUBLICATION AND A REAL OPERATOR SEAL, which is what makes this
	// the state the guard is about. Staging "finished" onto a run that stopped
	// early would fire the guard while reproducing nothing it describes.
	res, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !res.Unfinished {
		t.Fatal("the publication did not report itself unfinished")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the publication did not move the old ledger aside")
	}

	sealThroughTheFence(t, f.stateDir)

	// AND INTERRUPTED WHERE Finish WRITES THE MARKER: the phase is durable before
	// the fence comes down, so a crash between those two steps is exactly this —
	// a finished, sealed deployment whose journal is still here.
	setJournalPhase(t, f.stateDir, "finished")

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err == nil {
		t.Fatal("an abandon undid an operation that had finished")
	}

	// AND IT UNDID NOTHING: the ledger is still aside, where a finished
	// operation left it, and the restored one is still in place.
	if _, err := os.Lstat(aside); err != nil {
		t.Errorf("the refused abandon put the superseded ledger back anyway: %v", err)
	}

	if _, err := os.Lstat(deployarchive.LedgerPath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon removed the restored ledger: %v", err)
	}

	// AND RE-RUNNING WITHOUT --abandon IS THE WAY OUT, which is what the refusal
	// tells the operator to do.
	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("the retry after a finished operation: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the retry left the journal behind (%v)", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the retry left the ledger fenced (%v)", err)
	}
}

// setJournalPhase rewrites how far a journal says its operation got.
//
// THROUGH THE FILE rather than a hook in the production code: the phase is a
// crash marker, and what a test needs to stage is the file a crash leaves.
func setJournalPhase(t *testing.T, stateDir, phase string) {
	t.Helper()

	path := deployarchive.JournalPath(stateDir)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the journal does not parse: %v", err)
	}

	body["phase"] = phase

	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the journal: %v", err)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write the journal: %v", err)
	}
}

// A JOURNAL THIS BUILD DOES NOT UNDERSTAND IS NEVER ACTED ON.
//
// The phase decides whether a recovery skips sealing, proving quiescence and
// publishing altogether, so a record written by a NEWER billet — same manifest
// digest, a schema or a phase this one has never seen — must stop the command
// rather than be read as one of ours. journalSchema exists to say exactly that
// and, until this, InProgress did not consult it.
func TestAJournalThisBuildDoesNotUnderstandIsNeverActedOn(t *testing.T) {
	stubLifecycleLock(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "a newer schema", body: `{"schema":99,"intent":"recover","phase":"published"`},
		{name: "an older schema", body: `{"schema":2,"intent":"recover","phase":"published"`},
		{name: "an unknown phase", body: `{"schema":3,"intent":"recover","phase":"reticulating"`},
		{name: "an unknown operation", body: `{"schema":3,"intent":"reticulate","phase":"published"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBackupFixture(t, true)
			archive := backupInto(t, f)

			lease := populateLedger(t, f)
			releaseEverything(t, f, lease)

			manifest, err := os.ReadFile(filepath.Join(archive, deployarchive.EntryManifest))
			if err != nil {
				t.Fatalf("read the archive manifest: %v", err)
			}

			sum := sha256.Sum256(manifest)

			journal := fmt.Appendf(nil, `%s,"archive_dir":%q,"manifest_sha256":%q}`,
				tc.body, archive, hex.EncodeToString(sum[:]))
			if err := os.WriteFile(deployarchive.JournalPath(f.stateDir), journal,
				0o600); err != nil {
				t.Fatalf("stage the journal: %v", err)
			}

			if err := cmdLocalRecover(t.Context(), []string{
				"--config", f.configPath, "--from", archive, "--old-controller-fenced",
			}); err == nil {
				t.Fatal("a recovery acted on a journal this build does not understand")
			}

			// AND IT IS STILL THERE. Whatever wrote it is entitled to finish or
			// undo it, and this build removing it would take that away.
			if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
				t.Errorf("the refused recovery removed the journal: %v", err)
			}

			// AND NOTHING WAS TOUCHED. Refusing at the end is not the same as
			// refusing at the start: reading the phase as one of ours takes the
			// command down the already-published path, which SEALS this
			// deployment before anything downstream gets a chance to object.
			switch admission, err := state.PeekAdmission(t.Context(), f.stateDir); {
			case err != nil:
				t.Fatalf("PeekAdmission: %v", err)
			case admission.Sealed():
				t.Error("the refused recovery sealed this deployment on its way to refusing")
			}
		})
	}
}

// Finish REFUSES A PUBLICATION THAT DID NOT FINISH.
//
// Lifting the fence is what makes a directory startable, and a journal still
// reading "publishing" says files are missing from it. An exported entry point
// that took the fence down over that would hand a control plane a deployment
// with holes in it — and the caller's seal, which is the other thing Finish
// checks, says nothing at all about whether the files landed.
func TestFinishRefusesAPublicationThatDidNotFinish(t *testing.T) {
	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	})
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if _, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// EVERYTHING ELSE Finish ASKS FOR IS IN PLACE — the seal, the fence, this
	// archive's own journal — so the phase is the only thing left to refuse on.
	sealThroughTheFence(t, f.stateDir)
	setJournalPhase(t, f.stateDir, "publishing")

	if err := deployarchive.Finish(t.Context(), plan); err == nil {
		t.Fatal("Finish lifted the fence over a publication that had not finished")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish lifted the fence anyway: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused Finish removed the journal anyway: %v", err)
	}
}

// THE LEDGER GOES BACK FIRST, and the order is the whole reason a crash
// mid-put-back is resumable.
//
// Coming back, the ledger's presence at the CANONICAL name is what says the set
// is home — the mirror of the forward direction, where its presence at the
// SOURCE is what makes a re-plan return to supersedeLedger. Sidecars first left
// a canonical -wal beside a superseded LEDGER, which the foreign-log guard reads
// as a state it cannot explain, so a retry refused and the operator was left
// with a directory whose ledger was one rename from home and no command that
// would do it.
//
// ASSERTED ON THE REPORT, because that is the only place the order is visible:
// each implementation handles the partial states its OWN order produces, so no
// end-state assertion can tell them apart. Result.Restored is the honest record
// of what moved and when.
func TestAnAbandonPutsTheLedgerBackBeforeItsSidecars(t *testing.T) {
	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	a, err := deployarchive.Open(t.Context(), archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	target := deployarchive.Target{
		ConfigPath: f.configPath, StateDir: f.stateDir, AppKeyPath: f.keyPath,
		GitHub: deployarchive.GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
	}

	plan, err := deployarchive.PlanRecover(t.Context(), a, target)
	if err != nil {
		t.Fatalf("PlanRecover: %v", err)
	}

	if _, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan: plan, InstallAppKey: installAppKey, Now: time.Now, Actor: "test",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the publication did not move the old ledger aside")
	}

	// A SUPERSEDED LOG TO GO WITH IT, so there is more than one thing to put back
	// and the order can be observed at all. SQLite removes a -wal beside a
	// database it closes cleanly, so a real one is not reliably there.
	supersedeSidecar(t, f.stateDir, aside+"-wal", []byte("uncheckpointed"))

	res, err := deployarchive.Abandon(t.Context(), a, target, deployarchive.ReplaceLedger)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if len(res.Restored) < 2 {
		t.Fatalf("the abandon put back %v, want the ledger and its log", res.Restored)
	}

	if res.Restored[0] != deployarchive.LedgerPath(f.stateDir) {
		t.Errorf("the abandon put back %s first, want the ledger %s", res.Restored[0],
			deployarchive.LedgerPath(f.stateDir))
	}
}

// A LEDGER IS NEVER INSTALLED BESIDE A WRITE-AHEAD LOG BILLET CANNOT ACCOUNT FOR.
//
// SQLite replays a -wal it finds beside a database, so an absent billet.db with
// its sidecars still there is not an empty directory — it is a set that has come
// apart. The state is reachable: a supersede interrupted between its -wal and
// its ledger leaves exactly this, and so would any build that moved them in one
// batch with nothing ordering the renames. Without this the re-plan reads the
// directory as an ordinary install and puts the archive's database down on top.
func TestARestoreRefusesALedgerNameBesideAStrayWriteAheadLog(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	ledger := deployarchive.LedgerPath(tgt.stateDir)

	if err := os.MkdirAll(tgt.stateDir, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}

	if err := os.Remove(ledger); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the preflight ledger: %v", err)
	}

	if err := os.WriteFile(ledger+"-wal", []byte("a log for a database that is gone"),
		0o600); err != nil {
		t.Fatalf("stage the stray log: %v", err)
	}

	err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a restore installed a ledger beside a write-ahead log for another one")
	}

	if _, err := os.Lstat(ledger); !os.IsNotExist(err) {
		t.Errorf("the refused restore installed a ledger anyway (%v)", err)
	}
}

// AND A RECOVERY REFUSES THE TORN SUPERSEDE THAT PRODUCES THAT STATE.
//
// This is the state a power loss inside supersedeLedger can leave, because
// nothing orders two renames: the ledger's rename persisted and its -wal's did
// not. The re-plan then finds no billet.db, reads the directory as an ordinary
// install, and puts the archive's database down beside a log belonging to the
// one that is now under the superseded name. That is the corruption the forward
// ordering claims to prevent, reached by the ordering not being durable.
func TestARecoveryRefusesATornSupersede(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	ledger := deployarchive.LedgerPath(f.stateDir)

	// THE TORN STATE: the ledger's rename landed, its log's did not, and the
	// archive's copy was never installed.
	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the installed ledger: %v", err)
	}

	if err := os.WriteFile(ledger+"-wal", []byte("the superseded ledger's own log"),
		0o600); err != nil {
		t.Fatalf("stage the stray log: %v", err)
	}

	if err := os.Remove(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Fatalf("clear the journal: %v", err)
	}

	if err := state.ClearMaintenanceFence(f.stateDir,
		deployarchive.RecoverFenceReason); err != nil {
		t.Fatalf("clear the fence: %v", err)
	}

	err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("a recovery installed a ledger beside a log belonging to another one")
	}

	if _, err := os.Lstat(ledger); !os.IsNotExist(err) {
		t.Errorf("the refused recovery installed a ledger anyway (%v)", err)
	}

	if _, err := os.Lstat(aside); err != nil {
		t.Errorf("the refused recovery disturbed the superseded ledger: %v", err)
	}
}

// AN ABANDON WILL NOT ACCEPT AN ENTRY THAT MERELY HAS THE LEDGER'S NAME.
//
// A symlink or a directory at billet.db satisfies "something is there" and is
// not a file a control plane can open, so accepting either lifts the fence over
// the same empty-deployment outcome the postcondition exists to refuse — a
// control plane starting there mints a fresh, empty capacity record.
//
// MORE THAN ONE GUARD REFUSES THIS, and saying so is more useful than pretending
// otherwise: abandonOne re-reads every path it was asked about and will not
// prove a link is a copy of anything, and requireLedgerPresent asks separately
// whether what is left is a regular file. Reverting either alone leaves this
// green. What it pins is the end-to-end property — the fence does not come down
// over a ledger name that is not a ledger — and TestAnAbandonRefusesToOpenA
// DeploymentWithNoLedger is what pins the postcondition itself.
func TestAnAbandonRefusesALedgerNameThatIsNotAFile(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	ledger := deployarchive.LedgerPath(f.stateDir)

	// Nothing left for the put-back to do, and a link where the ledger should be.
	elsewhere := filepath.Join(t.TempDir(), "somewhere-else.db")
	if err := os.Rename(aside, elsewhere); err != nil {
		t.Fatalf("move the superseded ledger away: %v", err)
	}

	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the installed ledger: %v", err)
	}

	if err := os.Symlink(elsewhere, ledger); err != nil {
		t.Fatalf("stage the link: %v", err)
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err == nil {
		t.Fatal("an abandon lifted the fence over a link standing in for the ledger")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}
}

// AN ABANDON PROVES IT PUT THE RIGHT LEDGER BACK, not that a file is there.
//
// "Something is at billet.db" and "a regular file is at billet.db" were both
// tried and are the same mistake at different depths: the second is satisfied by
// an empty file, after which the next control plane initialises it as a fresh
// capacity record and this deployment's own is gone — past the command whose
// whole job was putting it back. The supersede records the ledger's digest
// before it moves anything, and that is what authorises lifting the fence.
func TestAnAbandonRefusesALedgerThatIsNotTheOneItMovedAside(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	ledger := deployarchive.LedgerPath(f.stateDir)

	// SOMEBODY "FIXED IT" WHILE THE DIRECTORY WAS FENCED: the superseded ledger
	// moved somewhere else and an empty file left at the canonical name. Every
	// earlier form of this check accepts that and lifts the fence.
	if err := os.Rename(aside, filepath.Join(t.TempDir(), "kept.db")); err != nil {
		t.Fatalf("move the superseded ledger away: %v", err)
	}

	if err := os.Remove(ledger); err != nil {
		t.Fatalf("remove the installed ledger: %v", err)
	}

	if err := os.WriteFile(ledger, nil, 0o600); err != nil {
		t.Fatalf("stage the empty ledger: %v", err)
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err == nil {
		t.Fatal("an abandon lifted the fence over a ledger it had never moved aside")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon removed the journal: %v", err)
	}
}

// AND IT ACCEPTS THE LEDGER IT DID MOVE ASIDE, which is the other half: a proof
// that refuses the real case is worse than none, because the next thing anybody
// does is delete the check.
//
// IT ALSO PINS THE RECORDING, which is the half a happy-path assertion alone
// cannot reach. Deleting the digest comparison leaves an abandon that still
// succeeds here, so the test asserts that the supersede WROTE the ledger's
// digest into the journal before it moved anything — without that there is
// nothing for the comparison to be right about.
func TestAnAbandonAcceptsTheLedgerItMovedAside(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	// AND IT DESCRIBES THE FILE THAT MOVED. Taken from the ledger under its
	// superseded name rather than from a snapshot before the command ran, because
	// the recovery SEALS this deployment on the way in and that is a write.
	if recorded, moved := journalDigestOf(t, f.stateDir,
		deployarchive.LedgerPath(f.stateDir)), digestOf(t, aside); recorded != moved {
		t.Errorf("the supersede recorded %q for the ledger it moved aside, and that file "+
			"digests to %q", recorded, moved)
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("billet local recover --abandon: %v", err)
	}

	contents, err := state.PeekLedger(t.Context(), deployarchive.LedgerPath(f.stateDir))
	if err != nil {
		t.Fatalf("PeekLedger: %v", err)
	}

	if !contents.Populated {
		t.Error("the ledger that came back holds no deployment data, so it is not the live one")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the abandon left the ledger fenced (%v)", err)
	}
}

// supersedeSidecar stages a write-ahead log the recovery moved aside, and
// records it in the journal exactly as supersedeLedger would have.
//
// STAGED IN BOTH PLACES OR IT IS NOT THE STATE IT CLAIMS TO BE. A file sitting
// under a superseded name that the journal does not describe is a DIFFERENT
// scenario — the substituted log the abandon must refuse — and a test that wrote
// only the file would be staging that one while claiming the other. The ledger
// is closed cleanly by the time a recovery runs, so SQLite has removed the real
// -wal and there is none to carry through; this reconstructs what the supersede
// would have recorded had there been.
func supersedeSidecar(t *testing.T, stateDir, superseded string, body []byte) {
	t.Helper()

	if err := os.WriteFile(superseded, body, 0o600); err != nil {
		t.Fatalf("stage %s: %v", superseded, err)
	}

	path := deployarchive.JournalPath(stateDir)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the journal does not parse: %v", err)
	}

	digests, ok := doc["superseded_digests"].(map[string]any)
	if !ok {
		t.Fatal("the journal recorded no superseded digests, so this recovery moved nothing")
	}

	// KEYED BY THE BASE NAME the supersede moved from, which is the canonical one.
	sum := sha256.Sum256(body)
	digests[filepath.Base(canonicalOf(t, stateDir, superseded))] = hex.EncodeToString(sum[:])
	doc["superseded_digests"] = digests

	// AND RECORDED IN Created WHERE supersedeLedger PUTS IT: sidecars before the
	// ledger, because that is the order it moves them in and it notes each
	// destination before its rename. Leaving the entry out, or appending it after
	// the ledger, made every test using this helper stage a journal production
	// cannot write — the abandon skips these paths rather than deleting them, so
	// it changed no behaviour, which is exactly why it went unnoticed.
	created, isList := doc["created"].([]any)
	if !isList && doc["created"] != nil {
		t.Fatalf("the journal's created list is %T", doc["created"])
	}

	doc["created"] = insertSuperseded(created, superseded, rankOf(superseded))

	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode the journal: %v", err)
	}

	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write the journal: %v", err)
	}
}

// canonicalOf maps a superseded path back to the name it came from.
func canonicalOf(t *testing.T, stateDir, superseded string) string {
	t.Helper()

	ledger := deployarchive.LedgerPath(stateDir)

	for i, sidecar := range deployarchive.LedgerSidecarPaths(ledger) {
		if strings.HasSuffix(superseded, deployarchive.LedgerSidecarPaths("")[i]) {
			return sidecar
		}
	}

	return ledger
}

// THE PROOF COVERS THE LEDGER SET, NOT JUST THE LEDGER.
//
// SQLite REPLAYS a write-ahead log it finds beside a database, so proving
// billet.db is the file this recovery moved aside proves nothing about the
// ledger: a log substituted under the superseded name is moved back alongside
// the correct database — its canonical name is free, so nothing stops it — and
// then replayed into a ledger it was never a journal for. That is the same proxy
// defect one file over, and the only answer is to account for every file in the
// set in both directions: a mismatch and an appearance are equally unexplainable.
func TestAnAbandonRefusesAWriteAheadLogItNeverMovedAside(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	// A LOG UNDER THE SUPERSEDED NAME THAT THE RECOVERY NEVER MOVED THERE. The
	// file alone, with nothing recorded about it — which is the whole difference
	// between this and the log the abandon legitimately puts back.
	if err := os.WriteFile(aside+"-wal", []byte("a journal for somebody else's database"),
		0o600); err != nil {
		t.Fatalf("stage the substituted log: %v", err)
	}

	err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	})
	if err == nil {
		t.Fatal("an abandon put a write-ahead log beside a ledger it was never a journal for")
	}

	// AND IT SAYS WHICH OF THE TWO THIS IS. "Not the file that was moved aside"
	// and "no file of that name was ever moved aside" send an operator to
	// different places — the second means nothing billet did put it there — so
	// falling through to a digest comparison against an empty recorded value
	// would be the right refusal for the wrong reason.
	if !strings.Contains(err.Error(), "a file of that name aside") {
		t.Errorf("the refusal does not say the file is unaccounted for: %v", err)
	}

	// AND IT REFUSED BEFORE MOVING IT, which is the difference between a proof
	// and a post-mortem: the log never reaches the name SQLite would replay it
	// from.
	if _, err := os.Lstat(deployarchive.LedgerPath(f.stateDir) + "-wal"); !os.IsNotExist(err) {
		t.Errorf("the substituted log was put beside the ledger anyway (%v)", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon removed the journal: %v", err)
	}
}

// AND A LOG THAT WAS MOVED ASIDE AND CAME BACK CHANGED IS REFUSED TOO, which is
// the other direction of the same account: recording the set is only half of it
// if the digests are never compared.
func TestAnAbandonRefusesAWriteAheadLogThatChanged(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	supersedeSidecar(t, f.stateDir, aside+"-wal", []byte("what the recovery moved aside"))

	// SWAPPED AFTERWARDS, leaving the journal describing the log that was there
	// and the filesystem holding a different one.
	if err := os.WriteFile(aside+"-wal", []byte("something else entirely"), 0o600); err != nil {
		t.Fatalf("swap the log: %v", err)
	}

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err == nil {
		t.Fatal("an abandon put back a log that is not the one it moved aside")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon lifted the fence: %v", err)
	}
}

// journalDigestOf reads what the supersede recorded for one canonical path.
func journalDigestOf(t *testing.T, stateDir, path string) string {
	t.Helper()

	raw, err := os.ReadFile(deployarchive.JournalPath(stateDir))
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}

	var doc struct {
		SupersededDigests map[string]string `json:"superseded_digests"`
	}

	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the journal does not parse: %v", err)
	}

	return doc.SupersededDigests[filepath.Base(path)]
}

// AN ABANDON ACTS ONLY ON ITS OWN OPERATION'S JOURNAL.
//
// `understood` proves the journal names an operation billet has; it does not
// prove it names THIS one, and the two abandons do different things — a
// restore's never reaches the ledger put-back or the proof that guards it. The
// fence reason usually separates them and cannot be relied on to, because Finish
// takes the fence down one step before removing the journal: a journal that
// outlives its fence would let a restore's abandon act on a recovery's record
// and skip every check that belongs to it.
func TestARestoresAbandonWillNotActOnARecoverysJournal(t *testing.T) {
	stubLifecycleLock(t)

	f := newBackupFixture(t, true)
	archive := backupInto(t, f)

	lease := populateLedger(t, f)
	releaseEverything(t, f, lease)

	stopAfterTheLedger(t, f)

	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	aside := supersededLedgerOrEmpty(f.stateDir)
	if aside == "" {
		t.Fatal("the interruption did not get as far as moving the ledger aside")
	}

	// THE FENCE GONE, THE JOURNAL STILL HERE — the state Finish passes through,
	// and the one where nothing but the journal says whose record this is.
	if err := os.Remove(state.MaintenanceFencePath(f.stateDir)); err != nil {
		t.Fatalf("clear the fence: %v", err)
	}

	err := cmdLocalRestore(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	})
	if err == nil {
		t.Fatal("a restore's abandon acted on a recovery's journal")
	}

	if !strings.Contains(err.Error(), "recover") {
		t.Errorf("the refusal does not name the command that owns the journal: %v", err)
	}

	// AND IT TOUCHED NOTHING: the superseded ledger is where the recovery left
	// it, and the journal is still there for the command that owns it.
	if _, err := os.Lstat(aside); err != nil {
		t.Errorf("the refused abandon moved the superseded ledger: %v", err)
	}

	if _, err := os.Lstat(deployarchive.JournalPath(f.stateDir)); err != nil {
		t.Errorf("the refused abandon removed the journal: %v", err)
	}

	// AND IT LEFT NO FENCE OF ITS OWN, which is the half that decides whether
	// the operator has a way forward at all. Raising a restore fence and then
	// refusing puts the wrong reason over somebody else's record, permanently —
	// a fence over a published journal is deliberately not cleared on a refusal —
	// and the command that owns the journal then cannot fence the directory,
	// because WriteMaintenanceFence will not replace another reason. Two
	// commands, neither able to proceed, produced by the check whose job is
	// sending an operator to the right one.
	switch reason, fenced, err := state.MaintenanceFenceReason(f.stateDir); {
	case err != nil:
		t.Fatalf("MaintenanceFenceReason: %v", err)
	case fenced && reason != deployarchive.RecoverFenceReason:
		t.Errorf("the refused abandon left a %q fence over a recovery's journal", reason)
	}

	// SO THE COMMAND THAT OWNS IT STILL WORKS.
	if err := cmdLocalRecover(t.Context(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("the recovery's own abandon after the refused one: %v", err)
	}

	if _, err := os.Lstat(aside); !os.IsNotExist(err) {
		t.Errorf("the recovery's abandon left the ledger aside (%v)", err)
	}
}

// rankOf is where supersedeLedger records a superseded path: the sidecars in
// LedgerSidecarPaths order, then the ledger.
func rankOf(superseded string) int {
	for i, suffix := range deployarchive.LedgerSidecarPaths("") {
		if strings.HasSuffix(superseded, suffix) {
			return i
		}
	}

	return len(deployarchive.LedgerSidecarPaths(""))
}

// insertSuperseded puts one entry where production would have written it, and
// replaces rather than duplicates when it is already there.
//
// BY RANK RATHER THAN "BEFORE THE LEDGER", because two sidecars staged in the
// wrong order, or one staged twice, produce a Created list supersedeLedger could
// not have written — and a fixture production cannot write is not evidence about
// production. Today's callers all stage a single -wal, so this changes nothing
// they do; it is the helper's general claim that has to be true.
func insertSuperseded(created []any, superseded string, rank int) []any {
	out := make([]any, 0, len(created)+1)
	placed := false

	for _, entry := range created {
		name, isText := entry.(string)
		if isText && name == superseded {
			continue
		}

		if !placed && name != "" && rankOf(name) > rank {
			out = append(out, superseded)
			placed = true
		}

		out = append(out, entry)
	}

	if !placed {
		out = append(out, superseded)
	}

	return out
}
