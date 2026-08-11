package alloc

import (
	"context"
	"database/sql"
	"slices"

	"github.com/junioryono/billet/internal/config"
)

// placer hands out machines for a tier's reservations, one at a time.
//
// CHOOSING HAPPENS AT ESCROW, and that is the whole point. Escrow is what billet
// advertises against, so a reservation that has not chosen a machine is a
// promise it may not be able to keep — and it was worse than that: an unplaced
// lease was charged to the deployment and to no host, so the fleet's remaining
// room never shrank and the same slots were advertised again and again.
//
// It also has to hand out a DIFFERENT machine as room runs out, which is why
// this is a small stateful object rather than a function called in a loop.
// Recomputing from the ledger each time would return the same answer every time,
// because none of the previous choices is durable until the transaction commits.
type placer struct {
	// order is the eligible hosts, best first.
	order []nodeRow
	// room is how many more of this tier each host can still take.
	room map[string]int
	// rank is a host's position in the tier's provider preference.
	rank map[string]int
	// policy decides between hosts that preference cannot separate.
	policy config.PlacementPolicy
}

// newPlacer measures the fleet for one tier.
//
// INSIDE THE CALLER'S TRANSACTION, which is what makes the whole thing atomic
// against a concurrent escrow. Measuring outside it would be a read followed by
// a hopeful write — the 7x overcommit this package exists to prevent.
func (a *Allocator) newPlacer(ctx context.Context, tx *sql.Tx, t config.Tier) (*placer, error) {
	nodes, err := a.eligibleNodes(ctx, tx, t)
	if err != nil {
		return nil, err
	}

	p := &placer{
		order:  nodes,
		room:   make(map[string]int, len(nodes)),
		rank:   make(map[string]int, len(nodes)),
		policy: a.placement.Or(),
	}

	accepts := t.AcceptableProviders()

	for _, n := range nodes {
		room, err := a.headroomOn(ctx, tx, n, t)
		if err != nil {
			return nil, err
		}

		p.room[n.name] = room

		// THE TIER'S OWN ORDER OF PREFERENCE, which is what finally makes
		// `providers: [firecracker, ec2]` mean "the machine at home first, the
		// cloud if you must". It has been recorded and ignored until now.
		p.rank[n.name] = slices.Index(accepts, n.provider)
	}

	return p, nil
}

// next picks the machine this reservation should be aimed at.
//
// PREFERENCE FIRST, THEN THE POLICY, THEN THE NAME.
//
// Preference is the operator's INSTRUCTION — `[firecracker, ec2]` means the
// machine at home before the cloud — and a tuning knob must never overrule an
// instruction, which is why it is ranked above the policy rather than blended
// with it.
//
// The policy then separates hosts preference cannot: pack fills a machine before
// starting the next, spread keeps them even. The name is the final tie-break,
// and it is not cosmetic — Go map iteration is randomised, so without a total
// order one fleet gives different answers on different runs, which cannot be
// reproduced from a log or tested at all.
func (p *placer) next() (string, bool) {
	best := ""

	for _, n := range p.order {
		if p.room[n.name] <= 0 {
			continue
		}

		if best == "" || p.better(n.name, best) {
			best = n.name
		}
	}

	if best == "" {
		return "", false
	}

	p.room[best]--

	return best, true
}

// better reports whether a should be chosen over b.
func (p *placer) better(a, b string) bool {
	if p.rank[a] != p.rank[b] {
		return p.rank[a] < p.rank[b]
	}

	if p.room[a] != p.room[b] {
		if p.policy == config.PlacementSpread {
			return p.room[a] > p.room[b]
		}

		// LESS ROOM WINS. Filling a machine keeps the others whole, so the fleet
		// can still hold a tier larger than what any fragment would fit.
		return p.room[a] < p.room[b]
	}

	return a < b
}
