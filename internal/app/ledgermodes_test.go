package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// EACH LEDGER MODE IS THE OPEN ITS NAME SAYS, told apart by what each does: on a
// state directory with no ledger the decision read answers ErrNoLedgerYet and
// the report state.ErrNoLedger, neither creating one; the operator open
// creates and migrates one and, holding no directory lock, admits a second
// operator beside it where the control plane's or the maintenance open would
// be refused; the report's handle refuses to write. And OpenLedgerWith takes
// only the report, with the caller's connection string rather than the
// environment's.
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

	if db, err := OpenLedger(t.Context(), cfg, LedgerDecision); !errors.Is(err, ErrNoLedgerYet) {
		closeUnexpected(t, db)
		t.Fatalf("the decision read of an empty directory answered %v, want ErrNoLedgerYet", err)
	}

	if db, err := OpenLedger(t.Context(), cfg, LedgerInspect); !errors.Is(err, state.ErrNoLedger) {
		closeUnexpected(t, db)
		t.Fatalf("the report of an empty directory answered %v, want state.ErrNoLedger", err)
	}

	if db, err := OpenLedgerWith(t.Context(), cfg, LedgerInspect, ""); !errors.Is(err, state.ErrNoLedger) {
		closeUnexpected(t, db)
		t.Fatalf("the report handed a connection string, of an empty directory, answered %v, want state.ErrNoLedger", err)
	}

	if _, err := os.Lstat(state.LedgerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the decision read or the report created a ledger: %v", err)
	}

	first, err := OpenLedger(t.Context(), cfg, LedgerOperator)
	if err != nil {
		t.Fatalf("the operator open of an empty directory: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	// A SECOND OPERATOR BESIDE THE FIRST: the operator open proceeds without the
	// directory lock another holds, which the control plane's open and the
	// maintenance open each refuse.
	second, err := OpenLedger(t.Context(), cfg, LedgerOperator)
	if err != nil {
		t.Fatalf("a second operator open beside the first: %v", err)
	}

	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := OpenLedgerWith(t.Context(), cfg, LedgerInspect, "")
	if err != nil {
		t.Fatalf("the report of the ledger the operator open created: %v", err)
	}

	t.Cleanup(func() { _ = report.Close() })

	if err := report.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, state.ErrInspect) {
		t.Errorf("the report's handle began a write transaction (%v), want state.ErrInspect", err)
	}

	// ONLY THE REPORT TAKES A CALLER'S CONNECTION STRING, refused by that rule and
	// not by whatever the mode's own open would have said.
	for _, mode := range []LedgerMode{LedgerControlPlane, LedgerStandby, LedgerMaintenance, LedgerOperator, LedgerDecision} {
		db, err := OpenLedgerWith(t.Context(), cfg, mode, "")
		if db != nil {
			_ = db.Close()
		}

		if err == nil || !strings.Contains(err.Error(), "takes no connection string of the caller's") {
			t.Errorf("OpenLedgerWith in mode %d answered %v; only the report takes a caller's connection string", mode, err)
		}
	}

	// AND IT DIALS THAT ONE: a listener this test owns is the caller's string,
	// and it must be connected to.
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	dialled := make(chan struct{}, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		dialled <- struct{}{}

		_ = conn.Close()
	}()

	pg := &config.Config{Server: &config.ServerConfig{IdentityDir: dir, State: &config.StateConfig{
		Backend: config.StatePostgres, Postgres: &config.PostgresStateConfig{DSNEnv: "BILLET_TEST_UNSET_LEDGER_DSN"},
	}}}

	if db, err := OpenLedger(t.Context(), pg, LedgerInspect); err == nil ||
		!strings.Contains(err.Error(), "BILLET_TEST_UNSET_LEDGER_DSN") {
		closeUnexpected(t, db)
		t.Fatalf("the report read its connection string from somewhere other than the environment: %v", err)
	}

	dsn := state.DSN(fmt.Sprintf("postgres://nobody@%s/none?connect_timeout=5&sslmode=disable", listener.Addr()))

	if db, err := OpenLedgerWith(t.Context(), pg, LedgerInspect, dsn); err == nil {
		_ = db.Close()

		t.Fatal("a report over a listener that speaks no PostgreSQL opened")
	}

	select {
	case <-dialled:
	case <-time.After(10 * time.Second):
		t.Fatal("the report was handed a connection string and never dialled it")
	}
}

// closeUnexpected closes a handle an open that should have refused returned, so
// the failure it reports leaves no pool or directory lock to the tests after it.
func closeUnexpected(t *testing.T, db *state.DB) {
	t.Helper()

	if db != nil {
		if err := db.Close(); err != nil {
			t.Errorf("close the handle an open should not have returned: %v", err)
		}
	}
}
