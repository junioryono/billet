package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestTheNodeUpgraderUsesTheConfiguredLedgerVariable(t *testing.T) {
	cfg := &config.Config{
		Node: &config.NodeConfig{StateDir: "/tmp/billet-node"},
		Server: &config.ServerConfig{State: &config.StateConfig{
			Backend:  config.StatePostgres,
			Postgres: &config.PostgresStateConfig{DSNEnv: "CUSTOM_LEDGER_DSN"},
		}},
	}

	upgrader, err := nodeUpgrader(cfg, "/etc/billet/custom.yaml")
	if err != nil {
		t.Fatal(err)
	}

	if upgrader.DSNEnv != "CUSTOM_LEDGER_DSN" || upgrader.AckDir != cfg.Node.StateDir ||
		upgrader.ConfigPath != "/etc/billet/custom.yaml" {
		t.Errorf("upgrader dropped its host configuration: %+v", upgrader)
	}

	cfg.Server = nil
	upgrader, err = nodeUpgrader(cfg, "")
	if err != nil || upgrader.DSNEnv != "" {
		t.Errorf("a node-only host unexpectedly needs a ledger environment: %+v, %v", upgrader, err)
	}
}

// Drive startup before any provider or identity is opened. Missing certificates
// give a removed guard a deterministic different error, without starting compute.
func TestNodeStartupRejectsAnUnusableUpgradeSocketPath(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, strings.Repeat("x", 120))
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "billet.yaml")
	body := "node:\n  name: test-node\n  server_addr: 127.0.0.1:7717\n" +
		"  provider: docker\n  state_dir: " + stateDir + "\n" +
		"  tls:\n    cert: /missing-node.crt\n    key: /missing-node.key\n    ca: /missing-ca.crt\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdNode(t.Context(), newLifecycle(func() {}), []string{"--config", path, "--upgrade-probe"})
	if err == nil || !strings.Contains(err.Error(), "use a shorter node.state_dir path") {
		t.Fatalf("node startup did not diagnose its unusable upgrade socket path: %v", err)
	}
}
