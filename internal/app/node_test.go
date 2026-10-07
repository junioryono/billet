package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// openNodeConfig is a node whose provider OpenNode cannot build: the simulated
// backend, which NewProvider refuses by name. Reaching that refusal is how a
// test sees the provider was asked for.
func openNodeConfig(t *testing.T, stateDir, lockDir string) *config.Config {
	t.Helper()

	return &config.Config{Node: &config.NodeConfig{
		Name:       "host-1",
		StateDir:   stateDir,
		LockDir:    lockDir,
		ServerAddr: "http://127.0.0.1:1",
		Provider:   config.ProviderSimulated,
	}}
}

// THE PROVIDER IS BUILT ONLY UNDER THE DEPLOYMENT LOCK. A host whose identity
// another process holds is refused at the claim, before anything could touch a
// container; and an open that fails after the claim gives the lock back, so a
// failed start does not keep the next one out.
func TestANodeBuildsItsProviderOnlyUnderTheLock(t *testing.T) {
	t.Parallel()

	stateDir, lockDir := t.TempDir(), t.TempDir()

	_, held, err := ClaimNodeDeployment(openNodeConfig(t, stateDir, lockDir), nil)
	if err != nil {
		t.Fatalf("claim the identity first: %v", err)
	}

	if _, err := OpenNode(openNodeConfig(t, stateDir, lockDir), NodeOptions{}); !errors.Is(err, state.ErrDeploymentLocked) {
		t.Fatalf("OpenNode on a held identity = %v, want the lock's refusal before any provider", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	_, err = OpenNode(openNodeConfig(t, stateDir, lockDir), NodeOptions{})
	if err == nil || !strings.Contains(err.Error(), "test harness") {
		t.Fatalf("OpenNode with the lock free = %v, want the provider's refusal", err)
	}

	_, again, err := ClaimNodeDeployment(openNodeConfig(t, stateDir, lockDir), nil)
	if err != nil {
		t.Fatalf("an OpenNode that failed after its claim kept the lock: %v", err)
	}

	if err := again.Release(); err != nil {
		t.Errorf("release: %v", err)
	}
}

// AND AN OPEN THAT SUCCEEDS HOLDS THE LOCK UNTIL IT IS CLOSED. A node that let
// it go once its provider was built would let a second process manage the same
// compute beside it. The docker backend's constructor reaches no daemon, so the
// open succeeds wherever the test runs.
func TestAnOpenNodeHoldsTheLockUntilClosed(t *testing.T) {
	t.Parallel()

	stateDir, lockDir := t.TempDir(), t.TempDir()

	cfg := openNodeConfig(t, stateDir, lockDir)
	cfg.Node.Provider = config.ProviderDocker

	n, err := OpenNode(cfg, NodeOptions{})
	if err != nil {
		t.Fatalf("OpenNode over docker: %v", err)
	}

	if _, _, err := ClaimNodeDeployment(openNodeConfig(t, stateDir, lockDir), nil); !errors.Is(err, state.ErrDeploymentLocked) {
		t.Fatalf("a second claim beside an open node = %v, want the lock's refusal", err)
	}

	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, again, err := ClaimNodeDeployment(openNodeConfig(t, stateDir, lockDir), nil)
	if err != nil {
		t.Fatalf("a closed node kept the lock: %v", err)
	}

	if err := again.Release(); err != nil {
		t.Errorf("release: %v", err)
	}
}
