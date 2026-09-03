package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// backupInto captures f and returns the archive directory.
func backupInto(t *testing.T, f backupFixture) string {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "backup")

	if err := cmdLocalBackup(t.Context(), []string{"--config", f.configPath, "--out", dest}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	return dest
}

// clearAppKey takes the target's own key out of the way.
//
// The fixture writes a DIFFERENT key at the configured path, which a restore
// correctly refuses to replace — so a test about anything else has to remove it
// first or it is only ever testing that refusal.
func clearAppKey(t *testing.T, f backupFixture) {
	t.Helper()

	if err := os.Remove(f.keyPath); err != nil {
		t.Fatalf("clear the target's App key: %v", err)
	}
}

// TestLocalRestoreRefusesWithoutTheFleetFencingAssertion proves that local restore refuses without the fleet fencing assertion.
//
// NO LOCK ON THIS MACHINE CAN ESTABLISH IT. Restoring here while the controller
// this backup came from can still start produces two authoritative controllers
// on one identity, one authority and one App credential — and the only party
// that can know the old one is stopped everywhere is the operator.
func TestLocalRestoreRefusesWithoutTheFleetFencingAssertion(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	err := cmdLocalRestore(t.Context(),
		[]string{"--config", tgt.configPath, "--from", archive})
	if err == nil {
		t.Fatal("a restore ran without the fencing assertion")
	}

	if !strings.Contains(err.Error(), "--old-controller-fenced") {
		t.Errorf("the refusal does not name the flag that asserts it: %v", err)
	}

	if !strings.Contains(err.Error(), "two authoritative controllers") {
		t.Errorf("the refusal does not say what goes wrong: %v", err)
	}

	// AND NOTHING WAS WRITTEN. The assertion that matters is the state of the
	// host, not the error value.
	if probe := state.ProbeDeploymentID(tgt.stateDir); probe == state.IdentityPresent {
		t.Error("a refused restore installed a deployment identity anyway")
	}

	if _, err := os.Lstat(filepath.Join(tgt.stateDir, "billet.db")); !os.IsNotExist(err) {
		t.Errorf("a refused restore installed a ledger anyway: %v", err)
	}
}

// TestLocalRestoreDryRunChangesNothing, including on a host where the restore
// would be allowed to proceed.
func TestLocalRestoreDryRunChangesNothing(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--dry-run",
		"--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore --dry-run: %v", err)
	}

	if probe := state.ProbeDeploymentID(tgt.stateDir); probe == state.IdentityPresent {
		t.Error("--dry-run installed a deployment identity")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); !os.IsNotExist(err) {
		t.Errorf("--dry-run fenced the ledger: %v", err)
	}
}

// TestLocalRestorePutsTheDeploymentBack, end to end through both commands.
//
// THE APP KEY GOES THROUGH THE REAL INSTALLER. reserveKeyFile and
// writeKeyAtomically are what `billet github-app create` uses, and this is the
// path that proves a restore reuses them rather than growing a second
// implementation.
func TestLocalRestorePutsTheDeploymentBack(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v", err)
	}

	id, found, err := state.PeekDeploymentID(tgt.stateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.deployment {
		t.Errorf("restored deployment %s, want %s", id, src.deployment)
	}

	key, err := os.ReadFile(tgt.keyPath)
	if err != nil {
		t.Fatalf("read the restored App key: %v", err)
	}

	if !bytes.Equal(key, src.appKey) {
		t.Error("the restored App key is not the one in the backup")
	}

	for _, name := range []string{"ca.key", "ca.crt", "authority-created"} {
		want, err := os.ReadFile(wirecert.AuthorityPath(src.stateDir, name))
		if err != nil {
			t.Fatalf("read the source %s: %v", name, err)
		}

		got, err := os.ReadFile(wirecert.AuthorityPath(tgt.stateDir, name))
		if err != nil {
			t.Fatalf("read the restored %s: %v", name, err)
		}

		if !bytes.Equal(got, want) {
			t.Errorf("restored %s is not the one in the backup", name)
		}
	}

	// THE STAGING FILE MUST BE GONE. os.Link leaves two names for one private
	// key, and an unreported second copy of an App key is what nobody finds
	// until it matters.
	if _, err := os.Lstat(stagingPath(tgt.keyPath)); !os.IsNotExist(err) {
		t.Errorf("a second copy of the App key was left at %s (%v)",
			stagingPath(tgt.keyPath), err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); !os.IsNotExist(err) {
		t.Errorf("a finished restore left the ledger fenced: %v", err)
	}

	// And the restored deployment opens as a control plane.
	db, err := state.Open(t.Context(), tgt.stateDir)
	if err != nil {
		t.Fatalf("the restored deployment does not open: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLocalRestoreWillNotReplaceADifferentAppKey, and the key stays exactly as
// it was.
func TestLocalRestoreWillNotReplaceADifferentAppKey(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	// The fixture writes its own, different key at the configured path.
	tgt := newBackupFixture(t, false)

	before, err := os.ReadFile(tgt.keyPath)
	if err != nil {
		t.Fatalf("read the target's App key: %v", err)
	}

	// A DISTINCT NAME FOR THE REFUSAL. Reusing err here is how a test comes to
	// call Error() on the nil a later ReadFile assigned, which panics — and a
	// panicking test looks exactly like a failing assertion.
	refusal := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	})
	if refusal == nil {
		t.Fatal("a restore over a different App key succeeded")
	}

	after, err := os.ReadFile(tgt.keyPath)
	if err != nil {
		t.Fatalf("read the target's App key after: %v", err)
	}

	if !bytes.Equal(after, before) {
		t.Error("the App key already on this host was modified")
	}

	if !strings.Contains(refusal.Error(), "App private key") {
		t.Errorf("the refusal does not name the App key: %v", refusal)
	}
}

// makeStateDir creates the directory a fence needs to live in. An uncommissioned
// host has a config naming one and nothing there yet.
func makeStateDir(t *testing.T, f backupFixture) {
	t.Helper()

	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		t.Fatalf("create the target state dir: %v", err)
	}
}

// TestAbandonClearsAFenceLeftWithNoJournal proves --abandon takes down a fence
// that has no journal beside it.
//
// The journal is written just AFTER the fence, so a run that failed at the
// writer barrier leaves the fence standing alone. Reporting "nothing to abandon"
// there would leave the directory closed to every billet with nothing on the
// host explaining why.
func TestAbandonClearsAFenceLeftWithNoJournal(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)
	makeStateDir(t, tgt)

	if _, err := state.WriteMaintenanceFence(tgt.stateDir, deployarchive.FenceReason); err != nil {
		t.Fatalf("stage the fence: %v", err)
	}

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("billet local restore --abandon: %v", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); !os.IsNotExist(err) {
		t.Errorf("--abandon left a fence it established: %v", err)
	}
}

// TestAbandonLeavesSomebodyElsesFenceAlone proves --abandon refuses a fence it
// did not establish.
//
// A host upgrade's fence is not a restore's to clear, for the same reason an
// operator's admission seal is not a lifecycle command's.
func TestAbandonLeavesSomebodyElsesFenceAlone(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)
	makeStateDir(t, tgt)

	if _, err := state.WriteMaintenanceFence(tgt.stateDir, "host upgrade"); err != nil {
		t.Fatalf("stage somebody else's fence: %v", err)
	}

	err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--abandon",
	})
	if err == nil {
		t.Fatal("--abandon cleared a host upgrade's fence")
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); err != nil {
		t.Errorf("the other fence was removed: %v", err)
	}
}

// TestASecondRestoreConfirmsUnderTheLockRatherThanReportingFromAStalePlan
// proves a no-op answer is established under the lock rather than inferred.
//
// THE MUTATION HAPPENS AFTER THE PLAN AND BEFORE THE LOCK, which is the only
// placement that proves anything. An earlier version of this test changed the
// App key before the command started — so the command's own unlocked planner
// refused it, and the test passed with the locked recheck deleted. The
// lifecycle lock is taken after the plan is built and printed and before
// Execute runs, so its seam is exactly the window a real operator command
// leaves open.
func TestASecondRestoreConfirmsUnderTheLockRatherThanReportingFromAStalePlan(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v", err)
	}

	// Everything is in place, so the next run's plan has nothing to do — and
	// that answer must be established under the lock, not inferred.
	other := newBackupFixture(t, false)

	imposter, err := os.ReadFile(other.keyPath)
	if err != nil {
		t.Fatalf("read a different App key: %v", err)
	}

	prev := lifecycleLock
	swapped := false

	lifecycleLock = func() (*hostLock, error) {
		// The plan is built and printed by now; Execute has not run.
		if !swapped {
			if err := os.Remove(tgt.keyPath); err != nil {
				return nil, err
			}

			if err := os.WriteFile(tgt.keyPath, imposter, 0o600); err != nil {
				return nil, err
			}

			swapped = true
		}

		return prev()
	}

	t.Cleanup(func() { lifecycleLock = prev })

	err = cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	})

	if !swapped {
		t.Fatal("the fixture never reached the lock, so it staged nothing")
	}

	if err == nil {
		t.Fatal("a restore reported success on a plan taken before the App key was replaced")
	}

	// AND THE REPLACEMENT IS UNTOUCHED. It is somebody else's credential now.
	got, readErr := os.ReadFile(tgt.keyPath)
	if readErr != nil {
		t.Fatalf("read the App key after: %v", readErr)
	}

	if !bytes.Equal(got, imposter) {
		t.Error("the App key that arrived between planning and the lock was modified")
	}

	// AND THE DEPLOYMENT IS NOT LEFT FENCED. Nothing was published, so a run
	// that changed nothing must not take a healthy control plane offline.
	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); !os.IsNotExist(err) {
		t.Errorf("a restore that published nothing left the ledger fenced: %v", err)
	}
}

// TestANoOpRestoreThatRefusesLeavesTheDeploymentUnfenced proves a run that
// published nothing does not take a healthy control plane offline.
//
// A no-op now goes through the executor, which raises the fence before the
// barrier and the locked recheck. Every one of those can refuse — and each of
// them leaves the deployment exactly as it was, so a fence left standing would
// take a healthy control plane offline over an operation that did nothing.
func TestANoOpRestoreThatRefusesLeavesTheDeploymentUnfenced(t *testing.T) {
	stubLifecycleLock(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v", err)
	}

	// The deployment is complete and healthy. Break something between the plan
	// and the lock so the locked recheck refuses.
	prev := lifecycleLock

	lifecycleLock = func() (*hostLock, error) {
		if err := os.Remove(filepath.Join(tgt.stateDir, "deployment-id")); err != nil &&
			!os.IsNotExist(err) {
			return nil, err
		}

		return prev()
	}

	t.Cleanup(func() { lifecycleLock = prev })

	// IT MUST REFUSE, not merely "either succeed or fail". Tolerating both lets
	// this pass for the wrong reason: if the locked recheck stopped noticing the
	// staged change, the restore would succeed, its ordinary teardown would clear
	// the fence, and every assertion below would still hold.
	err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	})
	if err == nil {
		t.Fatal("the locked recheck did not notice the identity removed after planning")
	}

	if !strings.Contains(err.Error(), "changed between") {
		t.Errorf("the refusal does not say the target moved: %v", err)
	}

	// AND THE LEDGER MUST BE OPEN. The control plane on this host was fine before
	// the command ran and nothing was published.
	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.stateDir)); !os.IsNotExist(err) {
		t.Errorf("a restore that published nothing left the ledger fenced: %v", err)
	}

	db, err := state.Open(t.Context(), tgt.stateDir)
	if err != nil {
		t.Fatalf("the control plane can no longer open its own state directory: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLocalRestoreNeedsAFromDirectory proves that local restore needs a from directory.
func TestLocalRestoreNeedsAFromDirectory(t *testing.T) {
	f := newBackupFixture(t, false)

	err := cmdLocalRestore(t.Context(), []string{"--config", f.configPath})
	if err == nil || !strings.Contains(err.Error(), "--from") {
		t.Errorf("a restore with no source was not refused for that: %v", err)
	}
}

// TestLocalUsageNamesBackupAndRestore, so an operator who types `billet local`
// discovers them.
func TestLocalUsageNamesBackupAndRestore(t *testing.T) {
	err := cmdLocal(t.Context(), nil)
	if err == nil {
		t.Fatal("`billet local` with no subcommand succeeded")
	}

	for _, want := range []string{"backup", "restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the usage line does not mention %s: %v", want, err)
		}
	}

	err = cmdLocal(t.Context(), []string{"backyp"})
	if err == nil {
		t.Fatal("an unknown local subcommand succeeded")
	}

	for _, want := range []string{"backup", "restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unknown-command hint does not mention %s: %v", want, err)
		}
	}
}
