package deployarchive

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// appKeyOnce generates one RSA key for the whole package. Generating 2048 bits
// per test dominates the suite's runtime and proves nothing extra — what the
// tests are about is which BYTES land where, not which key they are.
var appKeyOnce = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), nil
})

// secondAppKey is a DIFFERENT App key, for the tests about never replacing one.
var secondAppKeyOnce = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), nil
})

func appKeyPEM(t *testing.T) []byte {
	t.Helper()

	body, err := appKeyOnce()
	if err != nil {
		t.Fatalf("generate an App key: %v", err)
	}

	return body
}

func otherAppKeyPEM(t *testing.T) []byte {
	t.Helper()

	body, err := secondAppKeyOnce()
	if err != nil {
		t.Fatalf("generate a second App key: %v", err)
	}

	return body
}

// deployment is a control plane's state as it stands on disk: a ledger, an
// identity, an authority and an App key beside a config.
type deployment struct {
	stateDir   string
	configPath string
	appKeyPath string
	id         string
	appKey     []byte
	github     GitHubIdentity
}

// newDeployment builds a REAL one, not a fixture of one.
//
// state.Open creates and migrates the ledger, state.DeploymentID mints the
// identity, and wirecert.LoadOrCreateCA mints the authority and its marker — so
// what the tests capture and restore is the shape the production code produces
// rather than an approximation of it that could drift from it silently.
func newDeployment(t *testing.T) deployment {
	t.Helper()

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")

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

	key := appKeyPEM(t)
	keyPath := filepath.Join(root, "app-private-key.pem")

	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("write the App key: %v", err)
	}

	configPath := filepath.Join(root, "billet.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  state_dir: "+stateDir+"\n"), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return deployment{
		stateDir:   stateDir,
		configPath: configPath,
		appKeyPath: keyPath,
		id:         id,
		appKey:     key,
		github:     GitHubIdentity{Org: "acme", AppID: 42, InstallationID: 4242},
	}
}

// backupTo captures d into dest, exactly as the command does.
func backupTo(t *testing.T, d deployment, dest string) Manifest {
	t.Helper()

	return backupToAt(t, d, dest, nowStub())
}

// backupToAt is backupTo with the clock chosen.
//
// TWO BACKUPS OF AN UNCHANGED DEPLOYMENT ARE OTHERWISE BYTE-IDENTICAL —
// measured: VACUUM INTO is deterministic over the same pages — so a test that
// needs two DISTINCT archives has to move the clock, or it silently ends up
// testing one archive twice.
func backupToAt(t *testing.T, d deployment, dest string, now time.Time) Manifest {
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
		ConfigBody:   []byte("server: {}\n"),
		Snapshot:     db.SnapshotInto,
		Now:          func() time.Time { return now },
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

// emptyTarget is a host prepared but not commissioned: a state directory that
// exists and a config naming an App key path nothing occupies.
type emptyTarget struct {
	stateDir   string
	appKeyPath string
	configPath string
}

func newTarget(t *testing.T, gh GitHubIdentity) Target {
	t.Helper()

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create the target state dir: %v", err)
	}

	e := emptyTarget{
		stateDir:   stateDir,
		appKeyPath: filepath.Join(root, "app-private-key.pem"),
		configPath: filepath.Join(root, "billet.yaml"),
	}

	return Target{
		ConfigPath: e.configPath,
		StateDir:   e.stateDir,
		AppKeyPath: e.appKeyPath,
		GitHub:     gh,
	}
}

// restoreInto runs a full restore, with the App key installed the plain way a
// test can assert on.
func restoreInto(t *testing.T, a *Archive, tgt Target) Result {
	t.Helper()

	plan, err := PlanRestore(t.Context(), a, tgt)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("PlanRestore refused: %v", plan.Refusals)
	}

	res, err := Execute(t.Context(), RestoreRequest{
		Plan:          plan,
		InstallAppKey: testInstallAppKey,
		Now:           func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
		Actor:         "test",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	return res
}

// testInstallAppKey stands in for the command layer's no-clobber installer.
//
// IT REFUSES AN OCCUPIED DESTINATION, which is the property the executor
// depends on — a stand-in that overwrote would make every test about preserving
// an existing key pass for the wrong reason.
func testInstallAppKey(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	if _, err := f.Write(body); err != nil {
		return err
	}

	return f.Sync()
}

// nowStub is the clock every backup in these tests uses.
func nowStub() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func read(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return body
}

func migrationsOf(t *testing.T, ctx context.Context, dbPath string) []state.AppliedMigration {
	t.Helper()

	got, err := state.PeekMigrations(ctx, dbPath)
	if err != nil {
		t.Fatalf("PeekMigrations(%s): %v", dbPath, err)
	}

	return got
}
