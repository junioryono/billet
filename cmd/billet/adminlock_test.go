package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// writeAdminConfig is writeCAConfig with a REAL App private key, which cmdCheck
// insists on before it ever reaches the state directory.
func writeAdminConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app.pem")

	if err := os.WriteFile(keyPath, testKey(t), 0o600); err != nil {
		t.Fatalf("write the app key: %v", err)
	}

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
  private_key_path: ` + keyPath + `
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

// THE COMMANDS THEMSELVES MUST WORK WHILE THE CONTROL PLANE IS RUNNING.
//
// state.OpenAdmin having the right behaviour proves nothing about whether the
// commands were rewired onto it — and the wiring is the whole defect. Every
// command below reached the ledger through Open, which takes the exclusive
// directory lock the server holds for its entire life, so all of them failed
// against a live deployment with "another billet process holds this state
// directory".
//
// Driven through the cmd functions rather than through controlPlaneAllocator,
// because calling the helper directly would stay green with the production call
// sites reverted to Open, which is exactly the regression this guards.
func TestOperatorCommandsRunWhileTheControlPlaneIsRunning(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	// The control plane, holding the directory exactly as `billet server` does
	// for its whole life.
	server, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	// The guard that made this a defect rather than a preference: a second
	// control plane really is refused, and must stay refused.
	if _, err := state.Open(ctx, stateDir); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("a second control plane must still be refused, got: %v", err)
	}

	// A READ and a WRITE, because they fail for the same reason but a fix could
	// plausibly cover only one. `ca token` inserts a join token; `nodes pending`
	// only queries.
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"nodes pending", func() error { return cmdNodesPending(ctx, []string{"--config", cfg}) }},
		{"ca token", func() error { return cmdCAToken(ctx, []string{"--config", cfg}) }},
		{"ca revocations", func() error { return cmdCARevocations(ctx, []string{"--config", cfg}) }},
		{"status", func() error { return cmdStatus(ctx, []string{"--config", cfg}) }},
		{"leases", func() error { return cmdLeases(ctx, []string{"held", "--config", cfg}) }},
		{"leases quarantined", func() error { return cmdLeasesQuarantined(ctx, []string{"--config", cfg}) }},
	} {
		if err := tc.run(); err != nil {
			t.Errorf("billet %s against a running control plane: %v", tc.name, err)
		}
	}

	// AND THE WRITE LANDED WHERE THE SERVER CAN SEE IT. A command that opens,
	// reports success and writes somewhere else would satisfy every assertion
	// above.
	var tokens int

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM join_tokens`).Scan(&tokens); err != nil {
		t.Fatalf("count the join tokens the server can see: %v", err)
	}

	if tokens != 1 {
		t.Errorf("join_tokens = %d, want 1 minted by `billet ca token`", tokens)
	}
}

// THE TWO COMMANDS THAT OPEN THE LEDGER DIRECTLY, rather than through
// controlPlaneAllocator.
//
// Separate from the test above because they need real fixtures — an App key for
// `check`, an issued certificate for `ca revoke` — and because without them
// either call site could be reverted to state.Open while everything else stayed
// green, which is the regression this pair exists to catch.
func TestTheDirectLedgerCommandsRunWhileTheControlPlaneIsRunning(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeAdminConfig(t, stateDir)
	ctx := t.Context()

	// A certificate for `ca revoke --cert` to name, issued before the server
	// takes the directory so the fixture is not itself part of what is measured.
	deployment, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	ca, err := wirecert.LoadOrCreateCA(stateDir, deployment)
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	bundleDir := t.TempDir()
	if err := bundle.Write(bundleDir); err != nil {
		t.Fatalf("write the bundle: %v", err)
	}

	server, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	if err := cmdCheck(ctx, []string{"--config", cfg}); err != nil {
		t.Errorf("billet check against a running control plane: %v", err)
	}

	if err := cmdCARevoke(ctx, []string{
		"epyc-1", "--config", cfg, "--cert", filepath.Join(bundleDir, "node.crt"),
	}); err != nil {
		t.Errorf("billet ca revoke against a running control plane: %v", err)
	}

	// AND THE REVOCATION LANDED WHERE THE SERVER READS IT, which is the whole
	// point of being allowed to run at all.
	var revoked int

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM revoked_certs WHERE node = ?`, "epyc-1").Scan(&revoked); err != nil {
		t.Fatalf("count revocations the server can see: %v", err)
	}

	if revoked != 1 {
		t.Errorf("revoked_certs for epyc-1 = %d, want 1", revoked)
	}
}

// A READ-ONLY COMMAND MUST NOT WAIT ON A SCHEDULING DECISION.
//
// Every write transaction begins IMMEDIATE, so a read routed through DB.Tx takes
// SQLite's single writer slot and queues behind the control plane. These two
// commands only read, and they are the ones an operator runs while wondering why
// capacity is missing — exactly when the plane is busy. This drives them with a
// server transaction genuinely open, which is what a reverted View migration
// would fail.
func TestAReadOnlyCommandRunsWhileTheServerHoldsTheWriteLock(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	server, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	holding := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)

	go func() {
		held <- server.Tx(ctx, func(*sql.Tx) error {
			// The writer slot is now genuinely taken, and stays taken until this
			// test says otherwise. That is what makes the ordering deterministic.
			close(holding)
			<-release

			return nil
		})
	}()

	// SELECT, not a bare receive: if the transaction fails BEFORE reaching its
	// callback, nothing ever closes `holding` and the test would hang until the
	// suite timed out — reporting a wedge instead of the error that caused it.
	select {
	case <-holding:
	case err := <-held:
		t.Fatalf("the server's transaction ended before it took the lock: %v", err)
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"leases quarantined", func() error { return cmdLeasesQuarantined(ctx, []string{"--config", cfg}) }},
		{"ca revocations", func() error { return cmdCARevocations(ctx, []string{"--config", cfg}) }},
	} {
		if err := tc.run(); err != nil {
			t.Errorf("billet %s while the server holds the write lock: %v", tc.name, err)
		}
	}

	close(release)

	if err := <-held; err != nil {
		t.Fatalf("the server's transaction: %v", err)
	}
}
