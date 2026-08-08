package alloc

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// smallTier and bigTier share one budget, which is the situation every real
// deployment is in: Spendify's own catalogue is 2, 4 and 8 vCPU tiers on one
// machine.
func smallTier(reserved int) config.Tier {
	return config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Reserved: reserved,
	}
}

func bigTier(reserved int) config.Tier {
	return config.Tier{
		Label: "billet-8vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 8, Memory: 16 * config.GiB, Reserved: reserved,
	}
}

// fill reserves until a tier can take no more, and reports how many it got.
func fill(t *testing.T, a *Allocator, label string) int {
	t.Helper()

	var n int

	for {
		_, err := a.Reserve(t.Context(), label)
		if errors.Is(err, ErrNoCapacity) {
			return n
		}

		if err != nil {
			t.Fatalf("Reserve %s: %v", label, err)
		}

		n++
	}
}

// WITHOUT A FLOOR, ONE TIER TAKES EVERYTHING. This is the behaviour being fixed,
// asserted rather than described so that a later change cannot quietly alter it.
//
// Nothing here is a bug in the allocator's accounting: the ledger balances
// perfectly and every reservation is legal. The problem is that the other tier
// then advertises zero capacity to GitHub, and its jobs queue indefinitely with
// no error anywhere in the system.
func TestWithoutAFloorOneTierTakesTheWholeBudget(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(0), bigTier(0)})

	if got := fill(t, a, "billet-2vcpu"); got != 16 {
		t.Fatalf("the small tier took %d slots, want the whole 32 vCPU budget (16)", got)
	}

	// And the other tier is left with nothing at all.
	if got := fill(t, a, "billet-8vcpu"); got != 0 {
		t.Errorf("the big tier got %d slots; the point of this test is that it gets none", got)
	}
}

// A FLOOR IS HELD BACK from a tier that would otherwise take everything.
func TestAFloorIsHeldBackFromAGreedyTier(t *testing.T) {
	t.Parallel()

	// The big tier is guaranteed two slots: 16 of the 32 vCPU.
	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(0), bigTier(2)})

	// The small tier can now only take what is left over.
	if got := fill(t, a, "billet-2vcpu"); got != 8 {
		t.Fatalf("the small tier took %d slots, want 8 (16 vCPU, with 16 reserved)", got)
	}

	// And the guarantee is real: the big tier gets exactly its floor.
	if got := fill(t, a, "billet-8vcpu"); got != 2 {
		t.Errorf("the reserved tier got %d slots, want its floor of 2", got)
	}
}

// A tier does not hold capacity back FROM ITSELF.
//
// Its own floor is not an obstacle to filling it, or a reservation would make a
// tier unable to use the capacity reserved for it.
func TestATiersOwnFloorDoesNotBlockIt(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{bigTier(2)})

	if got := fill(t, a, "billet-8vcpu"); got != 4 {
		t.Errorf("a tier reserving 2 slots could take %d of a 4-slot budget", got)
	}
}

// ONCE A FLOOR IS MET, THE TIER COMPETES FOR THE REST ON EQUAL TERMS.
//
// Deducting a whole floor regardless would idle capacity a tier has already
// claimed — the reservation would become a quota rather than a guarantee.
func TestAMetFloorStopsHoldingCapacityBack(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(0), bigTier(2)})

	// The big tier claims its guarantee first.
	for range 2 {
		if _, err := a.Reserve(t.Context(), "billet-8vcpu"); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}

	// Its floor is met, so nothing is held back any more: the small tier gets
	// the entire remainder rather than a remainder minus a floor already filled.
	if got := fill(t, a, "billet-2vcpu"); got != 8 {
		t.Errorf("the small tier took %d slots, want the whole 16 vCPU remainder (8)", got)
	}
}

// A PARTIALLY met floor holds back only the missing part.
func TestAPartialFloorHoldsBackOnlyWhatIsMissing(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(0), bigTier(2)})

	if _, err := a.Reserve(t.Context(), "billet-8vcpu"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// 8 vCPU used, 8 still owed to the big tier, so 16 remain for the small one.
	if got := fill(t, a, "billet-2vcpu"); got != 8 {
		t.Errorf("the small tier took %d slots, want 8", got)
	}
}

// Floors that do not fit the machine are refused at construction.
//
// Individually legal reservations can sum past the budget, and the failure is
// invisible where it happens: every tier deducts every other tier's unmet floor,
// so EVERY tier computes zero headroom and the deployment advertises nothing
// while reporting no error.
func TestFloorsThatDoNotFitAreRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		limits Limits
		tiers  []config.Tier
		want   string
	}{
		"more vCPU than the machine has": {
			limits: Limits{MaxVCPU: 16, MaxMemory: 256 * config.GiB},
			tiers:  []config.Tier{smallTier(4), bigTier(2)}, // 8 + 16 = 24 > 16
			want:   "vCPU",
		},
		"more memory than the machine has": {
			limits: Limits{MaxVCPU: 256, MaxMemory: 16 * config.GiB},
			tiers:  []config.Tier{bigTier(2)}, // 32GiB > 16GiB
			want:   "budget",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db := openState(t)

			_, err := New(db, tc.limits, tc.tiers)
			if err == nil {
				t.Fatal("accepted floors that cannot all be honoured; every tier would " +
					"compute zero headroom and billet would advertise nothing")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not explain the problem: %v", err)
			}
		})
	}
}

// A floor above a tier's own ceiling can never be filled, and would strand the
// capacity it holds back from everyone else.
func TestAFloorAboveItsOwnCeilingIsRefused(t *testing.T) {
	t.Parallel()

	tier := bigTier(4)
	tier.MaxConcurrent = 2

	db := openState(t)

	if _, err := New(db, Limits{MaxVCPU: 256, MaxMemory: 512 * config.GiB},
		[]config.Tier{tier}); err == nil {
		t.Fatal("accepted a reservation larger than the tier's own cap")
	}
}

func TestANegativeFloorIsRefused(t *testing.T) {
	t.Parallel()

	db := openState(t)

	if _, err := New(db, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(-1)}); err == nil {
		t.Fatal("accepted a negative reservation")
	}
}

// Reserving nothing is the default, and it leaves behaviour exactly as it was.
func TestNoFloorsBehavesAsBefore(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 64 * config.GiB},
		[]config.Tier{smallTier(0), bigTier(0)})

	// The whole budget is available to whichever tier asks.
	if got := fill(t, a, "billet-8vcpu"); got != 4 {
		t.Errorf("with no floors the big tier took %d of 4 slots", got)
	}
}

// The vCPU floor is held back INDEPENDENTLY of the memory floor.
//
// Both constraints are computed, and headroom is their minimum — so a test whose
// tiers are sized such that either one alone gives the same answer cannot tell
// which is working. Removing the vCPU deduction survived exactly that way.
//
// Memory is deliberately abundant here, so vCPU is the only thing that binds.
func TestTheVCPUFloorIsHeldBackOnItsOwn(t *testing.T) {
	t.Parallel()

	small := smallTier(0)
	small.Memory = 1 * config.GiB

	big := bigTier(2)
	big.Memory = 1 * config.GiB

	// 32 vCPU, and enough memory that it never constrains anything.
	a := newAllocator(t, Limits{MaxVCPU: 32, MaxMemory: 1024 * config.GiB},
		[]config.Tier{small, big})

	// 16 vCPU reserved for the big tier leaves 16 for the small one: 8 slots.
	if got := fill(t, a, "billet-2vcpu"); got != 8 {
		t.Errorf("the small tier took %d slots, want 8 — the vCPU floor is not being "+
			"held back on its own", got)
	}
}

// And the memory floor likewise, with vCPU abundant.
func TestTheMemoryFloorIsHeldBackOnItsOwn(t *testing.T) {
	t.Parallel()

	small := smallTier(0)
	small.VCPU = 1

	big := bigTier(2)
	big.VCPU = 1

	// 64GiB, and enough vCPU that it never constrains anything.
	a := newAllocator(t, Limits{MaxVCPU: 4096, MaxMemory: 64 * config.GiB},
		[]config.Tier{small, big})

	// 32GiB reserved for the big tier leaves 32GiB for the small one at 4GiB
	// each: 8 slots.
	if got := fill(t, a, "billet-2vcpu"); got != 8 {
		t.Errorf("the small tier took %d slots, want 8 — the memory floor is not being "+
			"held back on its own", got)
	}
}
