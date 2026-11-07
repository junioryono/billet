package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// STOPPED IS PROVED BY THE SERVICES' OWN LOCKS: a run's control plane or node that still
// holds its lock is live, whatever the run that started it said, and only when both
// locks can be taken has the run stopped. A node draining past `acceptance run`'s stop
// grace outlives the lock the run was handed, so that lock is not the proof.
func TestStoppedIsProvedByTheServicesOwnLocks(t *testing.T) {
	t.Parallel()

	ws := deriveForTest(t, writeAcceptanceBase(t, acceptanceBaseConfig), filepath.Join(t.TempDir(), "acc"))
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}
	if ws.DeploymentID == "" || cfg.Server == nil || cfg.Node == nil || cfg.Node.LockDir == "" {
		t.Fatalf("the derived workspace lacks what this test holds: %+v", ws)
	}

	if err := acceptanceStopped(cfg, ws.DeploymentID); err != nil {
		t.Fatalf("a run with neither service alive: %v", err)
	}

	plane, err := state.LockStateDir(cfg.Server.IdentityDir)
	if err != nil {
		t.Fatalf("hold the control plane's lock: %v", err)
	}
	if err := acceptanceStopped(cfg, ws.DeploymentID); !errors.Is(err, errAcceptanceLive) {
		t.Errorf("a control plane holding its lock answered %v, want it live", err)
	}
	if err := plane.Release(); err != nil {
		t.Fatal(err)
	}

	node, err := state.LockDeployment(ws.DeploymentID, state.LockOptions{Dir: cfg.Node.LockDir})
	if err != nil {
		t.Fatalf("hold the node's lock: %v", err)
	}
	if err := acceptanceStopped(cfg, ws.DeploymentID); !errors.Is(err, errAcceptanceLive) {
		t.Errorf("a node holding its lock answered %v, want it live", err)
	}
	if err := node.Release(); err != nil {
		t.Fatal(err)
	}

	if err := acceptanceStopped(cfg, ""); err == nil || errors.Is(err, errAcceptanceLive) {
		t.Errorf("a workspace with no deployment id answered %v, want could not tell", err)
	}
	if err := acceptanceStopped(cfg, ws.DeploymentID); err != nil {
		t.Errorf("once both locks are released: %v", err)
	}
}
