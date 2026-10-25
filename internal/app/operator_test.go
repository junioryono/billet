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
	if err := os.WriteFile(keyPath, []byte("not read before the refusal"), 0o600); err != nil {
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

	if _, _, _, err := OpenOperator(t.Context(), cfgPath); err != nil &&
		strings.Contains(err.Error(), "run it as the ledger's owner") {
		t.Errorf("the operator open, which keeps its own rules, was refused as a report: %v", err)
	}
}
