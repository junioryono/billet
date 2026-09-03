package deployarchive

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// TestALedgerChangedAfterOpenIsNotInstalled proves a snapshot swapped after
// verification never reaches the destination.
//
// Open verified the snapshot by PATHNAME, and publication reopens that pathname.
// On media somebody can change — which is where a backup lives — the bytes
// installed would not be the bytes checked. What has to hold is that the
// destination ends up with the manifest's bytes or with nothing at all.
func TestALedgerChangedAfterOpenIsNotInstalled(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	// Swapped AFTER Open has verified it, and after the plan is built, so only a
	// check at copy time can catch it.
	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	ledger := filepath.Join(dest, EntryLedger)

	if err := os.Remove(ledger); err != nil {
		t.Fatalf("clear the archive's ledger: %v", err)
	}

	if err := os.WriteFile(ledger, []byte("not the ledger that was verified\n"), 0o600); err != nil {
		t.Fatalf("swap the archive's ledger: %v", err)
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a ledger swapped after verification was installed")
	}

	if !strings.Contains(err.Error(), "changed after this backup was verified") {
		t.Errorf("the refusal does not say the archive changed: %v", err)
	}

	// THE ASSERTION IS THE DESTINATION. An error that left the wrong bytes at
	// the target would be the defect, not the fix.
	if _, err := os.Lstat(filepath.Join(tgt.StateDir, "billet.db")); !os.IsNotExist(err) {
		t.Errorf("bytes billet could not vouch for were left at the destination: %v", err)
	}

	// AND NO STAGING FILE EITHER. The copy goes through a temporary beside the
	// destination, and one left behind is a partial ledger nobody accounts for.
	strays := staging(t, tgt.StateDir)
	if len(strays) > 0 {
		t.Errorf("the refused copy left staging files behind: %v", strays)
	}
}

// staging lists billet's own temporary files in a directory.
func staging(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []string

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".billet-") {
			out = append(out, e.Name())
		}
	}

	return out
}

// TestARestoreHoldsTheAuthorityLock proves a restore excludes a concurrent
// rotation or retirement.
//
// Restore WRITES the five files `billet ca rotate` mutates, and rotate and
// retire hold only that lock — so without taking it a `ca retire` alongside a
// restore can delete the ca-previous pair the restore has just published, after
// which the restore reports success and every un-renewed node loses the
// authority it still trusts.
func TestARestoreHoldsTheAuthorityLock(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	if err := os.MkdirAll(tgt.StateDir, 0o700); err != nil {
		t.Fatalf("create the state dir: %v", err)
	}

	held, err := wirecert.LockAuthority(tgt.StateDir)
	if err != nil {
		t.Fatalf("LockAuthority: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a restore ran while the authority lock was held")
	}

	if !strings.Contains(err.Error(), "certificate authority") {
		t.Errorf("the refusal does not name the authority lock: %v", err)
	}

	// AND IT PUBLISHED NOTHING. An exclusion taken after the first write is not
	// an exclusion.
	if _, err := os.Lstat(filepath.Join(tgt.StateDir, "deployment-id")); !os.IsNotExist(err) {
		t.Errorf("a refused restore published the identity anyway: %v", err)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); !os.IsNotExist(err) {
		t.Errorf("a restore refused before it started left the ledger fenced: %v", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// BOTH DIRECTIONS: with the lock free it works, or the assertions above would
	// pass against an Execute that had simply been broken.
	if _, err := execute(t, a, tgt); err != nil {
		t.Fatalf("a restore with the authority lock free: %v", err)
	}
}

// TestAnAbandonHoldsTheAuthorityLock, for the same reason: it REMOVES authority
// files, and a concurrent rotation reading them would see half a generation.
func TestAnAbandonHoldsTheAuthorityLock(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	stopAt(t, 2)

	if _, err := execute(t, a, tgt); !errors.Is(err, errStopHere) {
		t.Fatalf("the staged interruption did not happen: %v", err)
	}

	onPublish = nil

	held, err := wirecert.LockAuthority(tgt.StateDir)
	if err != nil {
		t.Fatalf("LockAuthority: %v", err)
	}

	if _, err := Abandon(t.Context(), a, tgt, RestoreFresh); err == nil {
		t.Error("an abandon ran while the authority lock was held")
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := Abandon(t.Context(), a, tgt, RestoreFresh); err != nil {
		t.Fatalf("an abandon with the authority lock free: %v", err)
	}
}

// TestALeftoverPreviousAuthorityKeyIsRefused is the other half of the leftover
// case: the KEY alone, not the certificate.
func TestALeftoverPreviousAuthorityKeyIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	leftover := wirecert.AuthorityPath(tgt.StateDir, "ca-previous.key")

	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatalf("create the target ca dir: %v", err)
	}

	stale := read(t, wirecert.AuthorityPath(src.stateDir, "ca.key"))

	if err := os.WriteFile(leftover, stale, 0o600); err != nil {
		t.Fatalf("stage the leftover: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a leftover previous authority KEY was not refused")
	}

	if !strings.Contains(refusalText(plan), "ca-previous.key") {
		t.Errorf("the refusal does not name the leftover: %s", refusalText(plan))
	}
}

// TestAWidenedPlanIsRefused proves a plan that grew a deletion after it was
// printed is not executed.
//
// Refusing only on refusals lets the operation widen silently: a preflight
// ledger appearing between the plan and the run turns an Install into a
// ReplaceEmptyLedger, which DELETES a file the person who read the plan approved
// no deletion of.
func TestAWidenedPlanIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	// Planned against a directory with no ledger at all.
	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	for _, act := range plan.Actions {
		if act.Entry == EntryLedger && act.Disposition != Install {
			t.Fatalf("the ledger is planned as %s, so this test stages nothing", act.Disposition)
		}
	}

	// A preflight `billet check` runs in between and leaves one.
	preflight(t, tgt.StateDir, false)

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a plan that widened into a deletion was executed")
	}

	if !strings.Contains(err.Error(), "changed between") {
		t.Errorf("the refusal does not say the target moved: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(tgt.StateDir, "billet.db")); err != nil {
		t.Errorf("the ledger that appeared between planning and running was deleted: %v", err)
	}
}

// TestAStagedLedgerSwappedBeforeTheLinkIsNotPublished is the test the previous
// one could not be.
//
// `TestALedgerChangedAfterOpenIsNotInstalled` passes against the OLD
// copy-straight-to-the-destination implementation too: that code also spotted
// the digest mismatch, also unlinked its uncontended destination, and also left
// no staging file. It proves the digest is checked; it proves nothing about
// verified-BEFORE-publication.
//
// What only the staged shape survives is the temporary being replaced between
// the verification and the link. `onStaged` fires in exactly that window.
func TestAStagedLedgerSwappedBeforeTheLinkIsNotPublished(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	imposter := []byte("a ledger billet never verified\n")

	prev := onStaged
	swapped := ""

	onStaged = func(tmp string) error {
		// Somebody with write access to the directory renames the verified
		// temporary away and leaves their own file at that name.
		if err := os.Remove(tmp); err != nil {
			return err
		}

		if err := os.WriteFile(tmp, imposter, 0o600); err != nil {
			return err
		}

		swapped = tmp

		return nil
	}

	t.Cleanup(func() { onStaged = prev })

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a swapped staging file was published as the ledger")
	}

	if swapped == "" {
		t.Fatal("the fixture never reached the staging window, so it proves nothing")
	}

	if !strings.Contains(err.Error(), "no longer the file billet staged") {
		t.Errorf("the refusal does not say the staged file changed: %v", err)
	}

	// THE IMPOSTER IS NEITHER PUBLISHED...
	ledger := filepath.Join(tgt.StateDir, "billet.db")

	if body, readErr := os.ReadFile(ledger); readErr == nil && bytes.Equal(body, imposter) {
		t.Error("the swapped file was published as the ledger")
	} else if readErr == nil {
		t.Errorf("something was published at %s despite the refusal", ledger)
	}

	// ...NOR DELETED. It is somebody else's file at that name now, and this
	// package removes a staged name only while it still refers to what it wrote.
	body, err := os.ReadFile(swapped)
	if err != nil {
		t.Fatalf("the swapped file was deleted, which is a pathname unlink billet may not do: %v",
			err)
	}

	if !bytes.Equal(body, imposter) {
		t.Error("the swapped file was modified")
	}
}

// TestASuccessfulCopyLeavesNoSecondName proves the staging name is dropped
// after a successful publish.
//
// os.Link leaves TWO names for one inode and the temporary is the one to drop —
// for the App key one credential over, an unreported second copy is exactly
// what nobody finds until it matters. The removal is guarded by an identity
// check against the open descriptor, and that check has an ordering trap:
// f.Stat() on a CLOSED descriptor fails, which the guard reads as "not ours",
// so closing before checking leaves the copy behind on every SUCCESSFUL
// install.
func TestASuccessfulCopyLeavesNoSecondName(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	restoreInto(t, a, tgt)

	if strays := staging(t, tgt.StateDir); len(strays) > 0 {
		t.Errorf("a successful restore left a second copy of the ledger: %v", strays)
	}
}

// TestAFenceIsNotTakenDownOverAHalfRestoredDirectory proves a journal already
// there keeps the fence up whoever raised it.
//
// undoFence exists so a run that published NOTHING does not leave a healthy
// deployment closed. Keyed only on "did this call raise the fence", it also
// fires over a directory an EARLIER run half-published: a journal is there, the
// fence has since gone, a retry raises a new one, the fresh plan refuses — and
// the fence comes down over partial state. A control plane would then start,
// find whichever pieces landed, and MINT the rest, which is the catastrophe the
// whole command exists to prevent.
//
// So the journal is read first, under the locks, and its presence pins the
// fence up regardless of who raised it.
func TestAFenceIsNotTakenDownOverAHalfRestoredDirectory(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	// A run that published some of the unit and stopped.
	stopAt(t, 2)

	if _, err := execute(t, a, tgt); !errors.Is(err, errStopHere) {
		t.Fatalf("the staged interruption did not happen: %v", err)
	}

	onPublish = nil

	if _, err := os.Lstat(JournalPath(tgt.StateDir)); err != nil {
		t.Fatalf("the interrupted run left no journal, so this test stages nothing: %v", err)
	}

	// Its fence goes missing — an operator clearing what they took for a stray
	// file, or a half-finished abandon.
	if err := os.Remove(state.MaintenanceFencePath(tgt.StateDir)); err != nil {
		t.Fatalf("clear the fence: %v", err)
	}

	// The retry refuses, because the identity this run published is now in the
	// way of a plan that expects to install it... so instead make the retry
	// refuse for a reason that arises AFTER the fence is raised: change the
	// target between the plan and the run.
	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("the resumed plan was refused before it could run: %s", refusalText(plan))
	}

	// Somebody removes a piece the plan said was already present, so the locked
	// re-plan disagrees and the run refuses.
	if err := os.Remove(filepath.Join(tgt.StateDir, "deployment-id")); err != nil {
		t.Fatalf("remove the identity: %v", err)
	}

	if _, err := Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	}); err == nil {
		t.Fatal("the locked re-plan did not refuse, so this test stages nothing")
	}

	// THE ASSERTION. The directory is still half-published, so it must still be
	// closed — whoever raised the fence this time round.
	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); err != nil {
		t.Fatalf("the fence was taken down over a half-restored directory: %v", err)
	}

	if _, err := state.Open(t.Context(), tgt.StateDir); !errors.Is(err, state.ErrMaintenance) {
		t.Errorf("a control plane could open a half-restored state directory: %v", err)
	}
}
