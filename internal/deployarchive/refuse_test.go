package deployarchive

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// backupOf is the common setup: a real deployment captured into a new archive.
func backupOf(t *testing.T) (deployment, string) {
	t.Helper()

	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "backup")

	backupTo(t, src, dest)

	return src, dest
}

// rewriteManifest replaces the archive's manifest with a modified one.
func rewriteManifest(t *testing.T, dest string, edit func(*Manifest)) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dest, EntryManifest))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}

	edit(&m)

	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dest, EntryManifest), append(body, '\n'), 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

// TestAChangedArchiveIsRefused. The digest is the whole reason a restore may
// install a credential out of a directory nobody has verified since it was
// written.
//
// THE SAME-LENGTH CASE IS THE ONE THAT TESTS THE DIGEST. An earlier version of
// this test appended a byte, which changes the SIZE — so it passed with the
// digest comparison deleted, proving only that the length is checked. Mutation
// found it; both cases are kept, and the same-length one names the digest.
func TestAChangedArchiveIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func([]byte) []byte
		says   string
	}{
		{
			name: "one byte changed, same length",
			tamper: func(b []byte) []byte {
				out := append([]byte(nil), b...)
				out[len(out)/2] ^= 0x01

				return out
			},
			says: "digest",
		},
		{
			name:   "truncated",
			tamper: func(b []byte) []byte { return b[:len(b)-1] },
			says:   "bytes and",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dest := backupOf(t)

			path := filepath.Join(dest, AuthorityEntry("ca.crt"))

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read ca.crt: %v", err)
			}

			changed := tc.tamper(body)

			if bytes.Equal(changed, body) {
				t.Fatal("the fixture did not change the file, so this proves nothing")
			}

			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatalf("tamper with ca.crt: %v", err)
			}

			_, err = Open(t.Context(), dest)
			if err == nil {
				t.Fatal("a changed archive opened successfully")
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not name %q: %v", tc.says, err)
			}
		})
	}
}

// TestAFileTheManifestDoesNotDescribeIsRefused proves that a file the manifest does not describe is refused.
func TestAFileTheManifestDoesNotDescribeIsRefused(t *testing.T) {
	_, dest := backupOf(t)

	if err := os.WriteFile(filepath.Join(dest, "extra.pem"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("add a stray file: %v", err)
	}

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an archive with an undescribed file opened successfully")
	}

	if !strings.Contains(err.Error(), "extra.pem") {
		t.Errorf("the refusal does not name the stray file: %v", err)
	}
}

// TestASymlinkedArchiveEntryIsRefused proves that a symlinked archive entry is refused.
//
// A LINK MAKES BILLET READ A FILE THE ARCHIVE DOES NOT CONTAIN, after which the
// digest check compares the manifest against whatever that link found.
func TestASymlinkedArchiveEntryIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	path := filepath.Join(dest, EntryAppKey)

	if err := os.Remove(path); err != nil {
		t.Fatalf("clear the App key entry: %v", err)
	}

	if err := os.Symlink(src.appKeyPath, path); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an archive with a symlinked entry opened successfully")
	}

	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not name the symlink: %v", err)
	}
}

// TestAnArchiveSchemaThisBuildDoesNotKnowIsRefused, naming the version rather
// than failing five checks later on a field it never had.
func TestAnArchiveSchemaThisBuildDoesNotKnowIsRefused(t *testing.T) {
	_, dest := backupOf(t)

	rewriteManifest(t, dest, func(m *Manifest) { m.Schema = Schema + 7 })

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an archive from a future schema opened successfully")
	}

	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("the refusal does not name the schema: %v", err)
	}
}

// TestAnArchiveMissingPartOfTheUnitIsRefused. This is the refusal requirement
// stated as a test: a partial restore must be refused rather than silently
// producing a new authority or an empty ledger.
func TestAnArchiveMissingPartOfTheUnitIsRefused(t *testing.T) {
	// THE ORDINARY ARCHIVE'S SET, which is the one this is about: a backup that
	// carries its own ledger. The external variant's required set is one entry
	// shorter and is covered by its own tests.
	for _, entry := range requiredEntries(Manifest{}) {
		t.Run(entry, func(t *testing.T) {
			_, dest := backupOf(t)

			if err := os.Remove(filepath.Join(dest, entry)); err != nil {
				t.Fatalf("remove %s: %v", entry, err)
			}

			rewriteManifest(t, dest, func(m *Manifest) {
				kept := m.Files[:0]

				for _, f := range m.Files {
					if f.Path != entry {
						kept = append(kept, f)
					}
				}

				m.Files = kept
			})

			_, err := Open(t.Context(), dest)
			if err == nil {
				t.Fatalf("an archive with no %s opened successfully", entry)
			}

			if !errors.Is(err, errNotWholeDeployment) {
				t.Errorf("removing %s was not reported as an incomplete unit: %v", entry, err)
			}
		})
	}
}

// TestAManifestThatDoesNotDescribeItsLedgerIsRefused proves that a manifest that does not describe its ledger is refused.
func TestAManifestThatDoesNotDescribeItsLedgerIsRefused(t *testing.T) {
	_, dest := backupOf(t)

	rewriteManifest(t, dest, func(m *Manifest) {
		m.Ledger.Migrations = m.Ledger.Migrations[:len(m.Ledger.Migrations)-1]
	})

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("a manifest that miscounts its own ledger opened successfully")
	}

	if !strings.Contains(err.Error(), "migrations") {
		t.Errorf("the refusal does not name the mismatch: %v", err)
	}
}

// manifestOf reads an archive's manifest without going through Open, for a
// fixture that is about to change the archive.
func manifestOf(t *testing.T, dest string) Manifest {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dest, EntryManifest))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}

	return m
}

// forgeFutureMigration makes the archive's snapshot carry a migration version
// this binary has never heard of, and keeps the manifest honest about it.
func forgeFutureMigration(t *testing.T, dest string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "forge")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("create the forge dir: %v", err)
	}

	ledger := filepath.Join(dest, EntryLedger)

	rec, found := manifestOf(t, dest).Record(EntryLedger)
	if !found {
		t.Fatal("the manifest does not describe its ledger")
	}

	if _, err := copyFile(ledger, filepath.Join(work, "billet.db"), rec); err != nil {
		t.Fatalf("copy the snapshot aside: %v", err)
	}

	db, err := state.Open(t.Context(), work)
	if err != nil {
		t.Fatalf("Open the copied snapshot: %v", err)
	}

	err = db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(t.Context(),
			`INSERT INTO schema_migrations (version, name, checksum, applied_at)
			 VALUES (9999, 'from_the_future', 'deadbeef', '2030-01-01T00:00:00Z')`)

		return execErr
	})
	if err != nil {
		t.Fatalf("forge a future migration: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := os.Remove(ledger); err != nil {
		t.Fatalf("clear the archive's ledger: %v", err)
	}

	forged := filepath.Join(work, "billet.db")

	sum, size, err := digestFile(forged)
	if err != nil {
		t.Fatalf("digest the forged snapshot: %v", err)
	}

	if _, err := copyFile(forged, ledger,
		FileRecord{Path: EntryLedger, SHA256: sum, Size: size}); err != nil {
		t.Fatalf("put the forged snapshot back: %v", err)
	}

	rewriteManifest(t, dest, func(m *Manifest) {
		for i := range m.Files {
			if m.Files[i].Path == EntryLedger {
				m.Files[i].SHA256 = sum
				m.Files[i].Size = size
			}
		}

		m.Ledger.Migrations = append(m.Ledger.Migrations,
			state.AppliedMigration{Version: 9999, Name: "from_the_future", Checksum: "deadbeef"})
	})
}

// TestALedgerFromANewerBilletIsRefusedWithItsOwnDiagnostic proves that a ledger from a newer billet is refused with its own diagnostic.
//
// KEPT APART FROM ErrSchemaBehind ON PURPOSE. That error means a RUNNING control
// plane holds a ledger this binary would have to migrate, and its remedy is a
// restart; this one's remedy is a newer binary. Collapsing the two sends an
// operator to restart a service that is not the problem.
func TestALedgerFromANewerBilletIsRefusedWithItsOwnDiagnostic(t *testing.T) {
	src, dest := backupOf(t)

	forgeFutureMigration(t, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open a forged-but-consistent archive: %v", err)
	}

	tgt := newTarget(t, src.github)

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a backup from a newer billet was not refused")
	}

	joined := refusalText(plan)

	if !strings.Contains(joined, "newer version") {
		t.Errorf("the refusal does not say the backup came from a newer billet: %s", joined)
	}

	if strings.Contains(joined, state.ErrSchemaBehind.Error()) {
		t.Errorf("the refusal reuses ErrSchemaBehind, which means something else entirely: %s",
			joined)
	}
}

func refusalText(p Plan) string {
	var b strings.Builder

	for _, r := range p.Refusals {
		b.WriteString(r.What)
		b.WriteString(" | ")
		b.WriteString(r.Remedy)
		b.WriteString("\n")
	}

	return b.String()
}

// TestADifferentAppKeyIsPreservedAndRefused proves that a different app key is preserved and refused.
//
// THE ASSERTION IS THE BYTES, not that an error came back. GitHub issues this
// key exactly once and will not reissue it, so what matters is that the file is
// still exactly what it was.
func TestADifferentAppKeyIsPreservedAndRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	other := otherAppKeyPEM(t)
	if err := os.WriteFile(tgt.AppKeyPath, other, 0o600); err != nil {
		t.Fatalf("plant a different App key: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a restore over a different App key was not refused")
	}

	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, other) {
		t.Error("the App key already at the destination was modified")
	}

	if !strings.Contains(refusalText(plan), "App private key") {
		t.Errorf("the refusal does not name the App key: %s", refusalText(plan))
	}
}

// TestAnIdenticalAppKeyIsAccepted — the other direction, without which a
// planner that refused every occupied destination would pass the test above.
func TestAnIdenticalAppKeyIsAccepted(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	if err := os.WriteFile(tgt.AppKeyPath, src.appKey, 0o600); err != nil {
		t.Fatalf("plant the same App key: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("a restore beside an identical App key was refused: %s", refusalText(plan))
	}

	for _, act := range plan.Actions {
		if act.Entry == EntryAppKey && act.Disposition != AlreadyPresent {
			t.Errorf("an identical App key is planned as %s rather than already present",
				act.Disposition)
		}
	}
}

// TestADifferentDeploymentIdentityIsRefused, and the file is untouched.
func TestADifferentDeploymentIdentityIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	other := newDeployment(t)

	idPath := filepath.Join(tgt.StateDir, "deployment-id")
	if err := os.WriteFile(idPath, []byte(other.id+"\n"), 0o600); err != nil {
		t.Fatalf("plant a different identity: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a restore onto a different deployment was not refused")
	}

	if got := strings.TrimSpace(string(read(t, idPath))); got != other.id {
		t.Errorf("the identity already at the destination was changed to %s", got)
	}

	if !strings.Contains(refusalText(plan), other.id) {
		t.Errorf("the refusal does not name the identity that is already there: %s",
			refusalText(plan))
	}
}

// TestADifferentAuthorityIsPreservedAndRefused proves that a different authority is preserved and refused.
func TestADifferentAuthorityIsPreservedAndRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	other := newDeployment(t)

	caDir := filepath.Join(tgt.StateDir, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("create the target ca dir: %v", err)
	}

	otherCert := read(t, wirecert.AuthorityPath(other.stateDir, "ca.crt"))

	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), otherCert, 0o600); err != nil {
		t.Fatalf("plant a different authority: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a restore over a different authority was not refused")
	}

	if got := read(t, filepath.Join(caDir, "ca.crt")); !bytes.Equal(got, otherCert) {
		t.Error("the authority already at the destination was modified")
	}
}

// TestAConfigForAnotherAppIsRefused. The key file says nothing about which App
// it belongs to, so pairing it with unrelated configuration produces a control
// plane that authenticates as nothing.
func TestAConfigForAnotherAppIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	wrong := src.github
	wrong.AppID++

	tgt := newTarget(t, wrong)

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a restore against a config naming another App was not refused")
	}

	if !strings.Contains(refusalText(plan), "app ") {
		t.Errorf("the refusal does not name the App identities: %s", refusalText(plan))
	}
}

// TestAnEmptyPreflightLedgerIsReplacedAndAPopulatedOneIsNot proves that an empty preflight ledger is replaced and a populated one is not.
//
// BOTH DIRECTIONS IN ONE TEST, because they are the same rule seen from either
// side: `billet check` creates a schema-only billet.db on a host nobody has
// commissioned, so refusing on the FILE would refuse the main case this command
// exists for — and accepting on the file would discard somebody's capacity
// record.
func TestAnEmptyPreflightLedgerIsReplacedAndAPopulatedOneIsNot(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Run("a preflight ledger is replaced", func(t *testing.T) {
		tgt := newTarget(t, src.github)

		preflight(t, tgt.StateDir, false)

		plan, err := PlanRestore(t.Context(), a, tgt)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if len(plan.Refusals) > 0 {
			t.Fatalf("a restore over an untouched preflight ledger was refused: %s",
				refusalText(plan))
		}

		var found bool

		for _, act := range plan.Actions {
			if act.Entry == EntryLedger {
				found = true

				if act.Disposition != ReplaceEmptyLedger {
					t.Errorf("the preflight ledger is planned as %s", act.Disposition)
				}
			}
		}

		if !found {
			t.Error("the plan says nothing about the ledger")
		}

		// STAGED FROM INSIDE THE PUBLICATION, and two earlier attempts at this
		// assertion were vacuous for the same reason: SQLite deletes billet.db-wal
		// and billet.db-shm when the last connection closes, and TWO things open
		// that database before the ledger is replaced — PeekLedger while planning,
		// and the writer barrier at the start of Execute. Sidecars written before
		// either of those are gone by the time the executor acts, so the test
		// passed with the sidecar handling deleted.
		//
		// Placing them here puts them exactly where they have to be survivable
		// from: after the barrier, immediately before the ledger is removed.
		var staged []string

		prev := onPublish

		onPublish = func(act Action) error {
			if act.Entry != EntryLedger {
				return nil
			}

			for _, suffix := range []string{"-wal", "-shm"} {
				path := filepath.Join(tgt.StateDir, "billet.db"+suffix)

				if err := os.WriteFile(path, nil, 0o600); err != nil {
					return err
				}

				staged = append(staged, path)
			}

			return nil
		}

		t.Cleanup(func() { onPublish = prev })

		if _, err := Execute(t.Context(), RestoreRequest{
			Plan:          plan,
			InstallAppKey: testInstallAppKey,
			Now:           nowStub,
			Actor:         "test",
		}); err != nil {
			t.Fatalf("Execute: %v", err)
		}

		if len(staged) != 2 {
			t.Fatalf("the fixture staged %d sidecars; it never reached the ledger", len(staged))
		}

		for _, path := range staged {
			if _, err := os.Lstat(path); err == nil {
				t.Errorf("%s survived the replacement; it would be replayed into the restored "+
					"ledger, which is corruption rather than a stale file", path)
			}
		}
	})

	t.Run("a ledger with a row in it is refused", func(t *testing.T) {
		tgt := newTarget(t, src.github)

		preflight(t, tgt.StateDir, true)

		plan, err := PlanRestore(t.Context(), a, tgt)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if len(plan.Refusals) == 0 {
			t.Fatal("a restore over a ledger holding deployment data was not refused")
		}

		if !strings.Contains(refusalText(plan), "nodes") {
			t.Errorf("the refusal does not name the table that made it populated: %s",
				refusalText(plan))
		}
	})
}

// preflight creates the ledger `billet check` would leave, optionally with a
// row in it.
func preflight(t *testing.T, stateDir string, populated bool) {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("Open the preflight ledger: %v", err)
	}

	if populated {
		err = db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, execErr := tx.ExecContext(t.Context(),
				`INSERT INTO nodes (name, provider, last_seen_at) VALUES ('epyc-1', 'docker', '2026-01-01')`)

			return execErr
		})
		if err != nil {
			t.Fatalf("populate the preflight ledger: %v", err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// state.Open MINTS a deployment identity? It does not — only DeploymentID
	// does — but it does create the directory and the ledger, which is exactly
	// what the documented preflight leaves behind.
	if _, found, err := state.PeekDeploymentID(stateDir); err != nil || found {
		t.Fatalf("the preflight left an identity behind (found=%v, err=%v); this fixture would "+
			"then be testing a commissioned host", found, err)
	}
}
