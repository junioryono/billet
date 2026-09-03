package main

import (
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
		allowDowngrade: true,
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

// A CONVERGED CONTROLLER IS LEFT ALONE, however far the nodes are from the
// target: the host half of the rollout is done, and the coordinator does the rest.
func TestFromRolloutStopsOnceTheControllerConverged(t *testing.T) {
	cfg, r := ledgerWithRollout(t, "v9.9.9")

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStateAdmin: %v", err)
	}

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

	_ = db.Close()

	_, ok, err := rolloutInstruction(t.Context(), cfg)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	if ok {
		t.Fatal("a converged controller was told to upgrade again")
	}
}
