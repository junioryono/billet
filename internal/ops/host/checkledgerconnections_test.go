package host

import (
	"os"
	"strings"
	"testing"
)

// `billet check` SAYS WHAT A POSTGRESQL DEPLOYMENT HOLDS, from the operator's own
// budget, so packing deployments on one server is arithmetic an operator can do.
func TestCheckStatesThePostgresConnectionsADeploymentHolds(t *testing.T) {
	stubGitHubUnverifiable(t)

	cfgPath, _, _ := commissionPostgresBackup(t)

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}

	const dsnLine = "      dsn_env: BILLET_TEST_BACKUP_DSN\n"
	if !strings.Contains(string(body), dsnLine) {
		t.Fatalf("the commissioned config has no %q:\n%s", dsnLine, body)
	}

	budgeted := strings.Replace(string(body), dsnLine, dsnLine+"      max_connections: 4\n", 1)
	if err := os.WriteFile(cfgPath, []byte(budgeted), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	var checkErr error

	out := capture(t, func() { checkErr = Check(t.Context(), processEnv(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("check: %v\n%s", checkErr, out)
	}

	for _, want := range []string{"up to 4 connections per process", "2 readers", "transaction-mode pooler"} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not say %q:\n%s", want, out)
		}
	}
}

// A LEDGER THAT IS A FILE HOLDS NO SERVER CONNECTIONS, and check says nothing
// about them.
func TestCheckCountsNoConnectionsOnASQLiteLedger(t *testing.T) {
	stubGitHubUnverifiable(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfgPath := writeUntrustedCheckConfig(t, "billet-first")

	var checkErr error

	out := capture(t, func() { checkErr = Check(t.Context(), processEnv(), []string{"--config", cfgPath}) })
	if checkErr != nil {
		t.Fatalf("check: %v\n%s", checkErr, out)
	}

	if !strings.Contains(out, "state    ") {
		t.Fatalf("check did not reach the state line, so this proves nothing:\n%s", out)
	}

	if strings.Contains(out, "ledger   ") {
		t.Errorf("check counts server connections on a SQLite ledger:\n%s", out)
	}
}
