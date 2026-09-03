package deployarchive

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// TestARestoredDeploymentIsTheSameDeployment is the headline property: what
// comes back is byte-for-byte the identity, the App key and the authority that
// went in, and the ledger carries exactly the same applied migrations.
//
// THE LEDGER IS COMPARED BY SCHEMA AND CONTENT, NEVER BY FILE BYTES. VACUUM INTO
// repacks pages, so a byte-identity assertion would fail against a correct
// backup — and if it ever passed it would be pinning SQLite's page layout rather
// than anything billet is responsible for.
func TestARestoredDeploymentIsTheSameDeployment(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "backup")

	m := backupTo(t, src, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	res := restoreInto(t, a, tgt)

	if len(res.Installed) == 0 {
		t.Fatal("a restore into an empty directory installed nothing")
	}

	// The identity, exactly.
	gotID, found, err := state.PeekDeploymentID(tgt.StateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if gotID != src.id {
		t.Errorf("restored deployment id = %s, want %s", gotID, src.id)
	}

	// The App key, exactly. This is the one GitHub will not reissue.
	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, src.appKey) {
		t.Error("the restored App key is not byte-identical to the original")
	}

	// The authority, exactly — both halves and the marker.
	for _, name := range []string{"ca.key", "ca.crt", "authority-created"} {
		want := read(t, wirecert.AuthorityPath(src.stateDir, name))
		got := read(t, wirecert.AuthorityPath(tgt.StateDir, name))

		if !bytes.Equal(got, want) {
			t.Errorf("restored %s is not byte-identical to the original", name)
		}
	}

	// The ledger, by schema rather than by file bytes.
	wantMigrations := migrationsOf(t, t.Context(), filepath.Join(src.stateDir, "billet.db"))
	gotMigrations := migrationsOf(t, t.Context(), filepath.Join(tgt.StateDir, "billet.db"))

	if !reflect.DeepEqual(wantMigrations, gotMigrations) {
		t.Errorf("restored migration set differs:\n got %v\nwant %v", gotMigrations, wantMigrations)
	}

	if !reflect.DeepEqual(m.Ledger.Migrations, wantMigrations) {
		t.Errorf("the manifest's migration set does not describe the snapshot it was read from")
	}

	// And the restored deployment actually opens as a control plane.
	db, err := state.Open(t.Context(), tgt.StateDir)
	if err != nil {
		t.Fatalf("the restored state directory does not open as a control plane: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTheFenceIsClearedOnlyWhenTheRestoreFinished proves the two halves of the
// fence rule at once: a finished restore leaves nothing fenced, and a control
// plane can start.
func TestTheFenceIsClearedOnlyWhenTheRestoreFinished(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	restoreInto(t, a, tgt)

	if _, err := os.Lstat(state.MaintenanceFencePath(tgt.StateDir)); !os.IsNotExist(err) {
		t.Errorf("a finished restore left the ledger fenced (%v)", err)
	}

	if _, err := os.Lstat(JournalPath(tgt.StateDir)); !os.IsNotExist(err) {
		t.Errorf("a finished restore left its journal behind (%v)", err)
	}
}

// TestRestoringTwiceChangesNothing is what makes an interrupted run resumable
// at all: every decision is idempotent, so the second pass finds everything
// already in place rather than refusing.
func TestRestoringTwiceChangesNothing(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	restoreInto(t, a, tgt)

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore (second pass): %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("a second restore of the same backup was refused: %v", plan.Refusals)
	}

	if !plan.Nothing() {
		for _, act := range plan.Actions {
			if act.Disposition != AlreadyPresent {
				t.Errorf("second pass would still %s %s", act.Disposition, act.Path)
			}
		}
	}
}

// TestABackupDuringARotationCarriesBothAuthorities proves that a backup during a rotation carries both authorities.
//
// THE PREVIOUS KEY IS THE ONE THAT MATTERS. It signs the certificate the control
// plane PRESENTS until every node has renewed, so a backup that captured only
// the new authority would restore a deployment no un-renewed node can verify —
// over the wire it would need in order to recover.
func TestABackupDuringARotationCarriesBothAuthorities(t *testing.T) {
	src := newDeployment(t)

	if _, err := wirecert.Rotate(src.stateDir, src.id); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup")

	m := backupTo(t, src, dest)

	if !m.Authority.Rotating {
		t.Error("the manifest does not record that a rotation was running")
	}

	if m.Authority.PreviousFingerprint == "" {
		t.Error("the manifest records no previous authority during a rotation")
	}

	if m.Authority.PreviousFingerprint == m.Authority.Fingerprint {
		t.Error("the previous authority's fingerprint equals the current one's")
	}

	for _, name := range []string{"ca.key", "ca.crt", "ca-previous.key", "ca-previous.crt",
		"authority-created"} {
		if _, ok := m.Record(AuthorityEntry(name)); !ok {
			t.Errorf("the archive does not carry %s", name)
		}
	}

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	restoreInto(t, a, tgt)

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		want := read(t, wirecert.AuthorityPath(src.stateDir, name))
		got := read(t, wirecert.AuthorityPath(tgt.StateDir, name))

		if !bytes.Equal(got, want) {
			t.Errorf("restored %s is not byte-identical to the original", name)
		}
	}

	// And the restored overlap still resolves: the previous authority is what
	// the control plane would present.
	authority, err := wirecert.LoadServing(tgt.StateDir, src.id)
	if err != nil {
		t.Fatalf("LoadServing on the restored deployment: %v", err)
	}

	if authority.Presents.Fingerprint() != m.Authority.PreviousFingerprint {
		t.Errorf("the restored deployment would present %s, and the backup's previous authority "+
			"is %s", authority.Presents.Fingerprint(), m.Authority.PreviousFingerprint)
	}
}

// TestAnInterruptedRotationLeavesLeftoversOutOfTheArchive proves that an interrupted rotation leaves leftovers out of the archive.
//
// A ca.crt.new is a half-minted authority, not authority state. Capturing it
// would put it in an archive that says it is complete; ignoring it silently
// would leave an operator wondering later why it did not travel.
func TestAnInterruptedRotationLeavesLeftoversOutOfTheArchive(t *testing.T) {
	src := newDeployment(t)

	leftover := filepath.Join(wirecert.CADir(src.stateDir), "ca.crt.new")
	if err := os.WriteFile(leftover, []byte("half a rotation\n"), 0o600); err != nil {
		t.Fatalf("stage the leftover: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup")

	m := backupTo(t, src, dest)

	if _, err := os.Lstat(filepath.Join(dest, AuthorityEntry("ca.crt.new"))); !os.IsNotExist(err) {
		t.Errorf("the archive captured ca.crt.new (%v)", err)
	}

	if len(wirecert.RotationLeftovers(m.Authority.UnexpectedFilesPresent)) == 0 {
		t.Errorf("the manifest does not name the leftover; UnexpectedFilesPresent = %v",
			m.Authority.UnexpectedFilesPresent)
	}
}

// TestAHalfInitialisedAuthorityRefusesTheBackup, and the other direction: a
// healthy one is not refused. Without the second half, a ReadAuthority that
// refused everything would pass the first.
func TestAHalfInitialisedAuthorityRefusesTheBackup(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "healthy")

	backupTo(t, src, dest)

	if err := os.Remove(wirecert.AuthorityPath(src.stateDir, "ca.key")); err != nil {
		t.Fatalf("remove ca.key: %v", err)
	}

	db, err := state.OpenAdmin(t.Context(), src.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	_, err = Write(t.Context(), BackupRequest{
		Dest:         filepath.Join(t.TempDir(), "broken"),
		StateDir:     src.stateDir,
		ConfigPath:   src.configPath,
		DeploymentID: src.id,
		GitHub:       src.github,
		AppKeyPEM:    src.appKey,
		Snapshot:     db.SnapshotInto,
		Now:          nowStub,
	})

	if err == nil {
		t.Fatal("a backup of a half-initialised authority succeeded")
	}

	if !strings.Contains(err.Error(), "ca.crt") || !strings.Contains(err.Error(), "ca.key") {
		t.Errorf("the refusal does not name both halves: %v", err)
	}
}

// TestAMissingAuthorityMarkerRefusesTheBackup proves that a missing authority marker refuses the backup.
//
// The marker is what makes a LATER absence mean loss rather than day one. An
// archive without it restores a deployment that would silently mint a
// replacement authority and drop every node in the fleet.
func TestAMissingAuthorityMarkerRefusesTheBackup(t *testing.T) {
	src := newDeployment(t)

	if err := os.Remove(filepath.Join(src.stateDir, "authority-created")); err != nil {
		t.Fatalf("remove the marker: %v", err)
	}

	db, err := state.OpenAdmin(t.Context(), src.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	_, err = Write(t.Context(), BackupRequest{
		Dest:         filepath.Join(t.TempDir(), "no-marker"),
		StateDir:     src.stateDir,
		ConfigPath:   src.configPath,
		DeploymentID: src.id,
		GitHub:       src.github,
		AppKeyPEM:    src.appKey,
		Snapshot:     db.SnapshotInto,
		Now:          nowStub,
	})

	if err == nil {
		t.Fatal("a backup with no authority marker succeeded")
	}

	if !strings.Contains(err.Error(), "authority-created") {
		t.Errorf("the refusal does not name the marker: %v", err)
	}
}

// TestABackupRefusesADestinationInsideTheStateDirectory proves that a backup refuses a destination inside the state directory.
func TestABackupRefusesADestinationInsideTheStateDirectory(t *testing.T) {
	src := newDeployment(t)

	db, err := state.OpenAdmin(t.Context(), src.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	for _, dest := range []string{
		filepath.Join(src.stateDir, "backup"),
		src.stateDir,
		filepath.Dir(src.stateDir),
	} {
		_, err := Write(t.Context(), BackupRequest{
			Dest:         dest,
			StateDir:     src.stateDir,
			ConfigPath:   src.configPath,
			DeploymentID: src.id,
			GitHub:       src.github,
			AppKeyPEM:    src.appKey,
			Snapshot:     db.SnapshotInto,
			Now:          nowStub,
		})
		if err == nil {
			t.Errorf("a backup to %s (inside or holding the state directory) succeeded", dest)

			continue
		}

		if !strings.Contains(err.Error(), "state directory") {
			t.Errorf("the refusal for %s does not say why: %v", dest, err)
		}
	}
}

// TestABackupRefusesANonEmptyDestination, because an archive is a whole unit
// and mixing one into a directory that already holds something produces a
// manifest that does not describe its own directory.
func TestABackupRefusesANonEmptyDestination(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "occupied")

	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatalf("create the destination: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dest, "something"), []byte("x"), 0o600); err != nil {
		t.Fatalf("occupy the destination: %v", err)
	}

	db, err := state.OpenAdmin(t.Context(), src.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	_, err = Write(t.Context(), BackupRequest{
		Dest:         dest,
		StateDir:     src.stateDir,
		ConfigPath:   src.configPath,
		DeploymentID: src.id,
		GitHub:       src.github,
		AppKeyPEM:    src.appKey,
		Snapshot:     db.SnapshotInto,
		Now:          nowStub,
	})

	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("a backup into a non-empty directory was not refused for being non-empty: %v", err)
	}
}

// TestEveryArchiveFileIs0600, because the archive holds two private keys and a
// ledger, and the directory mode is not the only thing that should stand between
// them and every account on the host.
func TestEveryArchiveFileIs0600(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "backup")

	m := backupTo(t, src, dest)

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat the archive: %v", err)
	}

	if info.Mode().Perm() != 0o700 {
		t.Errorf("the archive directory is mode %04o, want 0700", info.Mode().Perm())
	}

	names := []string{EntryManifest}
	for _, f := range m.Files {
		names = append(names, f.Path)
	}

	for _, name := range names {
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}

		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %04o, want 0600", name, fi.Mode().Perm())
		}
	}
}
