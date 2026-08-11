package alloc

import (
	"context"
	"database/sql"
	"slices"

	"github.com/junioryono/billet/internal/config"
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
}

// fleetResources measures every reachable machine, inside the caller's
// transaction — which is what makes the whole thing atomic against a concurrent
// escrow. Measuring outside it would be a read followed by a hopeful write, the
// 7x overcommit this package exists to prevent.
func (a *Allocator) fleetResources(ctx context.Context, tx *sql.Tx) (*fleet, error) {
	nodes, err := a.liveNodes(ctx, tx)
	if err != nil {
		return nil, err
	}

	f := &fleet{
		vcpu:   make(map[string]int, len(nodes)),
		memory: make(map[string]config.ByteSize, len(nodes)),
		macOS:  make(map[string]int, len(nodes)),
	}

	for _, n := range nodes {
		used, err := a.usageOn(ctx, tx, n.name)
		if err != nil {
			return nil, err
		}

		f.vcpu[n.name] = n.vcpu - used.VCPU
		f.memory[n.name] = n.memory - used.Memory

		guests, err := a.countOpenMacOSByNode(ctx, tx, n.name)
		if err != nil {
			return nil, err
		}

		f.macOS[n.name] = a.macOSLimit(n.name) - guests
	}

	return f, nil
}

// forTier is a placer over the hosts one tier may use, spending the fleet's
// shared remaining resources.
//
// SHARED ON PURPOSE: whatever one tier's floor takes is gone from the machines
// every other tier is offered, because they are the same machines.
func (f *fleet) forTier(
	ctx context.Context, tx *sql.Tx, a *Allocator, t config.Tier,
) (*placer, error) {
	nodes, err := a.eligibleNodes(ctx, tx, t)
	if err != nil {
		return nil, err
	}

	p := &placer{
		order:      nodes,
		freeVCPU:   f.vcpu,
		freeMemory: f.memory,
		freeMacOS:  f.macOS,
		rank:       make(map[string]int, len(nodes)),
		policy:     a.placement.Or(),
	}

	p.rankBy(t)

	return p, nil
}

// placerWithFloors measures the fleet for a tier, with other tiers' outstanding
// floors already taken off the machines.
//
// ONE SEQUENCE FOR BOTH READERS. What a tier may advertise and what escrow may
// hand out have to be the same number computed the same way — if advertising
// counted room a floor was holding, GitHub would send work that escrow then
// refused, which reads as billet dropping jobs.
func (a *Allocator) placerWithFloors(
	ctx context.Context, tx *sql.Tx, t config.Tier,
) (*placer, int, config.ByteSize, error) {
	free, err := a.fleetResources(ctx, tx)
	if err != nil {
		return nil, 0, 0, err
	}

	vcpu, memory, err := a.reserveFloors(ctx, tx, t.Label, free)
	if err != nil {
		return nil, 0, 0, err
	}

	p, err := free.forTier(ctx, tx, a, t)

	return p, vcpu, memory, err
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
	room := min(p.freeVCPU[node]/t.VCPU, int(p.freeMemory[node]/t.Memory))

	if t.GuestOS == config.GuestMacOS {
		room = min(room, p.freeMacOS[node])
	}

	return max(room, 0)
}

// total is how many of a tier the candidate set can hold between them.
func (p *placer) total(t config.Tier) int {
	sum := 0
	for _, n := range p.order {
		sum += p.roomFor(n.name, t)
	}

	return sum
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
func (p *placer) next(t config.Tier) (string, bool) {
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
		return "", false
	}

	p.spend(best, t)

	return best, true
}

// spend takes one instance of a tier off a host.
func (p *placer) spend(node string, t config.Tier) {
	p.freeVCPU[node] -= t.VCPU
	p.freeMemory[node] -= t.Memory

	if t.GuestOS == config.GuestMacOS {
		p.freeMacOS[node]--
	}
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
