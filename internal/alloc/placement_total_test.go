package alloc

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// randomPlacer is a placer over a random fleet: hosts with random free room
// (some overcommitted), some with no cost for the tier, costs either all the
// same or varied, a deployment ceiling from none to tight, either policy and a
// spread of provider ranks.
func randomPlacer(r *rand.Rand, uniform bool) *placer {
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

	shared := placementCost{vcpu: 1 + r.IntN(8), memory: config.ByteSize(1+r.IntN(16)) * config.GiB}

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
		case uniform:
			p.cost[name] = shared
		default:
			p.cost[name] = placementCost{vcpu: 1 + r.IntN(8), memory: config.ByteSize(1+r.IntN(16)) * config.GiB}
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

// THE COUNT IS THE GREEDY COUNT. Whenever every candidate charges the same,
// counting in one pass gives exactly what placing one job at a time gives,
// across random fleets, ceilings, policies, ranks and macOS licences; and
// where they do not, total still answers what the greedy count does.
func TestCountingHeadroomGivesTheGreedyCount(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(2026, 1008))

	var uniformChecked, ceilingBound, macOS int

	for i := range 20000 {
		uniform := r.IntN(4) != 0

		p := randomPlacer(r, uniform)

		tier := config.Tier{Label: "t", GuestOS: config.GuestLinux}
		if r.IntN(3) == 0 {
			tier.GuestOS = config.GuestMacOS
		}

		want := p.greedyTotal(tier)

		if got, ok := p.uniformTotal(tier); ok {
			uniformChecked++

			if tier.GuestOS == config.GuestMacOS {
				macOS++
			}

			// BOUND BY THE CEILING when lifting it would place more.
			lifted := p.clone()
			lifted.deploymentVCPU, lifted.deploymentMemory = int(^uint(0)>>1), config.ByteSize(1<<63-1)

			if lifted.greedyTotal(tier) > want {
				ceilingBound++
			}

			if got != want {
				t.Fatalf("case %d: counted %d, placed %d, on %+v", i, got, want, *p)
			}
		} else if uniform {
			t.Fatalf("case %d: one cost for every candidate and no count: %+v", i, p.cost)
		}

		if got := p.total(tier); got != want {
			t.Fatalf("case %d: total %d, placed %d", i, got, want)
		}
	}

	// THE CASES THAT MATTER WERE MADE, not only possible: counted in one pass,
	// on macOS, and with the deployment's ceiling the binding bound.
	if uniformChecked < 10000 || macOS < 2000 || ceilingBound < 2000 {
		t.Fatalf("of 20000 cases %d were counted in one pass, %d on macOS and %d bound by the ceiling",
			uniformChecked, macOS, ceilingBound)
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
		p := randomPlacer(r, true)
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
