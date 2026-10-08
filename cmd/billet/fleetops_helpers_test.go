package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// Helpers cmd/billet's tests share with internal/ops/fleetops's, copied rather
// than imported: a test helper is not part of any package's API.

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

// THE CLI LAYER HAD NO TEST, AND BOTH ITS BUGS LIVED THERE.
//
// `billet ca issue <node>` could not run at all: the shared flag parser rejects
// positional arguments, which is right for every other command and wrong for
// this one. And once that was fixed, `billet ca issue epyc-1 --config x.yaml`
// silently ignored the config path, because Go's flag package stops parsing at
// the first positional — so the command read the DEFAULT config file and issued
// against whatever deployment that named.
//
// Neither is reachable from the packages underneath, both are on the path an
// operator takes exactly once per node, and the failure of the second is silent.
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

// nodeConfigWithoutName is nodeConfigFor with the name left to the certificate.
func nodeConfigWithoutName(t *testing.T, stateDir, bundleDir string) *config.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `
node:
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: ` + stateDir + `
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("a node config that leaves its name to the certificate was refused: %v", err)
	}

	return cfg
}

func admissionNow(t *testing.T, db *state.DB) state.Admission {
	t.Helper()

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	return a
}
