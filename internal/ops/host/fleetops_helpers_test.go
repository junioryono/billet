package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// Helpers this package's tests share with internal/ops/fleetops's and
// cmd/billet's, copied rather than imported: a test helper is not part of any
// package's API.

// drainFixture opens a ledger the way a running control plane holds it, and
// returns the config path an operator command is given.
//
// THE LEDGER STAYS OPEN THROUGH state.Open, which is the interesting case rather
// than a convenience: `billet drain` exists to be run against a LIVE deployment,
// so every one of these tests exercises the admin path that proceeds without the
// directory lock. Opening it here first is what makes that true.
func drainFixture(t *testing.T) (*state.DB, string) {
	t.Helper()

	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db, cfg
}

// outstandingLease makes the deployment hold something, so a drain has to wait.
func outstandingLease(t *testing.T, db *state.DB, cfg *config.Config) *alloc.Lease {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "drain-host", Provider: config.ProviderDocker,
		VCPU: 1 << 20, Memory: 1 << 20 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	lease, err := a.Reserve(t.Context(), cfg.Tiers[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "drain-host"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, 77); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	return lease
}

func loadFixtureConfig(t *testing.T, cfgPath string) *config.Config {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	return cfg
}

// writeCAConfig writes a control-plane config with one docker tier, the shape
// the CA and status commands are run against.
func writeCAConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

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
  private_key_path: /tmp/key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

func admissionNow(t *testing.T, db *state.DB) state.Admission {
	t.Helper()

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	return a
}
