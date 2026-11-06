package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A REPORT ON A SQLITE LEDGER RUNS AS THE DIRECTORY'S OWNER AND AS NOBODY
// ELSE, root included, because its read-only open would leave sidecars the
// owner cannot write. A PostgreSQL ledger has none, and a directory that does
// not exist is the open's to refuse.
//
// NOT PARALLEL: reportEUID is the package's.
func TestAReportOnASQLiteLedgerRunsOnlyAsItsOwner(t *testing.T) {
	dir := t.TempDir()
	sqlite := &config.Config{Server: &config.ServerConfig{IdentityDir: dir}}

	if err := reportRunsAsOwner(sqlite); err != nil {
		t.Fatalf("the directory's own owner was refused: %v", err)
	}

	owner := os.Geteuid()
	t.Cleanup(func() { reportEUID = os.Geteuid })

	for _, euid := range []int{owner + 1, 0} {
		if euid == owner {
			continue
		}

		reportEUID = func() int { return euid }

		err := reportRunsAsOwner(sqlite)
		if err == nil || !strings.Contains(err.Error(), "run it as the ledger's owner") {
			t.Errorf("uid %d over a directory uid %d owns gave %v; want it refused naming the owner", euid,
				owner, err)
		}
	}

	// JUDGED WHERE THE LEDGER IS OPENED: a link followed by `..` in the path as
	// spelled resolves beside the link's target, while the ledger's joined path
	// cleans the `..` away and opens beside the link. The second is where the
	// sidecars would be created.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "child"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	spelled := &config.Config{Server: &config.ServerConfig{IdentityDir: filepath.Join(dir, "link") + "/.."}}

	got, err := os.Stat(reportLedgerDir(spelled))
	if err != nil {
		t.Fatalf("stat the directory the report judges: %v", err)
	}
	opened, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := os.Stat(spelled.Server.IdentityDir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, opened) || os.SameFile(got, resolved) {
		t.Errorf("the report judges %s; want %s, where the ledger is opened, not %s, where the spelled path "+
			"resolves", reportLedgerDir(spelled), dir, elsewhere)
	}

	// AND THE CHECK ASKS THAT DIRECTORY. The ledger's own open would refuse
	// this path anyway (its spelled directory does not exist); it is here
	// because it tells the two directories apart without a second account: as
	// spelled, nothing is there to judge, and where the ledger would be opened,
	// another account owns the directory.
	reportEUID = func() int { return owner + 1 }
	cleaned := &config.Config{Server: &config.ServerConfig{IdentityDir: filepath.Join(dir, "absent") + "/.."}}
	if err := reportRunsAsOwner(cleaned); err == nil || !strings.Contains(err.Error(), "run it as the ledger's owner") {
		t.Errorf("a path whose ledger would be opened in another account's directory gave %v; want it refused", err)
	}
	reportEUID = os.Geteuid

	postgres := &config.Config{Server: &config.ServerConfig{IdentityDir: dir,
		State: &config.StateConfig{Backend: config.StatePostgres}}}
	if err := reportRunsAsOwner(postgres); err != nil {
		t.Errorf("a PostgreSQL ledger, which has no sidecar, was refused: %v", err)
	}

	gone := &config.Config{Server: &config.ServerConfig{IdentityDir: filepath.Join(dir, "absent")}}
	if err := reportRunsAsOwner(gone); err != nil {
		t.Errorf("a directory that does not exist was refused here rather than left to the open: %v", err)
	}
}

// AND THE REPORT'S OPEN ASKS IT, before the ledger is opened.
func TestOpenReportRefusesAnotherAccountBeforeOpening(t *testing.T) {
	dir, conf := t.TempDir(), t.TempDir()
	keyPath := filepath.Join(conf, "app.pem")
	if err := os.WriteFile(keyPath, []byte("a key nothing here reads"), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	cfgPath := filepath.Join(conf, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: `+keyPath+`
server:
  listen: 127.0.0.1:7717
  state_dir: `+dir+`
  max_vcpu: 8
  max_memory: 32GiB
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Cleanup(func() { reportEUID = os.Geteuid })
	reportEUID = func() int { return os.Geteuid() + 1 }

	if _, _, err := OpenReport(t.Context(), cfgPath); err == nil ||
		!strings.Contains(err.Error(), "run it as the ledger's owner") {
		t.Errorf("OpenReport as another account gave %v; want the owner refusal", err)
	}

	// THE OPERATOR OPEN KEEPS ITS OWN RULES, which create the ledger in an empty
	// directory.
	_, _, closeDB, err := OpenOperator(t.Context(), cfgPath)
	if err != nil {
		t.Fatalf("the operator open as another account failed: %v", err)
	}
	closeDB()
}
