package state

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// watermarkOf reads the mark through a handle that names no release, so the read
// itself can neither refuse nor record.
func watermarkOf(t *testing.T, dir string) string {
	t.Helper()

	db, err := OpenAdmin(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenAdmin to read the watermark: %v", err)
	}

	defer func() { _ = db.Close() }()

	release, _, err := db.ReleaseWatermark(t.Context())
	if err != nil {
		t.Fatalf("ReleaseWatermark: %v", err)
	}

	return release
}

// serveAs opens the ledger as a control plane running one release, claims the
// deployment as the control plane does, and closes it again. The claim is what
// records the mark; an open alone records nothing.
func serveAs(t *testing.T, dir, release string) {
	t.Helper()

	db, err := Open(t.Context(), dir, WithRunningRelease(release))
	if err != nil {
		t.Fatalf("Open as %s: %v", release, err)
	}

	if _, err := db.ClaimController(t.Context(), "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController as %s: %v", release, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A PROVED DOWNGRADE IS REFUSED BY EVERY KIND OF OPEN. The schema check catches
// only a pair of releases that differ in a migration; this is what catches the
// rest, and it has to hold for an operator command and for the upgrade probe as
// well as for the control plane, because each of them writes.
func TestAnOlderReleaseIsRefusedByEveryOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")

	if _, err := Open(t.Context(), dir, WithRunningRelease("v0.4.9")); !errors.Is(err,
		ErrReleaseBehind) {
		t.Fatalf("a control plane on v0.4.9 opened a ledger v0.5.0 served: err = %v", err)
	}

	if _, err := OpenAdmin(t.Context(), dir, WithRunningRelease("v0.4.9")); !errors.Is(err,
		ErrReleaseBehind) {
		t.Fatalf("an operator command on v0.4.9 opened a ledger v0.5.0 served: err = %v", err)
	}

	if _, err := OpenMaintenance(t.Context(), dir, WithRunningRelease("v0.4.9")); !errors.Is(err,
		ErrReleaseBehind) {
		t.Fatalf("the upgrade probe on v0.4.9 opened a ledger v0.5.0 served: err = %v", err)
	}

	// AND THE REFUSAL NAMES BOTH RELEASES AND THE WAY THROUGH, because a bare
	// "cannot open" sends an operator to the database.
	_, err := Open(t.Context(), dir, WithRunningRelease("v0.4.9"))
	for _, want := range []string{"v0.5.0", "v0.4.9", "--allow-downgrade"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Errorf("the refused opens moved the watermark to %q", got)
	}
}

// ONLY THE CONTROL PLANE RECORDS. A `billet check` run from a laptop carrying a
// newer binary must not fence the running server out of its own next restart,
// and the probe's whole point is to leave the recording to the service that then
// serves.
func TestOnlyTheControlPlaneRecordsAnUpgrade(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")

	admin, err := OpenAdmin(t.Context(), dir, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("OpenAdmin as a newer binary: %v", err)
	}

	_ = admin.Close()

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("an operator command recorded %q; only the control plane may", got)
	}

	probe, err := OpenMaintenance(t.Context(), dir, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("OpenMaintenance as a newer binary: %v", err)
	}

	_ = probe.Close()

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("the upgrade probe recorded %q; only the control plane may", got)
	}

	// THE OLDER SERVER STILL RESTARTS, which is the whole reason the two above
	// record nothing.
	serveAs(t, dir, "v0.5.0")

	serveAs(t, dir, "v0.6.0")

	if got := watermarkOf(t, dir); got != "v0.6.0" {
		t.Fatalf("the control plane on v0.6.0 left the watermark at %q", got)
	}
}

// A RELEASE THAT CANNOT BE ORDERED NEITHER REFUSES NOR RECORDS. A developer's
// build reports "(devel)", which is not older than anything; refusing it would
// stop every `go run` against a real ledger, and recording it would make every
// later comparison "could not tell" for as long as the row lived.
func TestAnUncomparableReleaseNeitherRefusesNorRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")
	serveAs(t, dir, "(devel)")

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("a development build moved the watermark to %q", got)
	}

	fresh := t.TempDir()

	serveAs(t, fresh, "(devel)")

	if got := watermarkOf(t, fresh); got != "" {
		t.Fatalf("a development build recorded %q on a fresh ledger", got)
	}

	// AND A CALLER THAT NAMES NOTHING GETS NOTHING, which is what every test that
	// opens a throwaway ledger relies on.
	serveAs(t, dir, "")

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("an open naming no release moved the watermark to %q", got)
	}
}

// A DELIBERATE DOWNGRADE LOWERS THE MARK THROUGH THE MAINTENANCE HANDLE AND
// NOTHING ELSE. That handle is the host upgrade's, opened after the ledger
// snapshot that keeps the higher mark for a rollback; every other handle moves it
// forwards or not at all.
func TestALoweredWatermarkAdmitsTheOlderBinary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")

	plane, err := Open(t.Context(), dir, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := plane.SetReleaseWatermark(t.Context(), "v0.4.0"); err == nil {
		t.Fatal("a control-plane handle lowered the watermark")
	}

	_ = plane.Close()

	probe, err := OpenMaintenance(t.Context(), dir, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenMaintenance: %v", err)
	}

	if err := probe.SetReleaseWatermark(t.Context(), "latest"); err == nil {
		t.Fatal("a non-release was accepted as the watermark")
	}

	if err := probe.SetReleaseWatermark(t.Context(), "v0.4.0"); err != nil {
		t.Fatalf("SetReleaseWatermark through the maintenance handle: %v", err)
	}

	_ = probe.Close()

	if got := watermarkOf(t, dir); got != "v0.4.0" {
		t.Fatalf("the watermark is %q after being lowered to v0.4.0", got)
	}

	serveAs(t, dir, "v0.4.0")
}

// THE MARK MOVES AT THE CLAIM, NOT AT THE OPEN. A newer binary pointed at another
// deployment's ledger is refused by the deployment binding inside the claim; had
// the open recorded, it would have raised that ledger's mark on the way to being
// refused and fenced the ledger's own controller out of its next restart.
func TestAForeignClaimantMovesNoWatermark(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	serveAs(t, dir, "v0.5.0")

	foreign, err := Open(t.Context(), dir, WithRunningRelease("v0.6.0"))
	if err != nil {
		t.Fatalf("Open as a newer binary: %v", err)
	}

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("an open alone recorded %q; only a claim may", got)
	}

	if _, err := foreign.ClaimController(t.Context(), "elsewhere", "deployment-b"); !errors.Is(err,
		ErrForeignLedger) {
		t.Fatalf("a claim for another deployment was not refused as foreign: %v", err)
	}

	_ = foreign.Close()

	if got := watermarkOf(t, dir); got != "v0.5.0" {
		t.Fatalf("a refused foreign claimant recorded %q", got)
	}

	// AND THE LEDGER'S OWN CONTROLLER STILL RESTARTS.
	serveAs(t, dir, "v0.5.0")
}

// AN OPERATOR HANDLE OPENED BEFORE A NEWER CONTROL PLANE CLAIMED CANNOT WRITE
// AFTER IT. The open-time check was true of the ledger then; two releases that
// share a schema pass the schema re-check, so the mark is re-read inside every
// write of a revalidating handle.
func TestAnOlderHandleOpenAcrossANewerClaimCannotWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	plane, err := Open(t.Context(), dir, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("Open the control plane: %v", err)
	}

	if _, err := plane.ClaimController(t.Context(), "controller", "deployment-a"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	// An operator command beside a running control plane: no lock, revalidating.
	older, err := OpenAdmin(t.Context(), dir, WithRunningRelease("v0.5.0"))
	if err != nil {
		t.Fatalf("OpenAdmin beside the control plane: %v", err)
	}

	defer func() { _ = older.Close() }()

	if err := older.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a write beside a control plane on the same release was refused: %v", err)
	}

	_ = plane.Close()

	serveAs(t, dir, "v0.6.0")

	err = older.Tx(t.Context(), func(*sql.Tx) error { return nil })
	if !errors.Is(err, ErrReleaseBehind) {
		t.Fatalf("an older handle wrote after a newer control plane recorded itself: %v", err)
	}
}
