package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheFenceReachesAHandleThatIsAlreadyOpen proves that the fence reaches a handle that is already open.
//
// THIS IS THE WHOLE REASON THE FENCE EXISTS BESIDE THE DIRECTORY LOCK. An
// operator command opens through OpenAdmin deliberately WITHOUT that lock, so
// taking the lock proves nothing about a handle somebody is already holding.
// The fence is a file both Tx and View consult on entry, which is what reaches
// one.
//
// IT DRIVES THE REAL HANDLE rather than a helper: the property is that an
// already-open writer stops being able to commit, and only that handle can show
// it.
func TestTheFenceReachesAHandleThatIsAlreadyOpen(t *testing.T) {
	dir := t.TempDir()

	plane, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	admin, err := OpenAdmin(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// It works before the fence, or the assertion after it proves nothing.
	if err := admin.View(t.Context(), func(Querier) error { return nil }); err != nil {
		t.Fatalf("the admin handle could not read before the fence: %v", err)
	}

	if _, err := WriteMaintenanceFence(dir, "billet local restore"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	if err := admin.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrMaintenance) {
		t.Errorf("an already-open writer committed through the fence: %v", err)
	}

	if err := admin.View(t.Context(), func(Querier) error { return nil }); !errors.Is(err, ErrMaintenance) {
		t.Errorf("an already-open reader read through the fence: %v", err)
	}

	if _, err := OpenAdmin(t.Context(), dir); !errors.Is(err, ErrMaintenance) {
		t.Errorf("a new handle opened through the fence: %v", err)
	}
}

// TestAFenceIsNotReplacedOrClearedBySomebodyElse proves that a fence is not replaced or cleared by somebody else.
//
// The same provenance argument admission makes one layer up: clearing a fence
// somebody else established reopens a ledger in the middle of their operation,
// and the evidence is a write landing during a window that was supposed to be
// closed.
func TestAFenceIsNotReplacedOrClearedBySomebodyElse(t *testing.T) {
	dir := t.TempDir()

	created, err := WriteMaintenanceFence(dir, "host upgrade")
	if err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	if !created {
		t.Error("the call that established the fence does not report having created it")
	}

	// Idempotent on its OWN reason, so a resumed operation is not blocked by its
	// own fence — AND it must not claim to have created one. A caller clears
	// only a fence it raised, so a second call reporting true would let a
	// resumed operation take down a fence its predecessor is relying on.
	created, err = WriteMaintenanceFence(dir, "host upgrade")
	if err != nil {
		t.Errorf("re-establishing the same fence failed: %v", err)
	}

	if created {
		t.Error("re-establishing an existing fence claims to have created it")
	}

	_, err = WriteMaintenanceFence(dir, "billet local restore")
	if err == nil {
		t.Fatal("a restore replaced a host upgrade's fence")
	}

	if !strings.Contains(err.Error(), "host upgrade") {
		t.Errorf("the refusal does not name whose fence it is: %v", err)
	}

	if err := ClearMaintenanceFence(dir, "billet local restore"); err == nil {
		t.Fatal("a restore cleared a host upgrade's fence")
	}

	if _, err := os.Lstat(MaintenanceFencePath(dir)); err != nil {
		t.Errorf("the fence was removed by a caller that did not establish it: %v", err)
	}

	if err := ClearMaintenanceFence(dir, "host upgrade"); err != nil {
		t.Fatalf("the fence's own owner could not clear it: %v", err)
	}

	if _, err := os.Lstat(MaintenanceFencePath(dir)); !os.IsNotExist(err) {
		t.Errorf("clearing the fence left it in place: %v", err)
	}
}

// TestAFenceMustSayWhatItIsFor, because whoever finds a ledger closed has only
// that to go on.
func TestAFenceMustSayWhatItIsFor(t *testing.T) {
	dir := t.TempDir()

	if _, err := WriteMaintenanceFence(dir, "   "); err == nil {
		t.Error("a fence with no reason was accepted")
	}

	if _, err := os.Lstat(MaintenanceFencePath(dir)); !os.IsNotExist(err) {
		t.Errorf("a refused fence was written anyway: %v", err)
	}
}

// TestTheWriterBarrierWaitsForATransactionThatBeganBeforeTheFence proves that the writer barrier waits for a transaction that began before the fence.
//
// THE FENCE ALONE IS NOT ENOUGH and this is the test that says so: a handle that
// got past the fence check a moment earlier is still free to commit, so a
// restore has to take the write lock once to prove nobody is mid-write.
func TestTheWriterBarrierWaitsForATransactionThatBeganBeforeTheFence(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// A writer holding the lock, started before any fence exists.
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- db.Tx(t.Context(), func(*sql.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held

	prev := writerBarrierLimit
	writerBarrierLimit = 250 * time.Millisecond

	t.Cleanup(func() { writerBarrierLimit = prev })

	if err := WriterBarrier(t.Context(), dir); err == nil {
		t.Error("the barrier passed while another transaction held the write lock")
	}

	close(release)

	if err := <-done; err != nil {
		t.Fatalf("the held transaction failed: %v", err)
	}

	// AND IT PASSES ONCE THAT TRANSACTION IS DONE — without which a barrier that
	// always failed would satisfy the assertion above.
	if err := WriterBarrier(t.Context(), dir); err != nil {
		t.Errorf("the barrier failed against a quiet ledger: %v", err)
	}
}

// TestTheWriterBarrierWillNotCreateALedger. A restore asks this about a
// directory before deciding whether a ledger is already there.
func TestTheWriterBarrierWillNotCreateALedger(t *testing.T) {
	dir := t.TempDir()

	if err := WriterBarrier(t.Context(), dir); err != nil {
		t.Fatalf("the barrier failed against an empty directory: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "billet.db")); !os.IsNotExist(err) {
		t.Errorf("the barrier created a ledger: %v", err)
	}
}

// TestLockStateDirExcludesAControlPlane, in both directions.
func TestLockStateDirExcludesAControlPlane(t *testing.T) {
	dir := t.TempDir()

	lock, err := LockStateDir(dir)
	if err != nil {
		t.Fatalf("LockStateDir: %v", err)
	}

	if _, err := Open(t.Context(), dir); !errors.Is(err, ErrLocked) {
		t.Errorf("a control plane opened a directory a restore was holding: %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// AND IT LETS GO. Without this, a lock that never released would satisfy the
	// assertion above and wedge every later operation on the host.
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("a control plane could not open the directory after the lock was released: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMaintenanceFenceReasonAnswersThreeStates proves that reading a fence tells
// "present, for this" from "absent" from "billet could not tell".
//
// THE THIRD STATE IS THE POINT. A caller uses this to decide whether a directory
// is already part-way through ITS OWN operation and may therefore skip work that
// makes the operation safe, so an unreadable fence collapsing into "there is no
// fence" is the could-not-tell/no collapse that hands out exactly the wrong
// answer.
func TestMaintenanceFenceReasonAnswersThreeStates(t *testing.T) {
	dir := t.TempDir()

	switch reason, fenced, err := MaintenanceFenceReason(dir); {
	case err != nil:
		t.Fatalf("an unfenced directory reported an error: %v", err)
	case fenced:
		t.Errorf("an unfenced directory reported a fence for %q", reason)
	}

	if _, err := WriteMaintenanceFence(dir, "billet local recover"); err != nil {
		t.Fatalf("WriteMaintenanceFence: %v", err)
	}

	switch reason, fenced, err := MaintenanceFenceReason(dir); {
	case err != nil:
		t.Fatalf("MaintenanceFenceReason: %v", err)
	case !fenced:
		t.Error("a fenced directory reported no fence")
	case reason != "billet local recover":
		t.Errorf("fence reason = %q, want %q", reason, "billet local recover")
	}

	// A directory where the fence's name is taken by something billet cannot
	// read. os.ReadFile of a directory fails on both platforms billet supports.
	other := t.TempDir()
	if err := os.Mkdir(MaintenanceFencePath(other), 0o700); err != nil {
		t.Fatalf("stage an unreadable fence: %v", err)
	}

	switch _, fenced, err := MaintenanceFenceReason(other); {
	case err == nil:
		t.Errorf("an unreadable fence was reported as fenced=%v with no error", fenced)
	case fenced:
		t.Error("an unreadable fence was reported as a fence rather than as an error")
	}
}
