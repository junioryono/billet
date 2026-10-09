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

	uniformChecked := 0

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

	if uniformChecked < 10000 {
		t.Fatalf("only %d of 20000 cases were counted in one pass", uniformChecked)
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
