package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A PURCHASE OUT OF TURN TAKES NOTHING A WAITER AHEAD NEEDS (#346). The fair
// order holds freed room for the longest waiter; a tier buying ahead of it may
// buy only while the waiter could still be granted one lease, and nothing at all
// while the waiter cannot be granted one even now, because then it is
// accumulating room.
func TestEscrowLeavingKeepsRoomForTheWaitersAhead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		ceiling   int
		held      int // small leases bought first, in turn
		want      int
		wantTaken int
	}{
		// 16 - 8 = 8 free: the big waiter fits once; one small purchase would
		// leave it 4, so none is made.
		{name: "a purchase that would starve the waiter is refused", ceiling: 16, held: 2, want: 4, wantTaken: 0},
		// 20 - 8 = 12 free: one small purchase leaves the big waiter 8, a
		// second would leave it 4, so exactly one is made.
		{name: "only what leaves the waiter room is bought", ceiling: 20, held: 2, want: 4, wantTaken: 1},
		// 16 - 12 = 4 free: the big waiter does not fit now, so the small tier
		// may take nothing even though one more of it fits.
		{name: "a waiter that cannot fit now is accumulating room", ceiling: 16, held: 3, want: 4, wantTaken: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newAllocator(t, Limits{MaxVCPU: tc.ceiling, MaxMemory: 256 * config.GiB},
				[]config.Tier{tier("small", 4, 4*config.GiB), tier("big", 8, 8*config.GiB)})

			if got, err := a.Escrow(t.Context(), "small", tc.held); err != nil || len(got) != tc.held {
				t.Fatalf("in-turn escrow bought %d (%v), want %d", len(got), err, tc.held)
			}

			got, err := a.EscrowLeaving(t.Context(), "small", tc.want, []string{"big"})
			if err != nil {
				t.Fatalf("EscrowLeaving: %v", err)
			}
			if len(got) != tc.wantTaken {
				t.Errorf("bought %d ahead of the waiter, want %d", len(got), tc.wantTaken)
			}

			// AND THE WAITER CAN STILL BUY WHAT IT WAS LEFT, which is the property
			// rather than the arithmetic.
			if tc.ceiling-4*(tc.held+len(got)) >= 8 {
				if room, err := a.Headroom(t.Context(), "big"); err != nil || room < 1 {
					t.Errorf("the waiter was left room for %d (%v), want at least one", room, err)
				}
			}
		})
	}
}

// A PURCHASE THAT MOVES ANOTHER TIER'S FLOOR ONTO THE WAITER'S HOST IS REFUSED.
// Charged against the waiter's fleet as first measured, the buyer's lease looked
// harmless; but the waiter's own escrow re-places the floor, and with host a at
// 14 and host b at 12 both fit it once, so the tie-break by name puts it on a and
// leaves the waiter, pinned to a, 6 vCPU for an 8 vCPU lease.
func TestEscrowLeavingSeesAFloorItWouldMove(t *testing.T) {
	t.Parallel()

	floor := tier("floor", 8, 8*config.GiB)
	floor.Reserved = 1
	waiter := tier("waiter", 8, 8*config.GiB)
	waiter.Node = "a"
	buyer := tier("buyer", 2, 2*config.GiB)
	buyer.Node = "a"

	a := newBareAllocator(t, Limits{MaxVCPU: 28, MaxMemory: 256 * config.GiB},
		[]config.Tier{floor, waiter, buyer})

	for name, vcpu := range map[string]int{"a": 16, "b": 12} {
		reg := testRegistration(name, config.ProviderFirecracker)
		reg.VCPU, reg.Memory = vcpu, 128*config.GiB
		mustRegister(t, a, reg)
	}

	got, err := a.EscrowLeaving(t.Context(), "buyer", 1, []string{"waiter"})
	if err != nil {
		t.Fatalf("EscrowLeaving: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("bought %d ahead of the waiter, though doing so moves the floor onto its only host", len(got))
	}

	// THE WAITER'S OWN ESCROW STILL GETS ITS LEASE, which is what was protected.
	leases, err := a.Escrow(t.Context(), "waiter", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("the waiter bought %d (%v), want its one lease", len(leases), err)
	}
}

// AND WITH NOTHING TO PROTECT IT IS ESCROW.
func TestEscrowLeavingWithNoWaitersIsEscrow(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier("small", 4, 4*config.GiB)})

	got, err := a.EscrowLeaving(t.Context(), "small", 4, nil)
	if err != nil || len(got) != 4 {
		t.Fatalf("bought %d (%v) with nothing to protect, want all 4", len(got), err)
	}
}
