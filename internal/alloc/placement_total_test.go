package alloc

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// greedyTotal counts by placing the tier's jobs one at a time, as next would,
// on a copy of the placer: the count total is held to.
func (p *placer) greedyTotal(t config.Tier) int {
	clone := p.clone()
	sum := 0
	for {
		if _, _, ok := clone.next(t); !ok {
			return sum
		}
		sum++
	}
}

// costShape is how a random fleet's hosts charge for the tier.
type costShape int

const (
	// costsVaried charges each host its own cost.
	costsVaried costShape = iota
	// costsUniform charges every host one cost.
	costsUniform
	// costsPerRank charges the hosts of one rank one cost and another rank
	// another, as a tier falling back from its hosts to a remote shape does;
	// some ranks are varied within.
	costsPerRank
)

// randomPlacer is a placer over a random fleet: hosts with random free room
// (some overcommitted), some with no cost for the tier, costs all the same,
// one per rank or varied, a deployment ceiling from none to tight, either
// policy and a spread of provider ranks.
func randomPlacer(r *rand.Rand, shape costShape) *placer {
	hosts := r.IntN(12)

	p := &placer{
		freeVCPU:         map[string]int{},
		freeMemory:       map[string]config.ByteSize{},
		freeMacOS:        map[string]int{},
		cost:             map[string]placementCost{},
		rank:             map[string]int{},
		deploymentVCPU:   int(^uint(0) >> 1),
		deploymentMemory: config.ByteSize(1<<63 - 1),
		policy:           []config.PlacementPolicy{config.PlacementPack, config.PlacementSpread}[r.IntN(2)],
	}

	randomCost := func() placementCost {
		return placementCost{vcpu: 1 + r.IntN(8), memory: config.ByteSize(1+r.IntN(16)) * config.GiB}
	}

	shared := randomCost()
	perRank := []placementCost{randomCost(), randomCost(), randomCost()}
	variedRank := r.IntN(6)

	for i := range hosts {
		name := fmt.Sprintf("host-%02d", i)
		p.order = append(p.order, nodeRow{name: name})

		p.freeVCPU[name] = r.IntN(80) - 8
		p.freeMemory[name] = config.ByteSize(r.IntN(160)-16) * config.GiB
		p.freeMacOS[name] = r.IntN(4) - 1
		p.rank[name] = r.IntN(3)

		switch {
		case r.IntN(6) == 0:
			// No cost: this host cannot take the tier.
		case shape == costsUniform:
			p.cost[name] = shared
		case shape == costsPerRank && p.rank[name] != variedRank:
			p.cost[name] = perRank[p.rank[name]]
		default:
			p.cost[name] = randomCost()
		}
	}

	switch r.IntN(3) {
	case 0:
		// No ceiling but the fleet.
	case 1:
		p.deploymentVCPU = r.IntN(200) - 10
		p.deploymentMemory = config.ByteSize(r.IntN(400)-20) * config.GiB
	default:
		p.deploymentVCPU = r.IntN(40)
		p.deploymentMemory = config.ByteSize(r.IntN(80)) * config.GiB
	}

	return p
}

// THE COUNT IS THE GREEDY COUNT. Counting a rank whose candidates charge the
// same in one pass, and placing the rest one at a time over their own rank's
// hosts, gives exactly what placing every job one at a time gives, across
// random fleets, ceilings, policies, ranks and macOS licences.
func TestCountingHeadroomGivesTheGreedyCount(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(2026, 1008))

	var uniformCounted, rankedCounted, mixedRanks, ceilingBound, macOS int

	for i := range 30000 {
		shape := []costShape{costsVaried, costsUniform, costsPerRank}[r.IntN(3)]

		p := randomPlacer(r, shape)

		tier := config.Tier{Label: "t", GuestOS: config.GuestLinux}
		if r.IntN(3) == 0 {
			tier.GuestOS = config.GuestMacOS
		}

		want := p.greedyTotal(tier)
		got, counted := p.countByRank(tier)

		ranks := map[int]bool{}
		for _, n := range p.order {
			if _, ok := p.cost[n.name]; ok {
				ranks[p.rank[n.name]] = true
			}
		}

		// BOUND BY THE CEILING when lifting it would place more.
		lifted := p.clone()
		lifted.deploymentVCPU, lifted.deploymentMemory = int(^uint(0)>>1), config.ByteSize(1<<63-1)

		switch {
		case shape == costsUniform && counted != len(ranks):
			t.Fatalf("case %d: one cost for every candidate and %d of %d ranks counted: %+v",
				i, counted, len(ranks), p.cost)
		case shape == costsUniform && counted > 0:
			uniformCounted++
		case shape == costsPerRank && counted > 0 && len(ranks) > 1:
			rankedCounted++

			// A RANK COUNTED BESIDE ONE PLACED, so the deployment room the one
			// leaves the other is what is being checked.
			if counted < len(ranks) {
				mixedRanks++
			}
		}

		if counted > 0 && lifted.greedyTotal(tier) > want {
			ceilingBound++
		}

		if counted > 0 && tier.GuestOS == config.GuestMacOS {
			macOS++
		}

		if got != want {
			t.Fatalf("case %d: counted %d, placed %d, on %+v", i, got, want, *p)
		}

		if total := p.total(tier); total != got {
			t.Fatalf("case %d: total %d, countByRank %d", i, total, got)
		}
	}

	// THE CASES THAT MATTER WERE MADE, not only possible: one cost counted in
	// one pass, several ranks each counted at its own cost, a counted rank
	// beside a placed one, on macOS, and with the deployment's ceiling binding.
	if uniformCounted < 5000 || rankedCounted < 3000 || mixedRanks < 1000 || macOS < 3000 || ceilingBound < 3000 {
		t.Fatalf("of 30000 cases %d were counted at one cost, %d at a cost per rank (%d beside a placed rank), "+
			"%d on macOS and %d bound by the ceiling", uniformCounted, rankedCounted, mixedRanks, macOS, ceilingBound)
	}
}

// A FLEET TOO LARGE TO SUM: two hosts whose rooms each fill an int, under a
// ceiling of one. Placing stops after one; adding the rooms first wrapped and
// answered none.
func TestCountingHeadroomDoesNotOverflow(t *testing.T) {
	t.Parallel()

	const huge = int(^uint(0) >> 1)

	p := &placer{
		order:            []nodeRow{{name: "a"}, {name: "b"}},
		freeVCPU:         map[string]int{"a": huge, "b": huge},
		freeMemory:       map[string]config.ByteSize{"a": 1<<63 - 1, "b": 1<<63 - 1},
		freeMacOS:        map[string]int{},
		cost:             map[string]placementCost{"a": {vcpu: 1, memory: 1}, "b": {vcpu: 1, memory: 1}},
		rank:             map[string]int{},
		deploymentVCPU:   1,
		deploymentMemory: 1,
		policy:           config.PlacementPack,
	}

	tier := config.Tier{Label: "t", GuestOS: config.GuestLinux}

	if got, want := p.total(tier), p.greedyTotal(tier); got != want || want != 1 {
		t.Fatalf("counted %d, placed %d, want 1", got, want)
	}
}

// AND THE PLACER IS UNTOUCHED: counting must not spend what it counts, as the
// greedy count spends only its copy.
func TestCountingHeadroomSpendsNothing(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(7, 7))

	for range 200 {
		p := randomPlacer(r, costsPerRank)
		before := *p.clone()

		p.total(config.Tier{Label: "t", GuestOS: config.GuestMacOS})

		for _, n := range p.order {
			if p.freeVCPU[n.name] != before.freeVCPU[n.name] || p.freeMemory[n.name] != before.freeMemory[n.name] ||
				p.freeMacOS[n.name] != before.freeMacOS[n.name] {
				t.Fatalf("counting spent %s's room", n.name)
			}
		}

		if p.deploymentVCPU != before.deploymentVCPU || p.deploymentMemory != before.deploymentMemory {
			t.Fatal("counting spent the deployment's room")
		}
	}
}
