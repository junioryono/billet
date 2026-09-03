package deployarchive

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// TestAPlanIsNotActedOnAfterTheTargetChanges is the regression for the sharpest
// defect this change had.
//
// THE PLANNER RUNS WITH NO LOCK, ON PURPOSE — --dry-run has to be able to report
// on a live deployment without touching it — and its output is then PRINTED for
// a person to read. Minutes can pass. An operator command holding a
// state.OpenAdmin handle can commit in that window, and the plan says the ledger
// is an empty preflight one that may be deleted.
//
// The writer barrier does not save this: it proves the writers have FINISHED,
// which is not a claim about what they wrote. The plan has to be re-derived
// inside the exclusion, and this test writes a row in exactly that window.
func TestAPlanIsNotActedOnAfterTheTargetChanges(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	preflight(t, tgt.StateDir, false)

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("the plan refused an untouched preflight ledger: %s", refusalText(plan))
	}

	// THE PRECONDITION. If the plan did not intend to delete the ledger, this
	// test proves nothing about the window it is named for.
	var deleting bool

	for _, act := range plan.Actions {
		if act.Entry == EntryLedger && act.Disposition == ReplaceEmptyLedger {
			deleting = true
		}
	}

	if !deleting {
		t.Fatal("the plan does not intend to replace the ledger, so this test stages nothing")
	}

	// Somebody commissions this host between the plan and the run.
	ledger := filepath.Join(tgt.StateDir, "billet.db")

	db, err := state.Open(t.Context(), tgt.StateDir)
	if err != nil {
		t.Fatalf("Open the target ledger: %v", err)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(t.Context(),
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES ('epyc-1', 'docker', '2026-01-01')`)

		return execErr
	}); err != nil {
		t.Fatalf("write a row: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before, _, err := digestFile(ledger)
	if err != nil {
		t.Fatalf("digest the ledger: %v", err)
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a stale plan was executed against a ledger that had since been written to")
	}

	if !strings.Contains(err.Error(), "changed between") {
		t.Errorf("the refusal does not say the target moved under the plan: %v", err)
	}

	// THE ASSERTION IS THE LEDGER, not the error value. What this defect
	// destroyed was a deployment's capacity record.
	after, _, err := digestFile(ledger)
	if err != nil {
		t.Fatalf("the ledger is gone: %v", err)
	}

	if after != before {
		t.Error("the ledger that was written to between planning and running was changed")
	}
}

// TestAnAlreadyPresentCredentialIsRechecked, for the same window: a plan that
// said a file was byte-identical must not skip it after somebody replaced it.
func TestAnAlreadyPresentCredentialIsRechecked(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	// The App key is already there and identical, so the plan skips it.
	if err := os.WriteFile(tgt.AppKeyPath, src.appKey, 0o600); err != nil {
		t.Fatalf("plant the same App key: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("the plan refused an identical App key: %s", refusalText(plan))
	}

	// Somebody replaces it with a DIFFERENT key between the plan and the run.
	other := otherAppKeyPEM(t)
	if err := os.WriteFile(tgt.AppKeyPath, other, 0o600); err != nil {
		t.Fatalf("replace the App key: %v", err)
	}

	_, err = Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           nowStub,
	})

	if err == nil {
		t.Fatal("a restore skipped an App key that was no longer the one it had checked")
	}

	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, other) {
		t.Error("the App key that arrived between planning and running was modified")
	}
}

// TestALeftoverPreviousAuthorityIsRefused proves a ca-previous file the target
// has and the archive does not is preserved and refused.
//
// AUTHORITY PLANNING WALKS THE ALLOWLIST, NOT THE ARCHIVE. A target holding a
// ca-previous.crt from an abandoned rotation, restored from an archive that is
// not rotating, would otherwise never have that file looked at: the restore
// installs the current authority, reports success, and the next control-plane
// start reads the leftover as an ACTIVE rotation and tries to serve a
// certificate whose key is not there.
func TestALeftoverPreviousAuthorityIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if a.Manifest.Authority.Rotating {
		t.Fatal("this fixture's archive is rotating, so it cannot stage the case")
	}

	tgt := newTarget(t, src.github)

	leftover := wirecert.AuthorityPath(tgt.StateDir, "ca-previous.crt")

	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatalf("create the target ca dir: %v", err)
	}

	stale := read(t, wirecert.AuthorityPath(src.stateDir, "ca.crt"))

	if err := os.WriteFile(leftover, stale, 0o600); err != nil {
		t.Fatalf("stage the leftover: %v", err)
	}

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("a restore onto a host with an abandoned rotation was not refused")
	}

	if !strings.Contains(refusalText(plan), "ca-previous.crt") {
		t.Errorf("the refusal does not name the leftover: %s", refusalText(plan))
	}

	if got := read(t, leftover); !bytes.Equal(got, stale) {
		t.Error("the leftover was modified rather than preserved")
	}
}

// TestAnAppKeyPathInsideTheStateDirectoryIsRefused proves a configured key path
// under the state directory is refused, however it is spelled.
//
// github.private_key_path is an operator's string and nothing in config
// validation stops it naming a file billet creates, renames or DELETES. An
// earlier version of this check enumerated those files and missed `ca.key.new`
// and `ca.crt.new`, which `billet ca rotate` removes by name — so an App key
// configured there would be issued, installed, and then deleted by an unrelated
// command. GitHub does not reissue one. The whole directory is billet's; the
// enumeration was the bug.
func TestAnAppKeyPathInsideTheStateDirectoryIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	base := newTarget(t, src.github)

	within := func(parts ...string) string {
		return filepath.Join(append([]string{base.StateDir}, parts...)...)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{"the ledger", within("billet.db")},
		{"the process lock", within("billet.lock")},
		{"the deployment identity", within("deployment-id")},
		{"the restore journal", within(journalFile)},
		{"the authority marker", within("authority-created")},
		{"the CA key", wirecert.AuthorityPath(base.StateDir, "ca.key")},
		// THE TWO THE ENUMERATION MISSED. `billet ca rotate` clears both by name
		// before it mints, so a key here is deleted by a command that has nothing
		// to do with restoring.
		{"a rotation's staging key", within("ca", "ca.key.new")},
		{"a rotation's staging certificate", within("ca", "ca.crt.new")},
		// A name nothing writes today. The point of refusing the directory rather
		// than a list is that this one is covered too.
		{"a name billet does not use yet", within("ca", "ca-next.key")},
		{"the state directory itself", base.StateDir},
		// SPELLED DIFFERENTLY. filepath.Clean does not resolve `..`, so a path
		// that walks out and back in names the same file by another string.
		{"reached through a parent", within("ca", "..", "billet.db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tgt := base
			tgt.AppKeyPath = tc.path

			plan, err := PlanRestore(t.Context(), a, tgt)
			if err != nil {
				t.Fatalf("PlanRestore: %v", err)
			}

			if len(plan.Refusals) == 0 {
				t.Fatalf("an App key path of %s was accepted", tc.path)
			}

			if !strings.Contains(refusalText(plan), "inside the state directory") {
				t.Errorf("the refusal does not say why: %s", refusalText(plan))
			}
		})
	}

	// AND A KEY OUTSIDE IT IS NOT REFUSED, or a check that refused everything
	// would pass every case above.
	//
	// ASSERTED AS ZERO REFUSALS AND A REAL ACTION, not as the absence of one
	// string: a canonicalisation error, or any other refusal, would satisfy a
	// substring check while the App key never got planned at all.
	t.Run("a key beside the config is fine", func(t *testing.T) {
		plan, err := PlanRestore(t.Context(), a, base)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if len(plan.Refusals) > 0 {
			t.Fatalf("an App key outside the state directory was refused: %s", refusalText(plan))
		}

		var planned *Action

		for i, act := range plan.Actions {
			if act.Entry == EntryAppKey {
				planned = &plan.Actions[i]
			}
		}

		if planned == nil {
			t.Fatal("the plan says nothing about the App key")

			// Unreachable; see internal/node/reap_test.go for why it is written.
			return
		}

		if planned.Path != base.AppKeyPath {
			t.Errorf("the App key is planned for %s, want %s", planned.Path, base.AppKeyPath)
		}

		if planned.Disposition != Install {
			t.Errorf("the App key is planned as %s, want install", planned.Disposition)
		}
	})
}

// TestAnAppKeyPathReachedThroughASymlinkedAncestorIsRefused proves a key path
// that reaches the state directory through a symlink is refused too.
//
// filepath.Clean is a string operation. A symlinked parent names the same file
// by a different string, and the comparison this feeds decides whether a
// credential lands somewhere billet rewrites.
func TestAnAppKeyPathReachedThroughASymlinkedAncestorIsRefused(t *testing.T) {
	src, dest := backupOf(t)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := newTarget(t, src.github)

	if err := os.MkdirAll(tgt.StateDir, 0o700); err != nil {
		t.Fatalf("create the state dir: %v", err)
	}

	link := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(tgt.StateDir, link); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	tgt.AppKeyPath = filepath.Join(link, "app-private-key.pem")

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) == 0 {
		t.Fatal("an App key path reaching the state directory through a symlink was accepted")
	}

	if !strings.Contains(refusalText(plan), "inside the state directory") {
		t.Errorf("the refusal does not say why: %s", refusalText(plan))
	}
}

// TestAnArchiveDeclaringAnEntryThisBuildCannotInstallIsRefused proves an entry
// the manifest declares and publication has no case for refuses the archive.
//
// Publication walks a fixed set of items, so an entry the manifest declares and
// the planner has no case for would travel in the archive, pass every integrity
// check, and be silently left behind by a restore that reports success.
func TestAnArchiveDeclaringAnEntryThisBuildCannotInstallIsRefused(t *testing.T) {
	_, dest := backupOf(t)

	extra := filepath.Join(dest, "authority", "ca-next.key")
	body := []byte("a key from a schema this build does not have\n")

	if err := os.WriteFile(extra, body, 0o600); err != nil {
		t.Fatalf("stage the entry: %v", err)
	}

	rewriteManifest(t, dest, func(m *Manifest) {
		m.Files = append(m.Files, FileRecord{
			Path:   AuthorityEntry("ca-next.key"),
			SHA256: digest(body),
			Size:   int64(len(body)),
		})
	})

	_, err := Open(t.Context(), dest)
	if err == nil {
		t.Fatal("an archive declaring an entry this build cannot install opened successfully")
	}

	if !strings.Contains(err.Error(), "ca-next.key") {
		t.Errorf("the refusal does not name the entry: %v", err)
	}
}
