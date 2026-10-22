package host

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// The commands on a deployment whose ledger is in PostgreSQL, driven the way an
// operator drives them, against a real server and the real pg_dump and
// pg_restore.

// pgFixture is a commissioned PostgreSQL deployment: the identity and the
// authority in the identity directory, the ledger in a schema of the test's
// own, bound to the identity by a controller's claim.
type pgFixture struct {
	backupFixture
	dsn string
}

// pgDSNEnv is the variable the fixture's config names for the connection.
const pgDSNEnv = "BILLET_TEST_COMMAND_DSN"

// requirePgClient skips without the PostgreSQL client programs, and refuses to
// under CI, where the job that sets the DSN installs them.
func requirePgClient(t *testing.T) {
	t.Helper()

	for _, program := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(program); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is not installed under CI, so the PostgreSQL commands would go untested",
					program)
			}

			t.Skipf("%s is not installed", program)
		}
	}
}

// pgConfig is a config naming an external ledger for an identity directory.
func pgConfig(t *testing.T, identityDir, keyPath string) string {
	t.Helper()

	body := `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + identityDir + `
  max_vcpu: 8
  max_memory: 32GiB
  state:
    backend: postgres
    postgres:
      dsn_env: ` + pgDSNEnv + `
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

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return path
}

func newPostgresFixture(t *testing.T) pgFixture {
	t.Helper()

	dsn := requireBackupPostgres(t)
	requirePgClient(t)

	t.Setenv(pgDSNEnv, dsn)

	f := newBackupFixture(t, false)
	f.configPath = pgConfig(t, f.stateDir, f.keyPath)

	db, err := state.OpenPostgres(t.Context(), f.stateDir, state.DSN(dsn))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	id, err := state.DeploymentID(f.stateDir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	// BOUND, as a controller's first claim binds a deployment's ledger: what a
	// restore keeps and a recovery replaces is a ledger that says whose it is.
	if _, err := db.ClaimController(t.Context(), "pg-host", id); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(f.stateDir, id); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	f.deployment = id

	return pgFixture{backupFixture: f, dsn: dsn}
}

// pgAllocator is the fixture's allocator over its PostgreSQL ledger, as an
// operator command opens it.
//
// FROM A DIRECTORY OF ITS OWN, not the fixture's identity directory: the rows
// are the database's, and an interrupted recovery leaves that directory fenced,
// which is exactly when a test has to look at what the database holds.
func (f pgFixture) pgAllocator(t *testing.T) (*alloc.Allocator, func()) {
	t.Helper()

	db, err := state.OpenPostgresAdmin(t.Context(), t.TempDir(), state.DSN(f.dsn))
	if err != nil {
		t.Fatalf("OpenPostgresAdmin: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
		}})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	return a, func() { _ = db.Close() }
}

// laterWork is a lease taken AFTER the backup and still held: the archive has
// never heard of it, so whether it survives says whose ledger is in place. A
// recovery has to be told to strand it (--accept-failing-jobs), which is the
// operation's real shape; a released lease is removed and would prove nothing.
func (f pgFixture) laterWork(t *testing.T) string {
	t.Helper()

	a, done := f.pgAllocator(t)
	defer done()

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "pg-node", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "billet-2vcpu")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	return lease.ID
}

// knows reports whether the ledger in the database has a lease.
func (f pgFixture) knows(t *testing.T, leaseID string) bool {
	t.Helper()

	a, done := f.pgAllocator(t)
	defer done()

	_, err := a.Lease(t.Context(), leaseID)

	switch {
	case err == nil:
		return true
	case errors.Is(err, alloc.ErrLeaseNotFound):
		return false
	default:
		t.Fatalf("Lease: %v", err)

		return false
	}
}

// keptCopies lists the files a PostgreSQL recovery keeps in the identity
// directory.
func keptCopies(t *testing.T, dir, suffix string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "ledger.pgdump.superseded-*"+suffix))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	return matches
}

// A POSTGRESQL RECOVERY REPLACES THE LEDGER IN THE DATABASE WITH THE ARCHIVE'S,
// keeps a dump of the one it replaced, and leaves the deployment sealed by an
// operator, read through the database. Until this existed `local recover`
// refused such a deployment outright.
func TestAPostgresRecoveryReplacesTheLedgerAndKeepsADumpOfIt(t *testing.T) {
	stubLifecycleLock(t)

	f := newPostgresFixture(t)
	archive := backupInto(t, f.backupFixture)

	later := f.laterWork(t)
	if !f.knows(t, later) {
		t.Fatal("the work taken after the backup is not in the ledger, so its absence would prove nothing")
	}

	var out bytes.Buffer

	env := processEnv()
	env.Stdout = &out

	if err := cmdLocalRecover(t.Context(), env, []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced", "--accept-failing-jobs",
	}); err != nil {
		t.Fatalf("billet local recover: %v\n%s", err, out.String())
	}

	if f.knows(t, later) {
		t.Error("the ledger in the database still knows the work taken after the backup, so the " +
			"archive's ledger is not the one in place")
	}

	copies := keptCopies(t, f.stateDir, "")
	if len(copies) != 1 {
		t.Fatalf("the recovery kept %v, want one dump of the ledger it replaced", copies)
	}

	if info, err := os.Stat(copies[0]); err != nil || info.Size() == 0 {
		t.Errorf("the kept dump is %v (%v)", info, err)
	}

	a, err := state.PeekPostgresAdmission(t.Context(), state.DSN(f.dsn))
	if err != nil {
		t.Fatalf("PeekPostgresAdmission: %v", err)
	}

	if a.Mode != state.AdmissionSealed || a.Provenance != state.ProvenanceOperator {
		t.Errorf("the recovered deployment's admission is %+v, want an operator's seal", a)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the recovery left the identity directory fenced (%v)", err)
	}

	if !strings.Contains(out.String(), "pg_dump copy") {
		t.Errorf("the recovery did not say where the replaced ledger was kept:\n%s", out.String())
	}
}

// AN ABANDONED POSTGRESQL RECOVERY PUTS THE LEDGER BACK from the dump it kept,
// staged the way stopAfterTheLedger stages it for SQLite: the App key is
// published after the ledger, so a directory billet cannot write stops the run
// with the archive's ledger already in the database.
func TestAnAbandonedPostgresRecoveryPutsTheLedgerBack(t *testing.T) {
	stubLifecycleLock(t)

	f := newPostgresFixture(t)
	archive := backupInto(t, f.backupFixture)
	later := f.laterWork(t)

	stopAfterTheLedger(t, f.backupFixture)

	if err := cmdLocalRecover(t.Context(), processEnv(), []string{
		"--config", f.configPath, "--from", archive, "--old-controller-fenced", "--accept-failing-jobs",
	}); err == nil {
		t.Fatal("the interrupted recovery reported success")
	}

	if f.knows(t, later) {
		t.Fatal("the interruption did not get as far as replacing the ledger, so the abandon " +
			"would have nothing to put back")
	}

	if err := cmdLocalRecover(t.Context(), processEnv(), []string{
		"--config", f.configPath, "--from", archive, "--abandon",
	}); err != nil {
		t.Fatalf("billet local recover --abandon: %v", err)
	}

	if !f.knows(t, later) {
		t.Error("the abandon did not put back the ledger that knew the later work")
	}

	if got := keptCopies(t, f.stateDir, ".abandoned-*"); len(got) != 1 {
		t.Errorf("the dump the abandon restored from was not kept aside: %v", got)
	}

	if _, err := os.Lstat(state.MaintenanceFencePath(f.stateDir)); !os.IsNotExist(err) {
		t.Errorf("the abandon left the identity directory fenced (%v)", err)
	}
}

// A RESTORE ONTO A FRESH HOST KEEPS THE DATABASE'S OWN LEDGER when it is this
// deployment's: the host died, the database did not, and its rows are newer than
// the archive's. The identity, the authority and the App key come back; the
// ledger is left exactly as it is, and billet needed no assertion to know whose
// it is, because it read the binding.
func TestAPostgresRestoreOntoAFreshHostKeepsTheDatabasesOwnLedger(t *testing.T) {
	stubLifecycleLock(t)

	f := newPostgresFixture(t)
	archive := backupInto(t, f.backupFixture)
	later := f.laterWork(t)

	fresh := filepath.Join(t.TempDir(), "identity")
	freshKey := filepath.Join(t.TempDir(), "app-private-key.pem")

	var out bytes.Buffer

	env := processEnv()
	env.Stdout = &out

	if err := cmdLocalRestore(t.Context(), env, []string{
		"--config", pgConfig(t, fresh, freshKey), "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v\n%s", err, out.String())
	}

	if !f.knows(t, later) {
		t.Error("the restore replaced the database's newer ledger with the archive's")
	}

	id, found, err := state.PeekDeploymentID(fresh)
	if err != nil || !found || id != f.deployment {
		t.Errorf("the fresh host is deployment %q (found %v, %v), want %s", id, found, err, f.deployment)
	}

	if !strings.Contains(out.String(), "KEEP the ledger") {
		t.Errorf("the restore did not say it keeps the database's ledger:\n%s", out.String())
	}
}
