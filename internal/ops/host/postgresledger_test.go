package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A DATA SOURCE NAMING SEVERAL SERVERS IS REFUSED WHEN A RESTORE, A RECOVERY OR
// AN ABANDON BUILDS ITS DATABASE, before the command seals, fences or copies
// anything: each writes through several connections that could reach different
// servers. Nothing here connects, so it needs no PostgreSQL.
func TestTheArchivesDatabaseIsRefusedThroughSeveralServers(t *testing.T) {
	f := newBackupFixture(t, false)

	body := `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + f.stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_TEST_SEVERAL_DSN
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

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	t.Setenv("BILLET_TEST_SEVERAL_DSN",
		"postgres://billet:pw@pg1.internal:5432,pg2.internal:5432/billet?sslmode=disable")

	if _, err := archiveTargetLedger(cfg, true); err == nil || !strings.Contains(err.Error(), "more than one server") {
		t.Fatalf("archiveTargetLedger through two servers = %v, want a refusal", err)
	}

	// AN ABANDON IS NOT REFUSED FOR IT: it writes at most one put-back, which
	// pgdump.Restore guards itself, and a restore's abandon writes nothing.
	if _, err := archiveTargetLedger(cfg, false); err != nil {
		t.Fatalf("archiveTargetLedger for an abandon through two servers: %v", err)
	}

	t.Setenv("BILLET_TEST_SEVERAL_DSN", "postgres://billet:pw@pg1.internal:5432/billet?sslmode=disable")

	if _, err := archiveTargetLedger(cfg, true); err != nil {
		t.Fatalf("archiveTargetLedger through one server: %v", err)
	}
}
