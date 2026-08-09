package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func claimConfig(t *testing.T, stateDir, lockDir string) *config.Config {
	t.Helper()

	return &config.Config{Server: &config.ServerConfig{
		StateDir: stateDir,
		LockDir:  lockDir,
	}}
}

// The dev path claims the identity and takes the lock.
func TestClaimDeploymentTakesTheLock(t *testing.T) {
	lockDir := t.TempDir()

	id, lock, err := claimDeployment(claimConfig(t, t.TempDir(), lockDir))
	if err != nil {
		t.Fatalf("claimDeployment: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	if id == "" {
		t.Fatal("no identity was claimed")
	}

	if lock.Degraded() != "" {
		t.Fatalf("the lock degraded without being asked to: %s", lock.Degraded())
	}

	if filepath.Dir(lock.Path()) != lockDir {
		t.Errorf("server.lock_dir was not honoured: locked in %q", lock.Path())
	}
}

// A COPIED STATE DIRECTORY IS REFUSED **BEFORE ITS DATABASE IS TOUCHED**.
//
// The ordering is the finding, not a detail. The claim used to happen after
// state.Open, which creates files, runs integrity checks and applies migrations
// — so an operator who accidentally started an old copied backup alongside the
// live original had that backup silently migrated on its way to being refused.
// A process must establish its right to the identity before it writes anything
// durable under it.
func TestAClaimIsRefusedBeforeAnyDatabaseIsCreated(t *testing.T) {
	lockDir := t.TempDir()
	original := t.TempDir()

	id, lock, err := claimDeployment(claimConfig(t, original, lockDir))
	if err != nil {
		t.Fatalf("claimDeployment: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	// The copy an operator would restore from a backup: same identity, own path.
	copied := t.TempDir()

	raw, err := os.ReadFile(filepath.Join(original, "deployment-id"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := os.WriteFile(filepath.Join(copied, "deployment-id"), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, _, err := claimDeployment(claimConfig(t, copied, lockDir)); !errors.Is(err, state.ErrDeploymentLocked) {
		t.Fatalf("the copy was allowed to start alongside the original: %v", err)
	}

	// NOTHING WAS WRITTEN. The copy carried only the identity file it was given;
	// a database, a WAL, or a lock file here would mean the refusal came too late
	// to be worth anything.
	entries, err := os.ReadDir(copied)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	for _, e := range entries {
		if e.Name() != "deployment-id" {
			t.Errorf("the refused copy had %q created in its state directory, so it wrote "+
				"before it was refused", e.Name())
		}
	}

	_ = id
}

// A host with nowhere to put the lock refuses to start unless the operator has
// opted in — checked HERE too, because this is the layer that turns config into
// the option, and a knob wired to nothing is worse than no knob.
func TestTheOptOutIsActuallyWired(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "notadir")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := claimConfig(t, t.TempDir(), blocker)

	if _, _, err := claimDeployment(cfg); err == nil {
		t.Fatal("billet started without a lock and without being asked to")
	} else if !strings.Contains(err.Error(), "allow_unlocked_deployment") {
		t.Errorf("the refusal does not name the opt-out: %v", err)
	}

	cfg.Server.AllowUnlockedDeployment = true

	_, lock, err := claimDeployment(cfg)
	if err != nil {
		t.Fatalf("allow_unlocked_deployment did not take effect: %v", err)
	}

	if lock.Degraded() == "" {
		t.Error("the lock claims to be held, but nothing could be locked")
	}
}
