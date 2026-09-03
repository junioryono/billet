package main

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/version"
)

// ledgerWithRollout opens a throwaway control-plane ledger and records one
// rollout on it, returning the config that names it.
func ledgerWithRollout(t *testing.T, target string) (*config.Config, *rollout.Rollout) {
	t.Helper()

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
