package server

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// ESCROW WHOSE MACHINE HAS GONE AWAY IS HANDED BACK, and the advertisement does
// not follow it down.
//
// The leases to drop are exactly identifiable because every reservation names
// its machine: the ones this listener is HOLDING (never assigned, so nothing is
// running under them) whose host is no longer live.
//
// The advertisement is the tier's configured ceiling (#140), so it does not move
// when a host goes: a job assigned while no machine can take it waits for one, as
// a job assigned to a full fleet does. Advertising zero would make the tier
// unassignable, which is the failure #140 exists for.
func TestEscrowIsReleasedWhenItsMachineGoesAway(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, tiers)

	// One machine, room for two of this tier.
	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "only", Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 64 * config.GiB,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	l := NewListener(a, tiers[0].Label, &fakeSession{})

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("hold escrow on the machine: %v", err)
	}

	if held := len(l.Held()); held != 2 {
		t.Fatalf("holds %d leases on a machine with room for 2; this test proves nothing", held)
	}

	if err := a.NodeGone(t.Context(), "only", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if released := l.releaseStrandedEscrow(t.Context()); released != 2 {
		t.Errorf("released %d stranded leases, want both", released)
	}

	if held := len(l.Held()); held != 0 {
		t.Errorf("still holds %d leases on a machine that has gone away", held)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.VCPU != 0 {
		t.Errorf("the ledger still charges %d vCPU to a machine that has gone away", usage.VCPU)
	}

	if got := l.steadyAdvertisement(); got != 16 {
		t.Errorf("advertises %d after the machine went away, want the configured ceiling 16", got)
	}
}
