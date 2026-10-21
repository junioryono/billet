package alloc

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// resizeFleet is a cloud tier on an EC2 node that buys 4 or 8 vCPU, and an
// 8 vCPU tier on a host of its own, under a deployment ceiling of maxVCPU. It
// returns the cloud tier's lease, launching at the 4 vCPU shape it was escrowed
// at, which is the moment a fallback asks to grow it.
func resizeFleet(t *testing.T, maxVCPU int) (*Allocator, *Lease) {
	t.Helper()

	cloud := tier("cloud", 2, 4*config.GiB)
	cloud.Provider = config.ProviderEC2
	waiter := tier("waiter", 8, 16*config.GiB)
	waiter.Node = "home"

	a := newBareAllocator(t, Limits{MaxVCPU: maxVCPU, MaxMemory: 256 * config.GiB},
		[]config.Tier{cloud, waiter})

	mustRegister(t, a, NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 128 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "four", VCPU: 4, Memory: 8 * config.GiB},
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
		},
	})
	mustRegister(t, a, NodeRegistration{
		Name: "home", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
	})

	lease := reserve(t, a, "cloud")
	if lease.VCPU != 4 {
		t.Fatalf("the cloud lease was charged %d vCPU, want the 4 vCPU shape it buys first", lease.VCPU)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	return a, lease
}

// chargedVCPU is what a lease is charged now.
func chargedVCPU(t *testing.T, a *Allocator, id string) int {
	t.Helper()

	got, err := a.Lease(t.Context(), id)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	return got.VCPU
}

// A FALLBACK TO A LARGER SHAPE ASKS THE ORDER (#172). Measured on 2026-09-22:
// under an 8 vCPU ceiling, with a launching cloud lease charged 4 and an 8 vCPU
// tier waiting, the queue refused the small tier another purchase while the
// fallback grew its lease from 4 to 8. Resize alone still grants that, which is
// what the budget allows; ResizeLeaving with the waiter to protect refuses it.
func TestAFallbackDoesNotGrowIntoRoomAWaiterIsAccumulating(t *testing.T) {
	a, lease := resizeFleet(t, 8)

	err := a.ResizeLeaving(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB, []string{"waiter"})
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "admission order holds for waiter") {
		t.Fatalf("a fallback growing into the waiter's room returned %v, want a no-capacity naming the order", err)
	}

	if got := chargedVCPU(t, a, lease.ID); got != 4 {
		t.Errorf("the refused fallback left the lease charged %d vCPU, want the 4 it had", got)
	}

	// AND THE LEASE'S OWN TIER IN protect IS NOT A WAITER IT MUST LEAVE ROOM
	// FOR: the same growth, which leaves the cloud tier no room for another of
	// its own, is granted when only its own tier is named.
	own, ownLease := resizeFleet(t, 8)

	if err := own.ResizeLeaving(t.Context(), ownLease.ID, ownLease.Epoch, "eight", 8, 16*config.GiB,
		[]string{"cloud"}); err != nil {
		t.Errorf("a growth was refused for its own tier: %v", err)
	}

	if got := chargedVCPU(t, own, ownLease.ID); got != 8 {
		t.Errorf("the growth protecting only its own tier left the lease charged %d vCPU, want 8", got)
	}

	// THE BUDGET ALONE ALLOWS IT, or this case would not tell the order from
	// the ceiling.
	if err := a.Resize(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB); err != nil {
		t.Fatalf("the budget alone refused the fallback (%v); this case cannot tell the order from the ceiling", err)
	}

	// AND A SHRINK IS NEVER JUDGED: back from 8 to 4 gives the waiter room, and
	// is granted though the waiter is still short of the 8 it needs after it.
	if err := a.ResizeLeaving(t.Context(), lease.ID, lease.Epoch, "four", 4, 8*config.GiB,
		[]string{"waiter"}); err != nil {
		t.Errorf("a shrink was refused for a waiter it gives room to: %v", err)
	}
}

// AND ONE THAT LEAVES THE WAITER ITS ROOM IS GRANTED, judged after the new
// shape is charged; one that would take that room is rolled back.
func TestAFallbackThatLeavesTheWaiterRoomIsGranted(t *testing.T) {
	roomy, lease := resizeFleet(t, 16)

	if err := roomy.ResizeLeaving(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB,
		[]string{"waiter"}); err != nil {
		t.Fatalf("a fallback that leaves the waiter 8 vCPU was refused: %v", err)
	}

	if got := chargedVCPU(t, roomy, lease.ID); got != 8 {
		t.Errorf("the granted fallback left the lease charged %d vCPU, want 8", got)
	}

	// Twelve: the waiter has its 8 now and would have 4 after the growth.
	tight, lease := resizeFleet(t, 12)

	err := tight.ResizeLeaving(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB, []string{"waiter"})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a fallback that would leave the waiter 4 vCPU of the 8 it needs returned %v, want no capacity", err)
	}

	if got := chargedVCPU(t, tight, lease.ID); got != 4 {
		t.Errorf("the refused fallback was not rolled back: the lease is charged %d vCPU, want 4", got)
	}
}
