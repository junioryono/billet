package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// backupFixture is a config, a state directory with a real authority, and an App
// key on disk — everything the two commands read.
type backupFixture struct {
	configPath string
	stateDir   string
	keyPath    string
	deployment string
	appKey     []byte
}

// newBackupFixture builds a commissioned deployment the commands can act on.
func newBackupFixture(t *testing.T, commissioned bool) backupFixture {
	t.Helper()

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	keyPath := filepath.Join(root, "app-private-key.pem")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an App key: %v", err)
	}

	appKey := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	if err := os.WriteFile(keyPath, appKey, 0o600); err != nil {
		t.Fatalf("write the App key: %v", err)
	}

	configPath := filepath.Join(root, "billet.yaml")

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + keyPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	f := backupFixture{
		configPath: configPath,
		stateDir:   stateDir,
		keyPath:    keyPath,
		appKey:     appKey,
	}

	if !commissioned {
		return f
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	id, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(stateDir, id); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	f.deployment = id

	return f
}

// stubLifecycleLock replaces the host lock, which lives in a directory only root
// can create.
func stubLifecycleLock(t *testing.T) {
	t.Helper()

	prev := hostLockDir
	hostLockDir = t.TempDir()

	t.Cleanup(func() { hostLockDir = prev })
}

// TestLocalBackupCapturesTheWholeDeployment drives the COMMAND rather than the
// package underneath it, so the config reading, the App-key read and the ledger
// handle are all exercised on the path an operator takes.
func TestLocalBackupCapturesTheWholeDeployment(t *testing.T) {
	f := newBackupFixture(t, true)
	dest := filepath.Join(t.TempDir(), "backup")

	if err := cmdLocalBackup(t.Context(), []string{"--config", f.configPath, "--out", dest}); err != nil {
		t.Fatalf("billet local backup: %v", err)
	}

	a, err := deployarchive.Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("the archive it wrote does not open: %v", err)
	}

	if a.Manifest.DeploymentID != f.deployment {
		t.Errorf("the archive is of deployment %s, want %s", a.Manifest.DeploymentID, f.deployment)
	}

	if a.Manifest.GitHub.Org != "acme" || a.Manifest.GitHub.AppID != 1 ||
		a.Manifest.GitHub.InstallationID != 2 {
		t.Errorf("the manifest does not record the App identity from the config: %v",
			a.Manifest.GitHub)
	}

	key, ok := a.Entry(deployarchive.EntryAppKey)
	if !ok || !bytes.Equal(key, f.appKey) {
		t.Error("the archive does not carry the App key the config names")
	}
}

// TestLocalBackupNeedsAControlPlane. The ledger, the identity and the authority
// all live in server.state_dir; a node-only host has none of them.
func TestLocalBackupNeedsAControlPlane(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "billet.yaml")

	body := `
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + filepath.Join(root, "node") + `
`

	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	err := cmdLocalBackup(t.Context(),
		[]string{"--config", configPath, "--out", filepath.Join(root, "backup")})
	if err == nil {
		t.Fatal("a backup on a node-only host succeeded")
	}

	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("the refusal does not say a backup is of a control plane: %v", err)
	}
}

// TestLocalBackupRefusesAnUncommissionedHost. A state directory with no identity
// is not a deployment, and PeekDeploymentID must not MINT one as a side effect of
// being asked.
func TestLocalBackupRefusesAnUncommissionedHost(t *testing.T) {
	f := newBackupFixture(t, false)
	dest := filepath.Join(t.TempDir(), "backup")

	err := cmdLocalBackup(t.Context(), []string{"--config", f.configPath, "--out", dest})
	if err == nil {
		t.Fatal("a backup of a host with no deployment identity succeeded")
	}

	if !strings.Contains(err.Error(), "deployment identity") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}

	if probe := state.ProbeDeploymentID(f.stateDir); probe == state.IdentityPresent {
		t.Error("asking about an uncommissioned host minted an identity for it")
	}
}

// TestLocalBackupNeedsAnOutDirectory proves that local backup needs an out directory.
func TestLocalBackupNeedsAnOutDirectory(t *testing.T) {
	f := newBackupFixture(t, true)

	err := cmdLocalBackup(t.Context(), []string{"--config", f.configPath})
	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Errorf("a backup with no destination was not refused for that: %v", err)
	}
}

// A POSTGRESQL BACKUP IS NOT REFUSED BEFORE IT STARTS.
//
// THIS IS THE TEST THE FEATURE SHIPPED WITHOUT, AND ITS ABSENCE IS WHY THE WHOLE
// THING WAS UNREACHABLE. `billet local backup` carried a blanket refusal for a
// PostgreSQL ledger that returned before anything else ran — correct while the
// archive had no identity-only form, and after that form existed it made every
// line below it dead code for precisely the deployment they were written for.
// The package tests drove deployarchive.Write directly and could not see it.
//
// NO DATABASE IS NEEDED TO ASSERT THIS, which is what makes it an ordinary test
// rather than one gated on a server. What is under test is that the command gets
// PAST the config and reaches the ledger handle: with no reachable database it
// still fails, and the assertion is about WHICH failure. A refusal that names the
// backend is the defect; anything about opening or connecting is the command
// having got where it was going.
func TestAPostgresBackupIsNotRefusedBeforeItStarts(t *testing.T) {
	f := newBackupFixture(t, true)

	// The same deployment, re-declared with an external ledger. identity_dir and
	// state_dir are mutually exclusive at load, so this is a rewrite rather than
	// an edit.
	body := `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + f.stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_TEST_UNREACHABLE_DSN
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + f.keyPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	// A DSN THAT PARSES AND CANNOT CONNECT. An unset variable would fail at
	// config load, which is a different refusal and would satisfy this test
	// without the command ever running.
	t.Setenv("BILLET_TEST_UNREACHABLE_DSN",
		"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=1")

	dest := filepath.Join(t.TempDir(), "archive")

	err := runLocalBackup(t.Context(), backupOptions{
		configPath: configPath,
		out:        dest,
		noUpload:   true,
	})

	// A NIL RESULT IS ASSERTED, NEVER SKIPPED. The first version skipped here on
	// the reasoning that nothing can answer on port 1 — which is true and is not
	// the point: a silent `return nil` added anywhere above would ALSO reach this
	// branch, and the gate would report a pass for a command that captured
	// nothing. A skip that stands in for an assertion is the failure mode this
	// whole test exists to catch, one level up.
	if err == nil {
		a, openErr := deployarchive.Open(t.Context(), dest)
		if openErr != nil {
			t.Fatalf("the command reported success and wrote no archive billet can read: %v",
				openErr)
		}

		if !a.Manifest.Ledger.IsExternal() {
			t.Fatal("the command reported success and the archive does not record an external " +
				"ledger")
		}

		return
	}

	// THE BOUNDARY IT HAD TO REACH, not merely a phrase it must avoid. Rejecting
	// the old refusal's wording alone passes against a REWORDED blanket refusal,
	// which is the same defect wearing a different sentence. `server state:` is
	// what runLocalBackup wraps the ledger handle's failure in, so its presence
	// proves execution got past the config and every check before it.
	if !strings.Contains(err.Error(), "server state:") {
		t.Fatalf("`billet local backup` did not reach the ledger on a PostgreSQL deployment, so "+
			"the identity, the node-wire authority and the App key are captured by nothing: %v",
			err)
	}
}

// requireBackupPostgres returns a DSN pointing at a schema of this test's own,
// or skips — and refuses to skip under CI.
//
// A SKIP THAT QUIETLY PASSES IS THE FAILURE THIS FILE IS ABOUT, one level up: the
// defect being guarded is a command that captured nothing while reporting
// success, so a gate that reports success while exercising nothing is the same
// shape. internal/state states the rule for its own suite; this is the command
// layer's copy of it, and the duplication is deliberate — a shared helper would
// have to be exported from a package that has no business exporting one.
func requireBackupPostgres(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("BILLET_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("BILLET_TEST_POSTGRES_DSN is unset under CI, so the one gate that proves " +
				"`billet local backup` reaches the end on a PostgreSQL deployment would go " +
				"unrun while the run reported success")
		}

		t.Skip("BILLET_TEST_POSTGRES_DSN is unset; start a PostgreSQL and set it to exercise " +
			"the end-to-end backup")
	}

	schema := strings.ToLower("billet_test_" + strings.ReplaceAll(t.Name(), "/", "_"))
	if len(schema) > 60 {
		schema = schema[:60]
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open BILLET_TEST_POSTGRES_DSN: %v", err)
	}

	defer func() { _ = admin.Close() }()

	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`,
		`CREATE SCHEMA ` + quoted,
	} {
		if _, err := admin.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("prepare the test schema: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		//nolint:errcheck,noctx // teardown, after the test's context is gone.
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse BILLET_TEST_POSTGRES_DSN: %v", err)
	}

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	return u.String()
}

// AND THE COMMAND RUNS TO THE END ON A REAL POSTGRESQL LEDGER.
//
// THIS IS THE GATE THE CHEAP ONE CANNOT BE. Its sibling proves execution reaches
// openStateAdmin, and MEASURED by mutation that is all it proves: a blanket
// refusal moved BELOW that line survives it, because with no database the
// command stops at the handle either way. The defect this whole pair exists for
// — every line of the feature unreachable behind an early return — could be
// reintroduced one statement further down and go unseen.
//
// So this one connects, migrates, mints an identity and an authority, and drives
// the real `runLocalBackup` to a finished archive. A refusal anywhere in the
// command fails it.
func TestAPostgresBackupWritesAnIdentityOnlyArchive(t *testing.T) {
	dsn := requireBackupPostgres(t)
	f := newBackupFixture(t, false)

	t.Setenv("BILLET_TEST_BACKUP_DSN", dsn)

	body := `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + f.stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_TEST_BACKUP_DSN
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + f.keyPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	// COMMISSIONED THE WAY THE CONTROL PLANE DOES IT: the ledger in PostgreSQL,
	// the identity and the authority on local disk. A fixture that wrote those by
	// hand would not prove the command reads what billet actually produces.
	db, err := state.OpenPostgres(t.Context(), f.stateDir, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	id, err := state.DeploymentID(f.stateDir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(f.stateDir, id); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "archive")

	if err := runLocalBackup(t.Context(), backupOptions{
		configPath: configPath,
		out:        dest,
		noUpload:   true,
	}); err != nil {
		t.Fatalf("`billet local backup` failed on a PostgreSQL deployment, so the identity, "+
			"the node-wire authority and the App key are captured by nothing: %v", err)
	}

	a, err := deployarchive.Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("the archive it wrote does not read back: %v", err)
	}

	if !a.Manifest.Ledger.IsExternal() {
		t.Error("the archive does not record the ledger as external")
	}

	if a.Manifest.Ledger.Backend != "postgres" {
		t.Errorf("the archive names %q as the ledger's engine", a.Manifest.Ledger.Backend)
	}

	if a.Manifest.Ledger.DSNEnv != "BILLET_TEST_BACKUP_DSN" {
		t.Errorf("the archive does not name the DSN variable: %q", a.Manifest.Ledger.DSNEnv)
	}

	if a.Manifest.DeploymentID != id {
		t.Errorf("the archive is of deployment %s and this host is %s", a.Manifest.DeploymentID, id)
	}

	// THE MIGRATIONS CAME FROM THE LIVE DATABASE, which is the one place this
	// path reads a schema from something still moving. An empty list would mean
	// AppliedMigrations answered nothing and nobody noticed.
	if len(a.Manifest.Ledger.Migrations) == 0 {
		t.Error("the archive records no migrations, so the live ledger was never read")
	}
}
