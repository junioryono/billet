package alloc

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// placer hands out machines for reservations, one at a time.
//
// CHOOSING HAPPENS AT ESCROW, and that is the whole point. Escrow is what billet
// advertises against, so a reservation that has not chosen a machine is a
// promise it may not be able to keep — and it was worse than that: an unplaced
// lease was charged to the deployment and to no host, so the fleet's remaining
// room never shrank and the same slots were advertised again and again.
//
// IT TRACKS RESOURCES, NOT SLOTS. Counting "how many of this tier fit" per host
// is the natural way to write it and cannot express what floors need: a slot of
// a 4 vCPU tier and a slot of an 8 vCPU tier are not the same thing, so two
// tiers competing over one fleet cannot share a tally denominated in either.
// Free vCPU and free memory are the common currency.
type placer struct {
	// order is the eligible hosts, in a stable order.
	order []nodeRow
	// freeVCPU and freeMemory are what each host has left as choices are made.
	freeVCPU   map[string]int
	freeMemory map[string]config.ByteSize
	// freeMacOS is each host's remaining macOS guest licence, which is a COUNT
	// rather than a resource: Apple's limit is per machine and does not care how
	// large the guests are.
	freeMacOS map[string]int
	// cost is what one lease really consumes on each candidate. For EC2 this is
	// the first declared shape that fits, not the smaller tier request.
	cost map[string]placementCost
	// deploymentFree applies the deployment-wide ceiling to the same real cost.
	deploymentVCPU   int
	deploymentMemory config.ByteSize
	// rank is a host's position in the current tier's provider preference.
	rank map[string]int
	// policy decides between hosts that preference cannot separate.
	policy config.PlacementPolicy
}

// fleet is what every machine has left, measured once.
//
// MEASURED OVER THE WHOLE FLEET, NOT ONE TIER'S CANDIDATES, because tiers
// compete for the same machines and a floor belonging to another tier has to be
// held on the hosts IT could use. A per-tier measurement has no entry for a
// machine that tier cannot use — so a macOS floor weighed against a docker
// tier's candidates was held on docker boxes: room denied to protect a
// reservation that could never be kept there, and the Mac left untouched.
type fleet struct {
	vcpu   map[string]int
	memory map[string]config.ByteSize
	macOS  map[string]int
	// rows are the hosts measured, read once per question: every tier's
	// candidates, the asking tier's and each floor's, are filtered from them
	// rather than read from the ledger again.
	rows []nodeRow
}

type placementCost struct {
	vcpu         int
	memory       config.ByteSize
	instanceType string
	// price is what the shape costs per hour at the moment it is charged, and
	// zero for a host-backed provider, which buys nothing. Recorded on the
	// lease so the history says what was bought rather than what today's
	// catalogue would charge.
	price config.USDPerHour
}

// cost is what one lease of this tier really consumes on this host.
//
// A REMOTE BACKEND IS CHARGED FOR THE SHAPE IT BUYS, NOT THE TIER REQUEST, and the
// predicate is `RunsOnHost` rather than `== ec2` — which is what it was until a
// second remote backend existed. A host-backed provider's cost IS the tier request,
// because the machine is already paid for and the lease consumes exactly what it
// asked for. A remote one purchases a discrete shape, and the smallest that fits is
// usually larger than the request: charging the request instead under-counts every
// lease on that host, and the third remote backend added without touching this line
// would have reintroduced that under-count silently.
//
// FALSE MEANS "NO DECLARED SHAPE CAN HOLD THIS TIER", which is a filter rather than
// an error: eligibleNodes drops the host, so the tier is simply not placed there.
// A provider billet does not recognise lands there too, since RunsOnHost is an
// allowlist and such a host has no shapes — which is the safe direction, and is why
// RegisterNode refuses one outright rather than leaving it to this filter.
func (n nodeRow) cost(t config.Tier) (placementCost, bool) {
	if n.provider.RunsOnHost() {
		return placementCost{vcpu: t.VCPU, memory: t.Memory}, true
	}

	for _, shape := range n.shapes {
		if shape.VCPU >= t.VCPU && shape.Memory >= t.Memory {
			return placementCost{
				vcpu: shape.VCPU, memory: shape.Memory, instanceType: shape.Type,
				price: shape.PriceUSDPerHour,
			}, true
		}
	}

	return placementCost{}, false
}

// fleetResources measures every reachable machine, inside the caller's
// transaction — which is what makes the whole thing atomic against a concurrent
// escrow. Measuring outside it would be a read followed by a hopeful write, the
// 7x overcommit this package exists to prevent.
func (a *Allocator) fleetResources(ctx context.Context, tx querier) (*fleet, error) {
	nodes, err := a.liveNodes(ctx, tx)
	if err != nil {
		return nil, err
	}

	f := &fleet{
		vcpu:   make(map[string]int, len(nodes)),
		memory: make(map[string]config.ByteSize, len(nodes)),
		macOS:  make(map[string]int, len(nodes)),
		rows:   nodes,
	}

	// ONE QUERY FOR THE WHOLE FLEET, not two per host.
	//
	// This runs inside the allocator's single writer connection, and it runs on
	// every headroom question and again inside every escrow — so a per-node pair
	// of statements is O(nodes) round trips holding the one connection every
	// listener needs. The arithmetic is identical; only the number of statements
	// changes.
	used, err := a.usageByNode(ctx, tx)
	if err != nil {
		return nil, err
	}

	for _, n := range nodes {
		u := used[n.name]

		f.vcpu[n.name] = n.vcpu - u.VCPU
		f.memory[n.name] = n.memory - u.Memory
		f.macOS[n.name] = a.macOSLimit(n.name) - u.MacOS
	}

	return f, nil
}

// nodeUsage is what one host has committed, measured in one pass over the fleet.
type nodeUsage struct {
	VCPU   int
	Memory config.ByteSize
	MacOS  int
}

// usageByNode is every host's committed capacity, keyed the way the rest of the
// arithmetic attributes a lease.
//
// COALESCE(node, target_node), the same expression usageOn uses one host at a
// time. A lease that has been bound is
// charged to the host running it; one only AIMED at a host is charged there too,
// because escrow commits a machine's room before any container starts.
func (a *Allocator) usageByNode(ctx context.Context, tx querier) (map[string]nodeUsage, error) {
	rows, err := state.ReadQueries(tx).UsageByNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: measure the fleet's committed capacity: %w", err)
	}

	out := map[string]nodeUsage{}

	for _, row := range rows {
		out[row.Node] = nodeUsage{
			VCPU:   int(row.Vcpu),
			Memory: config.ByteSize(row.Memory),
			MacOS:  int(row.MacosSlots),
		}
	}

	return out, nil
}

// forTier is a placer over the hosts one tier may use, spending the fleet's
// shared remaining resources.
//
// SHARED ON PURPOSE: whatever one tier's floor takes is gone from the machines
// every other tier is offered, because they are the same machines.
func (f *fleet) forTier(a *Allocator, t config.Tier) *placer {
	nodes := a.eligibleFrom(f.rows, t, t.NeedsCacheAwareNode())

	p := &placer{
		order:            nodes,
		freeVCPU:         f.vcpu,
		freeMemory:       f.memory,
		freeMacOS:        f.macOS,
		cost:             make(map[string]placementCost, len(nodes)),
		deploymentVCPU:   int(^uint(0) >> 1),
		deploymentMemory: config.ByteSize(1<<63 - 1),
		rank:             make(map[string]int, len(nodes)),
		policy:           a.placement.Or(),
	}

	for _, n := range nodes {
		cost, ok := n.cost(t)
		if ok {
			p.cost[n.name] = cost
		}
	}

	p.rankBy(t)

	return p
}

// placerWithFloors measures the fleet for a tier, with other tiers' outstanding
// floors already taken off the machines.
//
// ONE SEQUENCE FOR BOTH READERS. What a tier may advertise and what escrow may
// hand out have to be the same number computed the same way — if advertising
// counted room a floor was holding, GitHub would send work that escrow then
// refused, which reads as billet dropping jobs.
func (a *Allocator) placerWithFloors(
	ctx context.Context, tx querier, t config.Tier,
) (*placer, floorCharge, error) {
	free, err := a.fleetResources(ctx, tx)
	if err != nil {
		return nil, floorCharge{}, err
	}

	floors, err := a.reserveFloors(ctx, tx, t.Label, free, placementCost{})
	if err != nil {
		return nil, floorCharge{}, err
	}

	return free.forTier(a, t), floors, nil
}

// rankBy scores the hosts against one tier's provider preference.
//
// SEPARATE FROM THE MEASUREMENT, because another tier's floor is weighed against
// the SAME machines under a DIFFERENT order of preference.
func (p *placer) rankBy(t config.Tier) {
	accepts := t.AcceptableProviders()

	for _, n := range p.order {
		p.rank[n.name] = slices.Index(accepts, n.provider)
	}
}

// roomFor is how many more of a tier a host can still take.
func (p *placer) roomFor(node string, t config.Tier) int {
	cost, ok := p.cost[node]
	if !ok {
		return 0
	}

	room := min(p.freeVCPU[node]/cost.vcpu, int(p.freeMemory[node]/cost.memory))
	room = min(room, p.deploymentVCPU/cost.vcpu)
	room = min(room, int(p.deploymentMemory/cost.memory))

	if t.GuestOS == config.GuestMacOS {
		room = min(room, p.freeMacOS[node])
	}

	return max(room, 0)
}

// total is how many of a tier the candidate set can hold between them.
//
// COUNTED, NOT PLACED, ONE PREFERENCE RANK AT A TIME. Placing the jobs one at
// a time costs the hosts times the free slots, and it runs inside the
// purchase's write transaction: 30 ms of the ledger's one writer slot per
// purchase over 128 hosts, and 68 ms over 256 for a tier falling back to a
// remote shape (docs/reference/records/ledger-write-budget.md). next prefers
// rank above everything else, so the greedy count fills every host of one rank
// before it takes any of the next, and comes back to none: what moves it on
// is the rank's hosts having no room, and nothing it spends afterwards gives
// any back. Within a rank whose candidates charge the same, each placement
// takes exactly one off its host's room and one cost off the deployment's, so
// the rank's count is the smaller of the two and the order it would have
// chosen does not matter. A rank whose candidates charge differently (remote
// nodes of one backend with different shapes) spends the deployment's room
// unevenly, so its order does matter, and its hosts alone are placed one at a
// time.
func (p *placer) total(t config.Tier) int {
	n, _ := p.countByRank(t)

	return n
}

// countByRank is total, and how many ranks it counted in one pass rather than
// placing one job at a time, which a test reads to know which way it went.
func (p *placer) countByRank(t config.Tier) (int, int) {
	var ranks []int

	for _, n := range p.order {
		if _, ok := p.cost[n.name]; ok && !slices.Contains(ranks, p.rank[n.name]) {
			ranks = append(ranks, p.rank[n.name])
		}
	}

	slices.Sort(ranks)

	deploymentVCPU, deploymentMemory := p.deploymentVCPU, p.deploymentMemory
	sum, counted := 0, 0

	for _, rank := range ranks {
		var group []nodeRow

		for _, n := range p.order {
			if _, ok := p.cost[n.name]; ok && p.rank[n.name] == rank {
				group = append(group, n)
			}
		}

		if cost, ok := p.oneCost(group); ok {
			n := p.uniformCount(t, group, cost, deploymentVCPU, deploymentMemory)
			sum += n
			counted++
			deploymentVCPU -= n * cost.vcpu
			deploymentMemory -= config.ByteSize(n) * cost.memory

			continue
		}

		// PLACED ONE AT A TIME, over this rank's hosts alone: every rank before
		// it has no room left and none after it is chosen while it has some.
		placed := p.clone()
		placed.order = group
		placed.deploymentVCPU, placed.deploymentMemory = deploymentVCPU, deploymentMemory

		for {
			if _, _, ok := placed.next(t); !ok {
				break
			}
			sum++
		}

		deploymentVCPU, deploymentMemory = placed.deploymentVCPU, placed.deploymentMemory
	}

	return sum, counted
}

// oneCost is the cost every host of group charges, and false when they charge
// differently or the cost has a dimension no room can be divided by.
func (p *placer) oneCost(group []nodeRow) (placementCost, bool) {
	cost := p.cost[group[0].name]

	for _, n := range group[1:] {
		if c := p.cost[n.name]; c.vcpu != cost.vcpu || c.memory != cost.memory {
			return placementCost{}, false
		}
	}

	return cost, cost.vcpu > 0 && cost.memory > 0
}

// uniformCount is how many of a tier group holds between its hosts when every
// one of them charges cost, under the deployment room given.
func (p *placer) uniformCount(
	t config.Tier, group []nodeRow, cost placementCost, deploymentVCPU int, deploymentMemory config.ByteSize,
) int {
	deployment := max(min(deploymentVCPU/cost.vcpu, int(deploymentMemory/cost.memory)), 0)

	// EACH HOST'S OWN ROOM, which roomFor computes before the deployment's
	// ceiling: one placement takes exactly one off it, since every term of the
	// minimum falls by one. CAPPED AS IT IS ADDED, at the deployment's room,
	// which is the most the sum can contribute and keeps hosts large enough to
	// overflow an int from summing past it.
	hosts := 0

	for _, n := range group {
		room := min(p.freeVCPU[n.name]/cost.vcpu, int(p.freeMemory[n.name]/cost.memory))
		if t.GuestOS == config.GuestMacOS {
			room = min(room, p.freeMacOS[n.name])
		}

		hosts += min(max(room, 0), deployment-hosts)
	}

	return hosts
}

// next picks the machine a reservation should be aimed at, and spends it.
//
// PREFERENCE, THEN POLICY, THEN NAME.
//
// Preference is the operator's INSTRUCTION — `[firecracker, ec2]` means the
// machine at home before the cloud — and a tuning knob must never overrule an
// instruction, which is why it ranks above the policy rather than blending with
// it.
//
// The policy then separates hosts preference cannot: pack fills a machine before
// starting the next, spread keeps them even. The name is the final tie-break,
// and it is not cosmetic — Go map iteration is randomised, so without a total
// order one fleet gives different answers on different runs, which cannot be
// reproduced from a log or tested at all.
func (p *placer) next(t config.Tier) (string, placementCost, bool) {
	best := ""

	for _, n := range p.order {
		if p.roomFor(n.name, t) <= 0 {
			continue
		}

		if best == "" || p.better(n.name, best, t) {
			best = n.name
		}
	}

	if best == "" {
		return "", placementCost{}, false
	}

	cost := p.cost[best]
	p.spend(best, t)

	return best, cost, true
}

// siteOf is the registered site of one candidate host, which escrow records on
// the lease it aims there. Empty for a host that declared none.
func (p *placer) siteOf(node string) string {
	for _, n := range p.order {
		if n.name == node {
			return n.site
		}
	}

	return ""
}

// spend takes one instance of a tier off a host.
func (p *placer) spend(node string, t config.Tier) {
	cost := p.cost[node]
	p.freeVCPU[node] -= cost.vcpu
	p.freeMemory[node] -= cost.memory
	p.deploymentVCPU -= cost.vcpu
	p.deploymentMemory -= cost.memory

	if t.GuestOS == config.GuestMacOS {
		p.freeMacOS[node]--
	}
}

func (p *placer) clone() *placer {
	clone := *p
	clone.freeVCPU = maps.Clone(p.freeVCPU)
	clone.freeMemory = maps.Clone(p.freeMemory)
	clone.freeMacOS = maps.Clone(p.freeMacOS)
	clone.cost = maps.Clone(p.cost)
	clone.rank = maps.Clone(p.rank)

	return &clone
}

// better reports whether a should be chosen over b for this tier.
func (p *placer) better(a, b string, t config.Tier) bool {
	if p.rank[a] != p.rank[b] {
		return p.rank[a] < p.rank[b]
	}

	if roomA, roomB := p.roomFor(a, t), p.roomFor(b, t); roomA != roomB {
		if p.policy == config.PlacementSpread {
			return roomA > roomB
		}

		// LESS ROOM WINS. Filling a machine keeps the others whole, so the fleet
		// can still hold a tier larger than any fragment would fit.
		return roomA < roomB
	}

	return a < b
}
