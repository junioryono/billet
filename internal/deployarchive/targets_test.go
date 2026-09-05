package deployarchive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
)

// personal is the further, repository-scoped target these tests add beside the
// deployment's default organization target.
var personal = GitHubIdentity{Repository: "someone/widgets", AppID: 77, InstallationID: 7777}

// backupWithTargets captures d plus one further target's key.
func backupWithTargets(t *testing.T, d deployment, dest string, extraKey []byte) Manifest {
	t.Helper()

	db, err := state.OpenAdmin(t.Context(), d.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	m, err := Write(t.Context(), BackupRequest{
		Dest:         dest,
		StateDir:     d.stateDir,
		ConfigPath:   d.configPath,
		DeploymentID: d.id,
		GitHub:       d.github,
		AppKeyPEM:    d.appKey,
		Targets:      []TargetKey{{Name: "personal", GitHub: personal, AppKeyPEM: extraKey}},
		ConfigBody:   []byte("server: {}\n"),
		Snapshot:     db.SnapshotInto,
		Now:          nowStubAt(),
		Hostname:     "test-host",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return m
}

func nowStubAt() func() time.Time {
	return func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
}

// targetWithPersonal is an empty target whose config also declares the
// further target, with a key path of its own.
func targetWithPersonal(t *testing.T) Target {
	t.Helper()

	tgt := newTarget(t, GitHubIdentity{Org: "acme", AppID: 42, InstallationID: 4242})
	tgt.Targets = []TargetPath{{
		Name:       "personal",
		AppKeyPath: filepath.Join(filepath.Dir(tgt.AppKeyPath), "app-private-key-personal.pem"),
		GitHub:     personal,
	}}

	return tgt
}

// Both keys travel and both land: the archive names the further target, its
// key is under the target's own entry, and a restore installs it at the path
// the config gives that target.
func TestEveryTargetsKeyTravelsAndLands(t *testing.T) {
	src := newDeployment(t)
	extra := otherAppKeyPEM(t)

	dest := filepath.Join(t.TempDir(), "archive")
	m := backupWithTargets(t, src, dest, extra)

	if m.Schema != Schema || len(m.Targets) != 1 || m.Targets[0].Name != "personal" ||
		!m.Targets[0].Same(personal) {
		t.Fatalf("manifest = schema %d, targets %+v", m.Schema, m.Targets)
	}

	if _, ok := m.Record(EntryAppKeyFor("personal")); !ok {
		t.Fatalf("the manifest does not record %s", EntryAppKeyFor("personal"))
	}

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tgt := targetWithPersonal(t)
	restoreInto(t, a, tgt)

	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, src.appKey) {
		t.Error("the default target's key did not land")
	}

	if got := read(t, tgt.Targets[0].AppKeyPath); !bytes.Equal(got, extra) {
		t.Error("the further target's key did not land at its own path")
	}

	// AND A SECOND RUN CONVERGES: every key reads AlreadyPresent.
	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore again: %v", err)
	}

	if len(plan.Refusals) > 0 || !plan.Nothing() {
		t.Errorf("the second plan is not a no-op: refusals %v, actions %+v", plan.Refusals, plan.Actions)
	}
}

// A backup with no further targets is the archive it always was: schema 3,
// no targets listed, the default key under the old entry name — and a schema 2
// archive from before targets existed still restores.
func TestASingleTargetArchiveIsUnchangedAndAnOldOneStillRestores(t *testing.T) {
	src := newDeployment(t)

	dest := filepath.Join(t.TempDir(), "archive")
	m := backupTo(t, src, dest)

	if len(m.Targets) != 0 {
		t.Errorf("a single-target backup lists targets %+v", m.Targets)
	}

	if _, ok := m.Record(EntryAppKey); !ok {
		t.Error("the default key is not at its schema-2 entry name")
	}

	// Rewrite the manifest as schema 2, the way a billet before targets wrote
	// it, and prove this build still reads and restores it.
	raw := read(t, filepath.Join(dest, EntryManifest))
	old := strings.Replace(string(raw), `"schema": 3`, `"schema": 2`, 1)

	if old == string(raw) {
		t.Fatal("the manifest does not say schema 3, so this case rewrites nothing")
	}

	if err := os.WriteFile(filepath.Join(dest, EntryManifest), []byte(old), 0o600); err != nil {
		t.Fatalf("rewrite the manifest: %v", err)
	}

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open a schema-2 archive: %v", err)
	}

	tgt := newTarget(t, src.github)
	restoreInto(t, a, tgt)

	if got := read(t, tgt.AppKeyPath); !bytes.Equal(got, src.appKey) {
		t.Error("the schema-2 archive's key did not land")
	}
}

// A schema-2 manifest that lists targets was written by nothing billet ships,
// and reading it would plan keys under names that schema never had.
func TestAnOldSchemaListingTargetsIsRefused(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")
	backupWithTargets(t, src, dest, otherAppKeyPEM(t))

	raw := read(t, filepath.Join(dest, EntryManifest))
	old := strings.Replace(string(raw), `"schema": 3`, `"schema": 2`, 1)

	if err := os.WriteFile(filepath.Join(dest, EntryManifest), []byte(old), 0o600); err != nil {
		t.Fatalf("rewrite the manifest: %v", err)
	}

	if _, err := Open(t.Context(), dest); err == nil || !strings.Contains(err.Error(), "further targets") {
		t.Fatalf("Open: %v, want a refusal naming the contradiction", err)
	}
}

// The config and the archive must agree on every target, in both directions.
func TestTargetsAreHeldToTheConfigInBothDirections(t *testing.T) {
	src := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")
	backupWithTargets(t, src, dest, otherAppKeyPEM(t))

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Run("a target the config does not declare", func(t *testing.T) {
		tgt := newTarget(t, src.github)

		plan, err := PlanRestore(t.Context(), a, tgt)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if !refusedWith(plan, `carries an App key for target "personal"`) {
			t.Errorf("refusals: %+v", plan.Refusals)
		}
	})

	t.Run("a target naming another App", func(t *testing.T) {
		tgt := targetWithPersonal(t)
		tgt.Targets[0].GitHub.AppID = 78

		plan, err := PlanRestore(t.Context(), a, tgt)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if !refusedWith(plan, `this backup's target "personal" is repository someone/widgets, app 77`) {
			t.Errorf("refusals: %+v", plan.Refusals)
		}
	})

	t.Run("a target the archive has no key for", func(t *testing.T) {
		single := filepath.Join(t.TempDir(), "single")
		backupTo(t, src, single)

		one, err := Open(t.Context(), single)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		plan, err := PlanRestore(t.Context(), one, targetWithPersonal(t))
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if !refusedWith(plan, `declares target "personal"`) {
			t.Errorf("refusals: %+v", plan.Refusals)
		}
	})

	t.Run("a further key already present and different", func(t *testing.T) {
		tgt := targetWithPersonal(t)

		// The DEFAULT target's key at the further target's path: a real key,
		// and not the one the archive carries for that target.
		if err := os.WriteFile(tgt.Targets[0].AppKeyPath, src.appKey, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		plan, err := PlanRestore(t.Context(), a, tgt)
		if err != nil {
			t.Fatalf("PlanRestore: %v", err)
		}

		if !refusedWith(plan, "GitHub App private key for target personal") {
			t.Errorf("refusals: %+v", plan.Refusals)
		}
	})
}

// A backup refuses a further target with no key, no name or the default's name.
func TestABackupRefusesAHalfDescribedTarget(t *testing.T) {
	src := newDeployment(t)

	db, err := state.OpenAdmin(t.Context(), src.stateDir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	for name, target := range map[string]TargetKey{
		"no key":           {Name: "personal", GitHub: personal},
		"no name":          {GitHub: personal, AppKeyPEM: src.appKey},
		"the default name": {Name: "default", GitHub: personal, AppKeyPEM: src.appKey},
		"no App":           {Name: "personal", AppKeyPEM: src.appKey},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Write(t.Context(), BackupRequest{
				Dest:         filepath.Join(t.TempDir(), "archive"),
				StateDir:     src.stateDir,
				DeploymentID: src.id,
				GitHub:       src.github,
				AppKeyPEM:    src.appKey,
				Targets:      []TargetKey{target},
				Snapshot:     db.SnapshotInto,
				Now:          nowStubAt(),
			})
			if err == nil {
				t.Fatalf("Write accepted a further target with %s", name)
			}
		})
	}
}

// refusedWith reports whether any refusal carries the text.
func refusedWith(plan Plan, text string) bool {
	for _, r := range plan.Refusals {
		if strings.Contains(r.What, text) {
			return true
		}
	}

	return false
}
