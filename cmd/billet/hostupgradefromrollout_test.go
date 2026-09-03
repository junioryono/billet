package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/version"
)

// ledgerWithRollout opens a throwaway control-plane ledger and records one
// rollout on it, returning the config that names it.
func ledgerWithRollout(t *testing.T, target string) (*config.Config, *rollout.Rollout) {
	t.Helper()

	// THE DECISION MARK LIVES UNDER THE UPGRADE ROOT, and the timer's no-op raises
	// it, so each test gets a root of its own.
	original := upgradeRoot
	upgradeRoot = t.TempDir()
	t.Cleanup(func() { upgradeRoot = original })

	cfg := &config.Config{Server: &config.ServerConfig{IdentityDir: t.TempDir()}}

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStateAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	if target == "" {
		return cfg, nil
	}

	r, err := rollout.New(db).Start(t.Context(), rollout.StartRequest{
		Channel: "stable", TargetVersion: target,
		TargetDigest: "1111111111111111111111111111111111111111111111111111111111111111",
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: []string{"epyc-1"},
		Policy: rollout.Policy{Cohort: 1, FailureBudget: 1, AllowDowngrade: true},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	return cfg, r
}

// THE TIMER'S INSTRUCTION IS THE LEDGER'S, WHOLE. Every field the rollout
// persisted reaches the transaction — the pin, the digest, the id, the
// generation and the downgrade permission — so the host installs the bytes the
// decision named under the fences the decision carries, exactly as a node's
// dispatch would.
func TestFromRolloutTakesTheWholeInstructionFromTheLedger(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	got, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	if !ok {
		t.Fatal("an open rollout to another release was read as nothing to do")
	}

	want := hostUpgradeTarget{
		pin: "v9.9.9", digest: r.TargetDigest, rolloutID: r.ID, generation: r.Generation,
		allowDowngrade: true, fromRollout: true,
	}

	if got != want {
		t.Errorf("the instruction is %+v, want %+v", got, want)
	}
}

// FOUR WAYS TO HAVE NOTHING TO DO, AND NONE OF THEM IS AN ERROR. This runs every
// five minutes unattended; a refusal reported as a failure would be a red unit
// on every healthy host.
func TestFromRolloutHasNothingToDoWithoutADecisionToAct(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(t *testing.T) *config.Config
	}{
		{"no control plane", func(t *testing.T) *config.Config {
			t.Helper()

			return &config.Config{Node: &config.NodeConfig{}}
		}},
		{"automatic updates off", func(t *testing.T) *config.Config {
			t.Helper()

			cfg, _ := ledgerWithRollout(t, "v9.9.9")
			cfg.Release = &config.ReleaseConfig{Automatic: new(false)}

			return cfg
		}},
		{"no rollout", func(t *testing.T) *config.Config {
			t.Helper()

			cfg, _ := ledgerWithRollout(t, "")

			return cfg
		}},
		{"already on the target", func(t *testing.T) *config.Config {
			t.Helper()

			cfg, _ := ledgerWithRollout(t, version.Version())

			return cfg
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok, err := rolloutInstruction(t.Context(), c.cfg(t))
			if err != nil {
				t.Fatalf("rolloutInstruction: %v", err)
			}

			if ok {
				t.Fatal("something was read as an instruction to act on")
			}
		})
	}
}

// A CONVERGED CONTROLLER PHASE IS NOT THIS HOST'S PHASE. The rollout records one
// control plane's progress; a PostgreSQL standby is a second controller host
// that phase says nothing about, and its timer is the only thing that moves it.
// So a host not on the target acts however far the recorded controller got.
func TestFromRolloutMovesAHostBehindAConvergedController(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	advanceController(t, cfg, r)

	got, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	if !ok || got.rolloutID != r.ID || got.digest != r.TargetDigest {
		t.Fatalf("a host behind a converged controller was told nothing (ok=%v, %+v)", ok, got)
	}
}

// THE LAST COMPLETED ROLLOUT IS THE DECISION ONCE NOTHING IS OPEN. A standby is
// not a node, so the rollout completes without it; its timer then finds no open
// rollout and must still take the fleet to where the fleet went. An aborted
// rollout is an operator's decision and moves nobody.
func TestFromRolloutMovesAStandbyToTheLastCompletedRollout(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	finish(t, cfg, r.ID, rollout.StateCompleted)

	got, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	want := hostUpgradeTarget{
		pin: "v9.9.9", digest: r.TargetDigest, rolloutID: r.ID, generation: r.Generation,
		allowDowngrade: true, fromRollout: true,
	}

	if !ok || got != want {
		t.Fatalf("a completed rollout gave (ok=%v) %+v, want %+v", ok, got, want)
	}

	aborted, a := ledgerWithRollout(t, "v9.9.9")

	finish(t, aborted, a.ID, rollout.StateAborted)

	if _, ok, err := rolloutInstruction(t.Context(), aborted); err != nil || ok {
		t.Fatalf("an aborted rollout was acted on (ok=%v, err=%v)", ok, err)
	}
}

// THE INSTRUCTION IS READ AGAIN BEFORE IT IS ACCEPTED. Between the read and the
// claim an operator can abort the rollout or the coordinator replace it; the
// same tuple, open or completed, is confirmed and anything else is superseded.
func TestFromRolloutRefusesADecisionThatChangedSinceItWasRead(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	target, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil || !ok {
		t.Fatalf("rolloutInstruction: ok=%v err=%v", ok, err)
	}

	if err := confirmFleetDecision(t.Context(), cfg, target); err != nil {
		t.Fatalf("an unchanged decision was not confirmed: %v", err)
	}

	finish(t, cfg, r.ID, rollout.StateCompleted)

	if err := confirmFleetDecision(t.Context(), cfg, target); err != nil {
		t.Fatalf("a decision that completed meanwhile was not confirmed: %v", err)
	}

	aborted, a := ledgerWithRollout(t, "v9.9.9")

	target, _, err = rolloutInstruction(t.Context(), aborted)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	finish(t, aborted, a.ID, rollout.StateAborted)

	if err := confirmFleetDecision(t.Context(), aborted, target); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("a decision aborted since it was read was confirmed: %v", err)
	}

	other := target
	other.generation++

	if err := confirmFleetDecision(t.Context(), cfg, other); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("a decision with another generation was confirmed: %v", err)
	}
}

// advanceController walks the recorded control plane to committed.
func advanceController(t *testing.T, cfg *config.Config, r *rollout.Rollout) {
	t.Helper()

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStateAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	store := rollout.New(db)

	for _, phase := range []rollout.Phase{
		rollout.PhaseDraining, rollout.PhaseReadyToInstall, rollout.PhaseInstalling,
		rollout.PhaseVerifying, rollout.PhaseCommitted,
	} {
		if err := store.Advance(t.Context(), rollout.AdvanceRequest{
			RolloutID: r.ID, To: phase,
		}); err != nil {
			t.Fatalf("Advance the controller to %s: %v", phase, err)
		}
	}
}

// finish ends the rollout in the given state. A completed rollout is one whose
// controller and every node converged, so those are walked first.
func finish(t *testing.T, cfg *config.Config, id, outcome string) {
	t.Helper()

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStateAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	store := rollout.New(db)

	if outcome == rollout.StateCompleted {
		for _, node := range []string{"", "epyc-1"} {
			for _, phase := range []rollout.Phase{
				rollout.PhaseDraining, rollout.PhaseReadyToInstall, rollout.PhaseInstalling,
				rollout.PhaseVerifying, rollout.PhaseCommitted,
			} {
				if err := store.Advance(t.Context(), rollout.AdvanceRequest{
					RolloutID: id, Node: node, To: phase,
				}); err != nil {
					t.Fatalf("Advance %q to %s: %v", node, phase, err)
				}
			}
		}
	}

	if err := store.Finish(t.Context(), id, outcome, "the test said so"); err != nil {
		t.Fatalf("Finish %s: %v", outcome, err)
	}
}

// A HOST ON THE TARGET VERSION FROM ANOTHER MANIFEST IS REINSTALLED. The digest
// is the decision's identity, and a rollout blocks such a host and names this
// command as the repair; a timer that read the version alone would leave it
// blocked forever. Only a positive disagreement counts: no record is on target.
func TestFromRolloutReinstallsATargetVersionFromAnotherManifest(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	sum, err := provenance.HashFile(exe)
	if err != nil {
		t.Fatal(err)
	}

	prev := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")
	t.Cleanup(func() { provenance.Path = prev })

	cfg, r := ledgerWithRollout(t, version.Version())

	if _, ok, err := rolloutInstruction(t.Context(), cfg); err != nil || ok {
		t.Fatalf("a host with no record was reinstalled (ok=%v, err=%v)", ok, err)
	}

	if err := provenance.Write(provenance.Record{
		Version: version.Version(), ManifestDigest: strings.Repeat("2", 64), BinarySHA256: sum,
	}); err != nil {
		t.Fatalf("Write the provenance record: %v", err)
	}

	got, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil || !ok {
		t.Fatalf("a host from another manifest was left alone (ok=%v, err=%v)", ok, err)
	}

	if got.digest != r.TargetDigest || got.rolloutID != r.ID {
		t.Errorf("the reinstall names %+v, want rollout %s at %s", got, r.ID, r.TargetDigest)
	}
}

// DOING NOTHING RAISES BOTH MARKS. A host found on decision 10's target that
// recorded nothing would let a delayed decision 9 through the fence later and be
// downgraded by it; and it settles on the decision, so a completed rollout is
// not followed again after an operator moves this host by hand.
func TestFromRolloutRecordsTheDecisionItHadNothingToDoAbout(t *testing.T) {
	cfg, r := ledgerWithRollout(t, version.Version())

	if _, ok, err := rolloutInstruction(t.Context(), cfg); err != nil || ok {
		t.Fatalf("a host on the target was told to act (ok=%v, err=%v)", ok, err)
	}

	for name, read := range map[string]func() (int64, error){
		"last-decision": readDecision, "settled-decision": readSettled,
	} {
		got, err := read()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		if got != r.Generation {
			t.Errorf("the no-op left %s at %d, want %d", name, got, r.Generation)
		}
	}
}

// THE NO-OP WAITS ON THE HOST TRANSACTION LOCK. The binary it runs may be a
// candidate another updater installed and has yet to prove; settling on it from
// outside that transaction would bless what the transaction may still roll back.
func TestFromRolloutSettlesNothingWhileAnUpgradeHoldsTheHost(t *testing.T) {
	cfg, _ := ledgerWithRollout(t, version.Version())

	tx, err := takeTxLock()
	if err != nil {
		t.Fatalf("takeTxLock: %v", err)
	}

	defer tx.release()

	if _, ok, err := rolloutInstruction(t.Context(), cfg); err != nil || ok {
		t.Fatalf("under another transaction's lock: ok=%v err=%v, want nothing to do and no error",
			ok, err)
	}

	if settled, err := readSettled(); err != nil || settled != 0 {
		t.Fatalf("the no-op settled on decision %d under another transaction's lock (err %v)",
			settled, err)
	}
}

// A COMPLETED ROLLOUT IS TAKEN BY A HOST THAT NEVER SETTLED ON IT, AND ONLY BY
// THAT HOST. One that attempted it and rolled back has raised last-decision and
// not settled-decision, and is asked again; one that carried it out, or was
// found on its target and then moved by hand, settled on it and is left alone.
func TestFromRolloutFollowsACompletedRolloutOnlyUntilThisHostSettlesOnIt(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	finish(t, cfg, r.ID, rollout.StateCompleted)

	// Attempted and rolled back: the fence is raised, nothing settled.
	if err := checkAndRecordDecision(hostUpgradeTarget{generation: r.Generation}); err != nil {
		t.Fatalf("record the decision: %v", err)
	}

	if _, ok, err := rolloutInstruction(t.Context(), cfg); err != nil || !ok {
		t.Fatalf("a host that rolled back on the completed rollout was not asked again (ok=%v, "+
			"err=%v)", ok, err)
	}

	// Carried out: settled.
	if err := recordSettled(r.Generation); err != nil {
		t.Fatalf("recordSettled: %v", err)
	}

	if _, ok, err := rolloutInstruction(t.Context(), cfg); err != nil || ok {
		t.Fatalf("a host that settled on the completed rollout was moved to it again (ok=%v, "+
			"err=%v)", ok, err)
	}
}

// A COMMITTED TRANSACTION SETTLES ON ITS DECISION where it records what it
// installed, and an operator's own run, which serves no decision, settles on
// nothing.
func TestACommittedTransactionSettlesOnItsDecision(t *testing.T) {
	original := upgradeRoot
	upgradeRoot = t.TempDir()
	t.Cleanup(func() { upgradeRoot = original })

	prevPath, prevBinary := provenance.Path, installedBinary
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")
	installedBinary = filepath.Join(t.TempDir(), "billet")
	t.Cleanup(func() { provenance.Path, installedBinary = prevPath, prevBinary })

	if err := os.WriteFile(installedBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := newLedgerHost(&config.Config{}, "", &hostupgrade.Journal{Generation: 7})
	if err := h.RecordInstalled(t.Context(), strings.Repeat("3", 64), "v0.6.0"); err != nil {
		t.Fatalf("RecordInstalled: %v", err)
	}

	if settled, err := readSettled(); err != nil || settled != 7 {
		t.Fatalf("a committed transaction for decision 7 settled on %d (err %v)", settled, err)
	}

	own := newLedgerHost(&config.Config{}, "", &hostupgrade.Journal{})
	if err := own.RecordInstalled(t.Context(), strings.Repeat("4", 64), "v0.6.1"); err != nil {
		t.Fatalf("RecordInstalled: %v", err)
	}

	if settled, err := readSettled(); err != nil || settled != 7 {
		t.Fatalf("an operator's own run moved the settled mark to %d (err %v)", settled, err)
	}
}

// THE INSTRUCTION IS READ THROUGH A HANDLE THAT NAMES NO RELEASE, because the
// host reading it may be the standby the newer leader's watermark would refuse.
func TestTheInstructionIsReadWithoutTheWatermark(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "hostupgrade.go", nil, 0)
	if err != nil {
		t.Fatalf("parse hostupgrade.go: %v", err)
	}

	var reads, others int

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "readFleetDecision" {
			continue
		}

		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			switch ident, ok := call.Fun.(*ast.Ident); {
			case ok && ident.Name == "openStateForDecision":
				reads++
			case ok && strings.HasPrefix(ident.Name, "openState"):
				others++
			}

			return true
		})
	}

	if reads != 1 || others != 0 {
		t.Fatalf("readFleetDecision opens through openStateForDecision %d time(s) and through "+
			"another opener %d time(s); want exactly one and none", reads, others)
	}
}
