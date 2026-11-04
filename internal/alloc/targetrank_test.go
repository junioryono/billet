package alloc

import (
	"math"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// rankFleet is two targets' tiers on a host with room for all of them, under a
// ceiling of a hundred of each so a share reads as a percentage: the first tier
// binds on memory, the second on vCPU.
func rankFleet(t *testing.T, weights map[string]int) *Allocator {
	t.Helper()

	memoryBound := tier("memory-bound", 2, 20*config.GiB)
	memoryBound.Target = "first"

	vcpuBound := tier("vcpu-bound", 20, config.GiB)
	vcpuBound.Target = "second"

	return newAllocator(t, Limits{MaxVCPU: 100, MaxMemory: 100 * config.GiB, Weights: weights},
		[]config.Tier{memoryBound, vcpuBound})
}

// A TARGET'S RANK IS ITS DOMINANT SHARE OVER ITS WEIGHT: the larger of what it
// holds of the ceiling's vCPU and of its memory, so a target holding little vCPU
// and much memory is not ranked as if it held little.
func TestATargetsRankIsItsDominantShareOverItsWeight(t *testing.T) {
	a := rankFleet(t, map[string]int{"first": 4, "second": 1})

	nothing := admissionOf(t, a, "memory-bound")
	if nothing.TargetRank["first"] != 0 || nothing.TargetRank["second"] != 0 || len(nothing.TargetRank) != 2 {
		t.Errorf("with nothing held the ranks are %v; want both targets at zero, a target holding nothing "+
			"still being one the order can place", nothing.TargetRank)
	}

	reserve(t, a, "memory-bound")
	reserve(t, a, "vcpu-bound")

	got := admissionOf(t, a, "vcpu-bound").TargetRank

	// first: 2% of the vCPU and 20% of the memory, over 4. second: 20% of the
	// vCPU and 1% of the memory, over 1.
	if math.Abs(got["first"]-0.05) > 1e-9 || math.Abs(got["second"]-0.2) > 1e-9 {
		t.Errorf("the ranks are %v; want first 0.2/4 and second 0.2/1", got)
	}
}

// A TARGET THAT WRITES NO WEIGHT WEIGHS ONE, and with no weight anywhere there
// are no ranks at all, which is what leaves the order oldest first.
func TestRanksExistOnlyWhenATargetHasAWeight(t *testing.T) {
	if got := admissionOf(t, rankFleet(t, nil), "memory-bound").TargetRank; got != nil {
		t.Errorf("with no weight the ranks are %v; want none", got)
	}

	a := rankFleet(t, map[string]int{"first": 2})
	reserve(t, a, "vcpu-bound")

	if got := admissionOf(t, a, "memory-bound").TargetRank["second"]; math.Abs(got-0.2) > 1e-9 {
		t.Errorf("a target with no weight ranks %v; want its share over a weight of one", got)
	}
}

// A LEASE ON A TIER THAT LEFT THE CATALOGUE COUNTS ON NO TARGET: which it belonged
// to cannot be told, and charging it to one would rank that target behind for
// work it may not have.
func TestALeaseOfADepartedTierCountsOnNoTarget(t *testing.T) {
	a := rankFleet(t, map[string]int{"first": 1, "second": 1})
	reserve(t, a, "vcpu-bound")
	reserve(t, a, "memory-bound")

	kept := tier("memory-bound", 2, 20*config.GiB)
	kept.Target = "first"

	again, err := New(a.db, Limits{MaxVCPU: 100, MaxMemory: 100 * config.GiB,
		Weights: map[string]int{"first": 1, "second": 1}}, []config.Tier{kept})
	if err != nil {
		t.Fatalf("New without the tier: %v", err)
	}

	got := admissionOf(t, again, "memory-bound").TargetRank
	if _, listed := got["second"]; listed || math.Abs(got["first"]-0.2) > 1e-9 {
		t.Errorf("the ranks are %v; want first's own 20%% alone", got)
	}
}

// A WEIGHT THE CONFIG WOULD REFUSE IS REFUSED HERE TOO: New cannot prove its
// limits came through config.Parse.
func TestNewRefusesAWeightOutOfRange(t *testing.T) {
	for _, weight := range []int{0, -1, config.MaxTargetWeight + 1} {
		_, err := New(openTestLedger(t), Limits{MaxVCPU: 8, MaxMemory: 8 * config.GiB,
			Weights: map[string]int{"first": weight}}, []config.Tier{tier("small", 1, config.GiB)})
		if err == nil || !strings.Contains(err.Error(), `target "first": weight`) {
			t.Errorf("weight %d: New gave %v; want it refused by target", weight, err)
		}
	}
}

// A TIER BELOW ITS FLOOR SAYS SO, counted as the floor is, and one at it does not.
func TestATierBelowItsFloorSaysSo(t *testing.T) {
	reserved := tier("reserved", 2, 4*config.GiB)
	reserved.Reserved = 1

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 32 * config.GiB}, []config.Tier{reserved})

	if !admissionOf(t, a, "reserved").BelowFloor {
		t.Error("a tier holding nothing of a floor of one is not below it")
	}

	reserve(t, a, "reserved")

	if admissionOf(t, a, "reserved").BelowFloor {
		t.Error("a tier holding its floor is still below it")
	}
}
