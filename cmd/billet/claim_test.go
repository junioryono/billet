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

// claimConfig is a node, because the node is the role that takes the lock. The
// server takes none: it manages no containers, and holding the identity would
// keep a node on the same machine from ever starting.
func claimConfig(t *testing.T, stateDir, lockDir string) *config.Config {
	t.Helper()

	return &config.Config{Node: &config.NodeConfig{
		Name:     "host-1",
		StateDir: stateDir,
		LockDir:  lockDir,
	}}
}

func claimDeployment(cfg *config.Config) (string, *state.DeploymentLock, error) {
	return claimNodeDeployment(cfg, nil)
}

// A node claims the identity and takes the lock.
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
		t.Errorf("node.lock_dir was not honoured: locked in %q", lock.Path())
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

	cfg.Node.AllowUnlockedDeployment = true

	_, lock, err := claimDeployment(cfg)
	if err != nil {
		t.Fatalf("allow_unlocked_deployment did not take effect: %v", err)
	}

	if lock.Degraded() == "" {
		t.Error("the lock claims to be held, but nothing could be locked")
	}
}

// THE NODE LOCKS WHERE IT WAS TOLD TO, which is what keeps it colliding with a
// server on the same host.
//
// A server honouring server.lock_dir while the node used the per-user default
// would take two different locks for one deployment identity — and then both
// processes manage the same containers, each able to adopt or destroy the
// other's live work. The config refuses a mismatched pair; this proves the node
// actually uses the value rather than merely accepting it.
func TestTheNodeLocksWhereItWasTold(t *testing.T) {
	lockDir := t.TempDir()

	cfg := &config.Config{Node: &config.NodeConfig{
		Name:     "host-1",
		StateDir: t.TempDir(),
		LockDir:  lockDir,
	}}

	id, lock, err := claimNodeDeployment(cfg, nil)
	if err != nil {
		t.Fatalf("claimNodeDeployment: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	if id == "" {
		t.Fatal("the node claimed no identity")
	}

	if got := filepath.Dir(lock.Path()); got != lockDir {
		t.Fatalf("the node locked in %q, but node.lock_dir said %q — on a host also running a "+
			"server, those are two locks for one identity", got, lockDir)
	}
}

// TWO NODES ON ONE HOST COLLIDE, which is the whole point of the lock.
//
// Same identity, same configured directory: the second must be refused. If they
// landed in different files both would run, and both would manage the same
// containers, each able to adopt or destroy the other's live work.
//
// A SERVER ALONGSIDE THEM DOES NOT COLLIDE, and the earlier version of this test
// asserted the opposite — that a server and a node on one host must refuse each
// other. That was right while `--dev` existed, because a --dev server WAS a
// node: it ran a provider in its own process, so a second node beside it really
// was two managers of one machine's containers. With --dev gone the server runs
// no compute and takes no lock, and server-plus-node on one box is not a
// conflict to be caught — it is the single-machine deployment.
func TestTwoNodesOnOneHostCollide(t *testing.T) {
	lockDir := t.TempDir()
	stateDir := t.TempDir()

	first := &config.Config{Node: &config.NodeConfig{
		Name: "host-1", StateDir: stateDir, LockDir: lockDir,
	}}

	_, lock, err := claimNodeDeployment(first, nil)
	if err != nil {
		t.Fatalf("the first node could not claim: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	second := &config.Config{Node: &config.NodeConfig{
		Name: "host-2", StateDir: stateDir, LockDir: lockDir,
	}}

	if _, _, err := claimNodeDeployment(second, nil); !errors.Is(err, state.ErrDeploymentLocked) {
		t.Fatalf("a second node started alongside one holding the same identity: %v", err)
	}
}

// A NODE WITH NO CERTIFICATE JOINS THE CONTROL PLANE DESCRIBED BESIDE IT.
//
// This is the single-machine deployment, and it was broken in a way nothing
// could see. A control plane on loopback serves plain HTTP — a certificate there
// is a config ERROR, because there is nothing between two processes on one box
// to authenticate — so the node has no bundle, and a bundle is what normally
// says which deployment it is joining.
//
// Falling back to its own state directory looks reasonable and is wrong: that
// directory MINTS a fresh random identity when it has none. The server minted
// one too, in its own directory, and the two never match. The plane then refuses
// the node for belonging to another deployment — with ErrRefused, which the node
// loop treats as a verdict rather than an outage, so the process EXITS. Nothing
// retries and nothing repairs it. The shipped example config has exactly this
// shape: server.state_dir and node.state_dir are different paths.
//
// The rule is the same one the certificate path follows — identity comes from
// whatever proves which deployment you are joining. Without a certificate, that
// proof is this file describing the control plane itself.
func TestACertlessNodeJoinsTheServerInItsOwnConfig(t *testing.T) {
	serverState := t.TempDir()

	// The server founds the deployment, exactly as runServer does.
	want, err := state.DeploymentID(serverState)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	cfg := &config.Config{
		Server: &config.ServerConfig{IdentityDir: serverState, Listen: "127.0.0.1:7717"},
		Node: &config.NodeConfig{
			Name: "host-1", ServerAddr: "127.0.0.1:7717",
			// A DIFFERENT DIRECTORY, which is what billet.example.yaml ships.
			StateDir: t.TempDir(),
			LockDir:  t.TempDir(),
		},
	}

	got, lock, err := claimNodeDeployment(cfg, nil)
	if err != nil {
		t.Fatalf("claimNodeDeployment: %v", err)
	}

	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})

	if got != want {
		t.Errorf("the node claimed deployment %s but the control plane in the same file is %s; "+
			"the plane refuses that with ErrRefused and the node process exits, so a "+
			"single-machine deployment never starts", got, want)
	}
}
