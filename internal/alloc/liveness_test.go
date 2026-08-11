package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// live reads whether the ledger currently believes a node is reachable.
func live(t *testing.T, a *Allocator, name string) bool {
	t.Helper()

	var n int

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT live FROM nodes WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("read liveness of %s: %v", name, err)
	}

	return n == 1
}

// A REGISTERED NODE IS LIVE, which is the fact placement will need and the one
// nothing recorded. The plane's map knew it; the ledger did not, and the ledger
// is where capacity is counted.
func TestRegistrationMarksANodeLive(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	if _, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker)); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a node that just registered is not live")
	}
}

// A node the plane has given up on stops backing advertisements.
func TestANodeThatIsGoneStopsBeingLive(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	epoch, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if err := a.NodeGone(t.Context(), "n1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if live(t, a, "n1") {
		t.Error("a node the plane forgot is still live in the ledger")
	}
}

// COMING BACK IS THE ORDINARY ENDING, and it is the path a fresh registration
// does not exercise.
//
// A host that goes quiet is forgotten and then, almost always, returns — the
// process restarted, the link healed, the machine rebooted. That return goes
// through the UPDATE half of the upsert, not the INSERT half, so a version that
// set `live` only on first sight would leave every recovered node dead in the
// ledger: registered, reachable, taking commands, and contributing nothing to
// what its tier may advertise. Nothing else in the system would report it.
func TestANodeThatComesBackIsLiveAgain(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	epoch, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if err := a.NodeGone(t.Context(), "n1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if live(t, a, "n1") {
		t.Fatal("the node did not go away, so this test proves nothing about it coming back")
	}

	if _, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker)); err != nil {
		t.Fatalf("re-registering: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a node that registered again is still recorded as gone; it takes commands " +
			"while contributing nothing to what its tier may advertise")
	}
}

// THE RACE THIS FENCE EXISTS FOR, and it is not hypothetical — the ordering that
// produces it is in the code.
//
// Registration commits to the ledger BEFORE it takes the plane's mutex, and
// expiry holds that mutex while it drops the old entry. So a node that restarts
// quickly can commit its new registration and then be marked dead by the expiry
// of the incarnation it replaced. The ledger would say the fleet has no node
// while the plane happily launches onto one, and every tier would advertise
// zero against a machine that is right there.
//
// The epoch is the fence: re-registration bumps it, so a write carrying the old
// one matches nothing.
func TestAnExpiringOldIncarnationCannotKillTheNewOne(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	stale, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("first RegisterNode: %v", err)
	}

	// The node restarts. Same name, new incarnation, new epoch.
	fresh, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("second RegisterNode: %v", err)
	}

	if fresh == stale {
		t.Fatalf("re-registration did not move the epoch: still %d", fresh)
	}

	// The old incarnation's expiry lands late, carrying the epoch it knew.
	if err := a.NodeGone(t.Context(), "n1", stale); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a stale expiry killed the incarnation that replaced it; the ledger now says " +
			"there is no node while the plane is launching onto one")
	}
}

// NOTHING IS LIVE UNTIL IT SAYS SO AGAIN.
//
// Liveness is the plane's judgement, and a restarted control plane has no
// judgement yet — its map is empty. Rows left over from the previous process
// would otherwise back advertisements for machines this one has never heard
// from, which is the same over-advertisement in a different disguise. Nodes
// re-register within a poll, so the cost is a brief and correct zero.
func TestARestartedControlPlaneTrustsNoNodeUntilItRegistersAgain(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	for _, name := range []string{"n1", "n2"} {
		if _, err := a.RegisterNode(t.Context(), testRegistration(name, config.ProviderDocker)); err != nil {
			t.Fatalf("RegisterNode %s: %v", name, err)
		}
	}

	if err := a.ForgetEveryNode(t.Context()); err != nil {
		t.Fatalf("ForgetEveryNode: %v", err)
	}

	for _, name := range []string{"n1", "n2"} {
		if live(t, a, name) {
			t.Errorf("%s survived a control-plane restart as live", name)
		}
	}
}
