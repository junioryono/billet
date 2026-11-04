package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A RUN REOPENS ONLY THE SEAL AN EARLIER RUN LEFT. With --no-teardown that seal
// stays in the ledger, and the next run was assigned nothing for its whole --wait
// (2026-10-25); anyone else's seal is not this command's to overrule.
func TestARunReopensOnlyTheSealAnEarlierRunLeft(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reason  string
		wantErr string
		sealed  bool
		said    string
	}{
		{name: "an earlier run's", reason: acceptanceRunSealReason, said: "an earlier run sealed it"},
		{name: "an operator's", reason: "a maintenance window", sealed: true, wantErr: "billet resume --config"},
		{name: "a teardown's", reason: "billet acceptance down", sealed: true, wantErr: "billet resume --config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
			stateDir := filepath.Join(filepath.Dir(ws.ConfigPath), "server")
			ledgertest.Seed(t, stateDir)

			var out bytes.Buffer
			env := cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}
			sealWith(t, env, ws, tc.reason)
			out.Reset()

			err := reopenAfterAnEarlierRun(t.Context(), env, ws)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("reopen = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), tc.reason)) {
				t.Fatalf("reopen = %v, want a refusal naming the seal and %q", err, tc.wantErr)
			}
			if !strings.Contains(out.String(), tc.said) {
				t.Errorf("said %q, want %q", out.String(), tc.said)
			}
			if got := admissionOf(t, ws).Sealed(); got != tc.sealed {
				t.Errorf("sealed = %v, want %v", got, tc.sealed)
			}
		})
	}
}

// A WORKSPACE THAT HAS NEVER RUN HAS NO SEAL, and looking for one must not make a
// ledger the server would then find already there.
func TestAWorkspaceThatHasNeverRunIsLeftWithoutALedger(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))

	var out bytes.Buffer
	if err := reopenAfterAnEarlierRun(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, ws); err != nil {
		t.Fatalf("reopen = %v", err)
	}
	ledger := state.LedgerPath(filepath.Join(filepath.Dir(ws.ConfigPath), "server"))
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Errorf("looking for a seal made the ledger (%v)", err)
	}
}

// AN OPEN DEPLOYMENT IS LEFT ALONE: every write bumps admission's generation, so
// an unchanged one proves nothing was written.
func TestAnOpenDeploymentIsNotReopened(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	ledgertest.Seed(t, filepath.Join(filepath.Dir(ws.ConfigPath), "server"))
	before := admissionOf(t, ws)

	var out bytes.Buffer
	if err := reopenAfterAnEarlierRun(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, ws); err != nil {
		t.Fatalf("reopen = %v", err)
	}
	if after := admissionOf(t, ws); after.Generation != before.Generation {
		t.Errorf("generation moved from %d to %d on an open deployment", before.Generation, after.Generation)
	}
	if out.Len() != 0 {
		t.Errorf("said %q about a deployment nothing happened to", out.String())
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
