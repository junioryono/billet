package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A WORKSPACE TOO LONG FOR THE NODE IS REFUSED BY `up`, before anything exists.
// The node refuses it only at its own start, by which time `run` has created this
// run's scale sets (a Tart node, 2026-10-25). Driven through the command, because
// the check lives there and deriveAcceptance's tests run under long temporary paths.
func TestAWorkspaceTooLongForTheNodeIsRefusedBeforeAnythingIsMade(t *testing.T) {
	t.Parallel()

	workspace := filepath.Join(t.TempDir(), strings.Repeat("w", 120))
	base := writeAcceptanceBase(t, acceptanceBaseConfig)

	var out bytes.Buffer
	err := cmdAcceptanceUp(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv},
		[]string{"--config", base, "--workspace", workspace})
	if err == nil || !strings.Contains(err.Error(), "choose a shorter one") ||
		!strings.Contains(err.Error(), "upgrade answer socket") {
		t.Fatalf("up = %v, want a refusal naming the workspace and the socket", err)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Errorf("a refused workspace was created anyway (%v)", statErr)
	}
}

// A RUN REFUSES A SEALED WORKSPACE, AND LIFTS NOTHING. With --no-teardown a run's
// seal stays in the ledger, and the next run was assigned nothing for its whole
// --wait (2026-10-25). Its own seal is refused like any other: another run may still
// be draining behind it, and an operator's drain over it leaves it reading the same.
func TestARunRefusesASealedWorkspaceAndLiftsNothing(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"billet acceptance run", "a maintenance window", "billet acceptance down"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
			ledgertest.Seed(t, filepath.Join(filepath.Dir(ws.ConfigPath), "server"))

			var out bytes.Buffer
			sealWith(t, cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, ws, reason)
			before := admissionOf(t, ws)

			err := refuseASealedWorkspace(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, ws)
			if err == nil || !strings.Contains(err.Error(), "billet resume --config") ||
				!strings.Contains(err.Error(), reason) {
				t.Fatalf("refuse = %v, want a refusal naming the seal and the way out", err)
			}
			if after := admissionOf(t, ws); !after.Sealed() || after.Generation != before.Generation {
				t.Errorf("admission moved from %+v to %+v; a refusal writes nothing", before, after)
			}
		})
	}
}

// THE RUN ITSELF ASKS, before anything it would have to tear down exists. Driven
// through runAcceptance, since a refusal nothing calls refuses nothing.
func TestARunOnASealedWorkspaceStartsNothing(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	ledgertest.Seed(t, filepath.Join(filepath.Dir(ws.ConfigPath), "server"))

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	sealWith(t, env, ws, "billet acceptance run")
	out.Reset()

	err := runAcceptance(t.Context(), env, ws, acceptanceRunOptions{jobs: 1, wait: time.Second})
	if err == nil || !strings.Contains(err.Error(), "billet resume --config") {
		t.Fatalf("run = %v, want the refusal", err)
	}
	if strings.Contains(out.String(), "started billet") {
		t.Errorf("a refused run started a service:\n%s", out.String())
	}
}

// A WORKSPACE THAT HAS NEVER RUN HAS NO SEAL, and looking for one must not make a
// ledger the server would then find already there.
func TestAWorkspaceThatHasNeverRunIsLeftWithoutALedger(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	if err := refuseASealedWorkspace(t.Context(), env, ws); err != nil {
		t.Fatalf("refuse = %v", err)
	}
	ledger := state.LedgerPath(filepath.Join(filepath.Dir(ws.ConfigPath), "server"))
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Errorf("looking for a seal made the ledger (%v)", err)
	}
}

// AN OPEN DEPLOYMENT RUNS, and nothing is written: every write bumps admission's
// generation.
func TestAnOpenDeploymentIsNotRefused(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	ledgertest.Seed(t, filepath.Join(filepath.Dir(ws.ConfigPath), "server"))
	before := admissionOf(t, ws)

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	if err := refuseASealedWorkspace(t.Context(), env, ws); err != nil {
		t.Fatalf("refuse = %v", err)
	}
	if after := admissionOf(t, ws); after.Generation != before.Generation {
		t.Errorf("generation moved from %d to %d on an open deployment", before.Generation, after.Generation)
	}
}

// A LEDGER BEHIND THIS BINARY IS NOT MIGRATED BY ASKING WHETHER IT IS SEALED. A newer
// binary refusing to run must leave the workspace readable by the one that wrote it,
// and since it cannot tell without that migration it says so rather than answer "open".
func TestAskingWhetherAWorkspaceIsSealedMigratesNothing(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	stateDir := filepath.Join(filepath.Dir(ws.ConfigPath), "server")
	ledgertest.Seed(t, stateDir)
	// Opened once as a server would have, which is what makes it the WAL ledger a real
	// workspace holds, and an inspection cannot.
	admissionOf(t, ws)

	raw, err := sql.Open("sqlite", "file:"+state.LedgerPath(stateDir))
	if err != nil {
		t.Fatalf("open the ledger directly: %v", err)
	}
	if _, err := raw.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = ?`,
		state.LatestSchemaVersion()); err != nil {
		t.Fatalf("put the ledger one migration behind: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}
	before, err := state.PeekMigrations(t.Context(), state.LedgerPath(stateDir))
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}

	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
	if err := refuseASealedWorkspace(t.Context(), env, ws); err != nil {
		t.Fatalf("refuse = %v, want the run to go on: the server migrates this", err)
	}
	if !strings.Contains(out.String(), "cannot be read") {
		t.Errorf("said %q, want it to say it could not tell", out.String())
	}

	after, err := state.PeekMigrations(t.Context(), state.LedgerPath(stateDir))
	if err != nil {
		t.Fatalf("read the migrations again: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("asking moved the ledger from %d migrations to %d", len(before), len(after))
	}
}

func sealWith(t *testing.T, env cli.Env, ws acceptanceWorkspace, reason string) {
	t.Helper()

	db, _, err := fleetops.OpenLedgerForAdmission(t.Context(), ws.ConfigPath)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the ledger: %v", err)
		}
	}()

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}
	if _, err := fleetops.TakeTheSeal(t.Context(), env, db, current, reason); err != nil {
		t.Fatalf("seal: %v", err)
	}
}

func admissionOf(t *testing.T, ws acceptanceWorkspace) state.Admission {
	t.Helper()

	db, _, err := fleetops.OpenLedgerForAdmission(t.Context(), ws.ConfigPath)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the ledger: %v", err)
		}
	}()

	current, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	return current
}
