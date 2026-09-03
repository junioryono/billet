package deployarchive

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// errStopHere is the interruption a test stages.
var errStopHere = errors.New("stopped here on purpose")

// stopAt makes the next Execute stop before publishing the item at index n.
//
// BEFORE THE JOURNAL NOTE, so stopping at n models a crash that finished item
// n-1 completely — which is the boundary an operator's power cut lands on.
func stopAt(t *testing.T, n int) {
	t.Helper()

	prev := onPublish
	seen := 0

	onPublish = func(Action) error {
		if seen == n {
			return errStopHere
		}

		seen++

		return nil
	}

	t.Cleanup(func() { onPublish = prev })
}

// execute runs one restore pass without asserting it succeeded.
func execute(t *testing.T, a *Archive, tgt Target) (Result, error) {
	t.Helper()

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("PlanRestore refused: %s", refusalText(plan))
	}

	return Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
		Actor:         "test",
	})
}

// publicationSteps is how many items a restore into an empty directory
// publishes, so the interruption table covers every boundary rather than a
// number somebody guessed.
func publicationSteps(t *testing.T, a *Archive, tgt Target) int {
	t.Helper()

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	n := 0

	for _, act := range plan.Actions {
		if act.Disposition != AlreadyPresent {
			n++
		}
	}

	return n
}

// TestARestoreInterruptedAtEveryBoundaryResumesToTheSameResult proves that a restore interrupted at every boundary resumes to the same result.
//
// THE ASSERTION IS THE RESULT, NOT THAT IT SURVIVED. A resume that installed
// nothing, or that installed a different set of bytes, would also "not crash" —
// so every pass is compared against the same deployment the uninterrupted path
// produces, and the fence is checked while the restore is still unfinished.
func TestARestoreInterruptedAtEveryBoundaryResumesToTheSameResult(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	probe := newTarget(t, src.github)
	steps := publicationSteps(t, a, probe)

	if steps < 5 {
		t.Fatalf("a restore publishes only %d items; the table below would prove very little",
			steps)
	}

	for stop := range steps {
		t.Run(strings.ReplaceAll(itoa(stop), " ", ""), func(t *testing.T) {
			tgt := newTarget(t, src.github)

			stopAt(t, stop)

			_, err := execute(t, a, tgt)
			if !errors.Is(err, errStopHere) {
				t.Fatalf("the staged interruption did not happen: %v", err)
			}

			// THE HOST IS STILL FENCED. A half-restored directory holds some of
			// the pieces that make it this deployment and not the others, so a
			// control plane starting on it would mint whatever is missing.
			if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); err != nil {
				t.Fatalf("an interrupted restore left the ledger unfenced: %v", err)
			}

			if _, err := state.Open(t.Context(), tgt.StateDir); !errors.Is(err, state.ErrMaintenance) {
				t.Errorf("a control plane could open a half-restored state directory: %v", err)
			}

			if _, err := os.Lstat(JournalPath(tgt.StateDir)); err != nil && stop > 0 {
				t.Errorf("an interrupted restore left no journal: %v", err)
			}

			// Resume.
			onPublish = nil

			res, err := execute(t, a, tgt)
			if err != nil {
				t.Fatalf("the resumed restore failed: %v", err)
			}

			if stop > 0 && !res.Resumed {
				t.Error("the resumed restore did not report picking up a journal")
			}

			assertRestored(t, src, tgt)
		})
	}
}

// assertRestored proves a target holds exactly the deployment the archive
// carries.
func assertRestored(t *testing.T, src deployment, tgt Target) {
	t.Helper()

	id, found, err := state.PeekDeploymentID(tgt.StateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.id {
		t.Errorf("deployment id = %s, want %s", id, src.id)
	}

	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, src.appKey) {
		t.Error("the App key is not the one in the backup")
	}

	for _, name := range []string{"ca.key", "ca.crt", "authority-created"} {
		want := read(t, wirecert.AuthorityPath(src.stateDir, name))
		if got := read(t, wirecert.AuthorityPath(tgt.StateDir, name)); !bytes.Equal(got, want) {
			t.Errorf("%s is not the one in the backup", name)
		}
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); !os.IsNotExist(err) {
		t.Errorf("a finished restore left the ledger fenced: %v", err)
	}

	db, err := state.Open(t.Context(), tgt.StateDir)
	if err != nil {
		t.Fatalf("the restored deployment does not open: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAbandonRemovesOnlyWhatThisRunCreated, at every boundary.
func TestAbandonRemovesOnlyWhatThisRunCreated(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	probe := newTarget(t, src.github)
	steps := publicationSteps(t, a, probe)

	for stop := 1; stop < steps; stop++ {
		t.Run(itoa(stop), func(t *testing.T) {
			tgt := newTarget(t, src.github)

			// A file this restore did NOT create, to prove an abandon leaves it.
			bystander := filepath.Join(tgt.StateDir, "operator-note.txt")
			if err := os.WriteFile(bystander, []byte("mine\n"), 0o600); err != nil {
				t.Fatalf("plant a bystander: %v", err)
			}

			stopAt(t, stop)

			if _, err := execute(t, a, tgt); !errors.Is(err, errStopHere) {
				t.Fatalf("the staged interruption did not happen: %v", err)
			}

			onPublish = nil

			res, err := Abandon(t.Context(), a, tgt, RestoreFresh)
			if err != nil {
				t.Fatalf("Abandon: %v", err)
			}

			if len(res.Removed) == 0 {
				t.Error("an abandon after real work removed nothing")
			}

			if len(res.Kept) > 0 {
				t.Errorf("an abandon of this run's own files kept some: %v", res.Kept)
			}

			for _, path := range res.Removed {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("%s was reported removed and is still there (%v)", path, err)
				}
			}

			if _, err := os.Lstat(bystander); err != nil {
				t.Errorf("an abandon removed a file this restore never created: %v", err)
			}

			if _, err := os.Lstat(JournalPath(tgt.StateDir)); !os.IsNotExist(err) {
				t.Errorf("an abandon left its journal behind: %v", err)
			}

			if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); !os.IsNotExist(err) {
				t.Errorf("an abandon left the ledger fenced: %v", err)
			}
		})
	}
}

// TestARestoreInterruptedAfterItsLastInstallStillFinishes proves a resume whose
// plan has nothing left to install still finishes the restore.
//
// NOTHING TO INSTALL IS NOT NOTHING TO DO, and that is the trap this closes. A
// run stopped between the last publication and the teardown has every piece in
// place and is still unfinished: the fence is up and the journal is there, so no
// control plane can start on it. The resume's plan is entirely AlreadyPresent
// and must clear both marks anyway.
func TestARestoreInterruptedAfterItsLastInstallStillFinishes(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	prev := onPublished
	onPublished = func() error { return errStopHere }

	t.Cleanup(func() { onPublished = prev })

	res, err := execute(t, a, tgt)
	if !errors.Is(err, errStopHere) {
		t.Fatalf("the staged late failure did not happen: %v", err)
	}

	if len(res.Installed) == 0 {
		t.Fatal("nothing was installed before the staged failure")
	}

	onPublished = nil

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); err != nil {
		t.Fatalf("the interrupted run left the ledger unfenced: %v", err)
	}

	resume, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore (resume): %v", err)
	}

	// THE PRECONDITION THIS TEST IS NAMED FOR. If the resumed plan still had work
	// to do, it would finish for the ordinary reason and prove nothing.
	if !resume.Nothing() {
		for _, act := range resume.Actions {
			if act.Disposition != AlreadyPresent {
				t.Errorf("the resumed plan would still %s %s", act.Disposition, act.Path)
			}
		}

		t.Fatal("this test is not staging the case it is named for")
	}

	if _, err := Execute(t.Context(), RestoreRequest{
		Plan:          resume,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	}); err != nil {
		t.Fatalf("the resumed restore failed: %v", err)
	}

	assertRestored(t, src, tgt)
}

// TestAbandonKeepsAFileThatIsNoLongerTheOneThisRestoreWrote proves that abandon keeps a file that is no longer the one this restore wrote.
//
// THE SHARPEST CASE IS THE APP KEY. GitHub issues it once, so the only key an
// abandon may delete is one it can prove is a duplicate of the copy still
// sitting in the archive.
func TestAbandonKeepsAFileThatIsNoLongerTheOneThisRestoreWrote(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	steps := publicationSteps(t, a, tgt)

	// Stop on the last item, so everything before it — including the App key on
	// most orderings — is already published.
	stopAt(t, steps-1)

	if _, err := execute(t, a, tgt); !errors.Is(err, errStopHere) {
		t.Fatalf("the staged interruption did not happen: %v", err)
	}

	onPublish = nil

	// Somebody replaces the identity this restore installed.
	idPath := filepath.Join(tgt.StateDir, "deployment-id")
	if err := os.WriteFile(idPath, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("replace the identity: %v", err)
	}

	res, err := Abandon(t.Context(), a, tgt, RestoreFresh)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	var kept bool

	for _, path := range res.Kept {
		if path == idPath {
			kept = true
		}
	}

	if !kept {
		t.Errorf("an abandon removed a file that was no longer the one it wrote; kept = %v",
			res.Kept)
	}

	if _, err := os.Lstat(idPath); err != nil {
		t.Errorf("the replaced identity was deleted: %v", err)
	}
}

// TestAbandonToleratesAPathRecordedButNeverCreated proves that abandon tolerates a path recorded but never created.
//
// The journal records a path BEFORE creating it, so a crash between the two
// leaves an entry with no file. That direction is deliberate — the other order
// leaves a credential nothing knows about — and it has to be survivable.
func TestAbandonToleratesAPathRecordedButNeverCreated(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	j := &journal{
		Schema:       journalSchema,
		Intent:       RestoreFresh.String(),
		ArchiveDir:   a.Dir,
		ManifestSHA:  a.manifestDigest(),
		DeploymentID: a.Manifest.DeploymentID,
		StartedAt:    "2026-01-01T00:00:00Z",
		Created:      []string{filepath.Join(tgt.StateDir, "deployment-id")},
	}

	if err := j.save(tgt.StateDir); err != nil {
		t.Fatalf("save the journal: %v", err)
	}

	if _, err := state.WriteMaintenanceFence(tgt.StateDir, FenceReason); err != nil {
		t.Fatalf("fence: %v", err)
	}

	res, err := Abandon(t.Context(), a, tgt, RestoreFresh)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if len(res.Kept) != 0 {
		t.Errorf("a path that was never created was reported kept: %v", res.Kept)
	}

	// NOR REMOVED. The command PRINTS this list, and naming a file that never
	// existed as one this run deleted is a false report about a credential
	// directory — which is exactly what a two-valued answer produced.
	if len(res.Removed) != 0 {
		t.Errorf("a path that was never created was reported removed: %v", res.Removed)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); !os.IsNotExist(err) {
		t.Errorf("the abandon left the ledger fenced: %v", err)
	}
}

// TestASecondDifferentBackupCannotBeInterleaved proves that a second different backup cannot be interleaved.
//
// Half of one backup beside half of another is not a deployment, and each half
// verifies perfectly on its own.
func TestASecondDifferentBackupCannotBeInterleaved(t *testing.T) {
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

	// A different deployment's backup, restored against the same target.
	otherSrc := newDeployment(t)
	otherSrc.github = src.github

	otherDest := filepath.Join(t.TempDir(), "other-backup")

	backupTo(t, otherSrc, otherDest)

	other, err := Open(t.Context(), otherDest)
	if err != nil {
		t.Fatalf("Open the other backup: %v", err)
	}

	plan, err := PlanRestore(t.Context(), other, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	// The identity already installed belongs to the first backup, so the planner
	// refuses before the journal is even consulted — which is the outer of the
	// two guards and the one an operator meets first.
	if len(plan.Refusals) == 0 {
		t.Fatal("restoring a different deployment over a half-restored one was not refused")
	}

	if !strings.Contains(refusalText(plan), src.id) {
		t.Errorf("the refusal does not name the deployment already half-restored here: %s",
			refusalText(plan))
	}
}

// TestTheJournalRefusesAnArchiveItDoesNotDescribe drives the inner guard
// directly: same identity, different archive.
func TestTheJournalRefusesAnArchiveItDoesNotDescribe(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	stopAt(t, 1)

	if _, err := execute(t, a, tgt); !errors.Is(err, errStopHere) {
		t.Fatalf("the staged interruption did not happen: %v", err)
	}

	onPublish = nil

	// A SECOND backup of the SAME deployment, taken separately. Every file in it
	// is identical except the ledger snapshot, which VACUUM INTO repacks — so
	// the manifest digest differs and the journal must notice.
	second := filepath.Join(t.TempDir(), "second-backup")

	backupToAt(t, src, second, nowStub().Add(time.Hour))

	b, err := Open(t.Context(), second)
	if err != nil {
		t.Fatalf("Open the second backup: %v", err)
	}

	// NOT A SKIP IF THEY MATCH. Two backups of an unchanged deployment really are
	// byte-identical unless the clock moves, and a test that quietly skipped when
	// they were would prove nothing on exactly the run where it mattered.
	if b.manifestDigest() == a.manifestDigest() {
		t.Fatal("the two backups have the same manifest digest, so this test cannot stage the " +
			"case it is named for")
	}

	plan, err := PlanRestore(t.Context(), b, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("the planner refused a same-deployment backup: %s", refusalText(plan))
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           func() time.Time { return time.Unix(1_700_000_200, 0).UTC() },
	})

	if err == nil {
		t.Fatal("a restore from a different archive continued an existing journal")
	}

	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("the refusal does not say a restore is already in progress: %v", err)
	}
}

// itoa keeps the subtest names short without pulling in strconv for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var b []byte

	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}

	return "stop-after-" + string(b)
}
