package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// EACH LEDGER MODE IS THE OPEN ITS NAME SAYS, told apart by what each does to a
// state directory that holds no ledger: the decision read answers ErrNoLedgerYet
// and creates nothing, the report answers state.ErrNoLedger and creates
// nothing, and the operator open creates and migrates one, which the report can
// then read. And OpenLedgerWith takes only the report, with the caller's
// connection string rather than the environment's.
//
// NOT PARALLEL: it pins retirement's platform, so the operator open takes the
// inner lock alone as it does off Linux rather than refusing an unprepared host.
func TestEachLedgerModeIsTheOpenItsNameSays(t *testing.T) {
	oldRoot, oldPlatform := retirement.Root, retirement.Platform
	retirement.Root, retirement.Platform = t.TempDir(), "darwin"

	t.Cleanup(func() { retirement.Root, retirement.Platform = oldRoot, oldPlatform })

	dir := filepath.Join(t.TempDir(), "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{IdentityDir: dir}}

	if _, err := OpenLedger(t.Context(), cfg, LedgerDecision); !errors.Is(err, ErrNoLedgerYet) {
		t.Fatalf("the decision read of an empty directory answered %v, want ErrNoLedgerYet", err)
	}

	if _, err := OpenLedger(t.Context(), cfg, LedgerInspect); !errors.Is(err, state.ErrNoLedger) {
		t.Fatalf("the report of an empty directory answered %v, want state.ErrNoLedger", err)
	}

	if _, err := os.Lstat(state.LedgerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the decision read or the report created a ledger: %v", err)
	}

	db, err := OpenLedger(t.Context(), cfg, LedgerOperator)
	if err != nil {
		t.Fatalf("the operator open of an empty directory: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := OpenLedgerWith(t.Context(), cfg, LedgerInspect, "")
	if err != nil {
		t.Fatalf("the report of the ledger the operator open created: %v", err)
	}

	if err := report.Close(); err != nil {
		t.Fatal(err)
	}

	// ONLY THE REPORT TAKES A CALLER'S CONNECTION STRING.
	for _, mode := range []LedgerMode{LedgerControlPlane, LedgerStandby, LedgerMaintenance, LedgerOperator, LedgerDecision} {
		if db, err := OpenLedgerWith(t.Context(), cfg, mode, ""); err == nil {
			_ = db.Close()

			t.Errorf("OpenLedgerWith opened mode %d; only the report takes a caller's connection string", mode)
		}
	}

	// AND IT USES THAT ONE: on a PostgreSQL deployment whose variable is unset,
	// the environment's open refuses naming the variable, and the caller's
	// string is the one dialled instead.
	pg := &config.Config{Server: &config.ServerConfig{IdentityDir: dir, State: &config.StateConfig{
		Backend: config.StatePostgres, Postgres: &config.PostgresStateConfig{DSNEnv: "BILLET_TEST_UNSET_LEDGER_DSN"},
	}}}

	if _, err := OpenLedger(t.Context(), pg, LedgerInspect); err == nil ||
		!strings.Contains(err.Error(), "BILLET_TEST_UNSET_LEDGER_DSN") {
		t.Fatalf("the report read its connection string from somewhere other than the environment: %v", err)
	}

	if _, err := OpenLedgerWith(t.Context(), pg, LedgerInspect,
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1"); err == nil ||
		strings.Contains(err.Error(), "BILLET_TEST_UNSET_LEDGER_DSN") {
		t.Fatalf("the report was handed a connection string and did not dial it: %v", err)
	}
}
