package pool

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// shrinkIdleAfter is the idle_after every shrink test's pool is given.
const shrinkIdleAfter = 20 * time.Minute

// shrinkRig is a rig whose pool shrinks itself after shrinkIdleAfter, its first source
// confirming gone, with a joined x64 host per entry of ages idle that long, seeded in
// the order of their names, and the one Firecracker tier reporting no demand: its count
// in hand and nothing unstarted.
func shrinkRig(t *testing.T, policy Policy, ages map[string]time.Duration) *rig {
	t.Helper()
	r := newRig(t, policy, firecrackerTier("fc"))
	r.pool.cfg.IdleAfter = shrinkIdleAfter
	r.first.confirmsGone = true
	r.ledger.idleSince = map[string]time.Time{}
	ids := make([]string, 0, len(ages))
	for id := range ages {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		r.joined(id, "host-"+id, false)
		r.ledger.idleSince[id] = r.now.Add(-ages[id])
	}
	r.reportQuiet("fc", 0)

	return r
}

// reportQuiet sets a tier's fresh report with its unstarted work counted.
func (r *rig) reportQuiet(label string, unstarted int) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.reports[label] = alloc.TierCapacity{
		ObservedAt: r.now.UTC().Format(time.RFC3339Nano),
		Listener:   alloc.ListenerCapacity{Unstarted: &unstarted},
	}
}

// asked is the orders the pool asked to release, in the order it asked.
func asked(r *rig) []string {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	var out []string
	for _, a := range r.ledger.idleReleases {
		out = append(out, a.order)
	}

	return out
}

// THE POOL SHRINKS BY DRAIN, ONE HOST A STEP, THE LONGEST IDLE FIRST, AND NEVER BELOW ITS
// FLOOR: a host idle past idle_after on an architecture above its floor, with no demand
// and nothing on its way, has its release asked for, with the floor the allocator checks
// again; the next step counts it leaving and reads the floor again.
func TestThePoolShrinksTheLongestIdleHostToItsFloor(t *testing.T) {
	t.Parallel()
	r := shrinkRig(t, Policy{Floor: 1, Max: 4},
		map[string]time.Duration{"o-a": 30 * time.Minute, "o-b": time.Hour, "o-c": 45 * time.Minute})

	step(t, r)
	if got := asked(r); !slices.Equal(got, []string{"o-b"}) {
		t.Fatalf("asked %v after one step, want only the longest idle, o-b", got)
	}
	first := r.ledger.idleReleases[0]
	if !first.cutoff.Equal(r.now.Add(-20*time.Minute)) || first.floor != 1 ||
		!strings.Contains(first.reason, "idle since") || !strings.Contains(first.reason, "idle_after 20m0s") {
		t.Fatalf("asked %+v, want the idle_after cutoff, the floor and a reason saying why", first)
	}

	step(t, r)
	if got := asked(r); !slices.Equal(got, []string{"o-b", "o-c"}) {
		t.Fatalf("asked %v after two steps, want o-c next", got)
	}
	// AT THE FLOOR: one host serves, and it stays however idle it is.
	step(t, r)
	step(t, r)
	if got := asked(r); len(got) != 2 {
		t.Fatalf("asked %v, below the floor of 1", got)
	}
}

// NOTHING SHRINKS that the rule does not allow, and the scaler does not even ask: a pool
// told no idle_after; a host idle less than idle_after; demand on its architecture, or a
// count of it the listener does not have, or work unstarted; demand that could not be
// read; an order on its way or unknown; a held order standing where a host would; a host
// a tier pins by name; and a host whose source does not confirm gone.
func TestThePoolKeepsItsHostsWhenItMayNotShrink(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		floor  int
		setup  func(r *rig)
		unread bool
	}{
		"no idle_after":       {setup: func(r *rig) { r.pool.cfg.IdleAfter = 0 }},
		"not idle long":       {setup: func(r *rig) { r.pool.cfg.IdleAfter = 2 * time.Hour }},
		"a shortage":          {setup: func(r *rig) { r.report("fc", true, 0, "") }},
		"a count not in hand": {setup: func(r *rig) { r.report("fc", false, 0, "") }},
		"work unstarted":      {setup: func(r *rig) { r.reportQuiet("fc", 1) }},
		"demand unreadable":   {setup: func(r *rig) { r.report("fc", false, time.Hour, "") }, unread: true},
		"a demand read error": {setup: func(r *rig) { r.report("fc", false, 0, "the listener could not say") },
			unread: true},
		"an order on its way": {setup: func(r *rig) { r.place("o-new", alloc.PoolPlaced, "") }},
		"an order unknown":    {setup: func(r *rig) { r.place("o-unk", alloc.PoolUncertain, "") }},
		"a held order in the floor's place": {floor: 1, setup: func(r *rig) {
			r.ledger.mu.Lock()
			defer r.ledger.mu.Unlock()
			delete(r.ledger.idleSince, "o-b")
			r.ledger.orders = slices.DeleteFunc(r.ledger.orders, func(o alloc.PoolOrder) bool { return o.ID == "o-b" })
			r.ledger.orders = append(r.ledger.orders, alloc.PoolOrder{ID: "o-held", Source: "first",
				Scope: "first:us-west-2", Shape: "z1d.metal", Arch: config.ArchX64, State: alloc.PoolPlaced,
				HostID: "host-held", JoinHold: "the source contradicted the join", FirstAttempt: r.now, Updated: r.now})
		}},
		"a host a tier pins": {setup: func(r *rig) {
			pin := firecrackerTier("pinned")
			pin.Node = alloc.PoolNodeName("o-a")
			pinB := firecrackerTier("pinned-b")
			pinB.Node = alloc.PoolNodeName("o-b")
			r.pool.cfg.Tiers = append(r.pool.cfg.Tiers, pin, pinB)
		}},
		"a source that never says gone": {setup: func(r *rig) { r.first.confirmsGone = false }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := shrinkRig(t, Policy{Floor: c.floor, Max: 4},
				map[string]time.Duration{"o-a": time.Hour, "o-b": time.Hour})
			c.setup(r)
			// DEMAND THAT COULD NOT BE READ IS THE STEP'S ERROR, and the only one here.
			if err := r.pool.Step(t.Context()); errors.Is(err, ErrDemandUnknown) != c.unread {
				t.Fatalf("step answered %v; want demand unknown: %t", err, c.unread)
			}
			if got := asked(r); len(got) != 0 {
				t.Fatalf("asked for %v", got)
			}
		})
	}
}

// place seeds an order in a state, from the first source, holding a node name or none.
func (r *rig) place(id, state, node string) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.orders = append(r.ledger.orders, alloc.PoolOrder{ID: id, Source: "first", Scope: "first:us-west-2",
		Shape: "z1d.metal", Arch: config.ArchX64, State: state, HostID: "host-" + id, NodeName: node,
		FirstAttempt: r.now, Updated: r.now})
}

// ONE ARCHITECTURE'S SHRINK NEVER TAKES ANOTHER'S HOST: an arm64 host idle longer than
// any x64 one, on an architecture at its floor, stays while x64 shrinks.
func TestAShrinkTakesOnlyItsOwnArchitecturesHost(t *testing.T) {
	t.Parallel()
	r := shrinkRig(t, Policy{Floor: 0, Max: 4}, map[string]time.Duration{"o-x": 30 * time.Minute})
	r.pool.cfg.Archs[config.ArchARM64] = Policy{Floor: 1, Max: 4, Choices: r.pool.cfg.Archs[config.ArchX64].Choices}
	r.ledger.mu.Lock()
	r.ledger.orders = append(r.ledger.orders, alloc.PoolOrder{ID: "o-arm", Source: "first", Scope: "first:us-west-2",
		Shape: "z1d.metal", Arch: config.ArchARM64, State: alloc.PoolPlaced, HostID: "host-arm",
		NodeName: alloc.PoolNodeName("o-arm"), FirstAttempt: r.now, Updated: r.now})
	r.ledger.idleSince["o-arm"] = r.now.Add(-3 * time.Hour)
	r.ledger.mu.Unlock()

	step(t, r)
	if got := asked(r); !slices.Equal(got, []string{"o-x"}) {
		t.Fatalf("asked %v, want only the x64 host", got)
	}
}

// THE ALLOCATOR'S REFUSALS LEAVE THE HOST AND THE STEP GOES ON: a host that took work
// between the reading and the asking, or a person's release that left the floor needing
// it, is asked once, stays, and fails nothing.
func TestARefusedIdleReleaseLeavesTheHost(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"took work":    alloc.ErrPoolHostNotIdle,
		"at the floor": alloc.ErrPoolAtFloor,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := shrinkRig(t, Policy{Floor: 0, Max: 4}, map[string]time.Duration{"o-a": time.Hour})
			r.ledger.idleAnswer = map[string]error{"o-a": refusal}
			step(t, r)
			if got := asked(r); !slices.Equal(got, []string{"o-a"}) {
				t.Fatalf("asked %v, want o-a once", got)
			}
			if o := r.ledger.snapshot()["o-a"]; !o.ReleaseRequested.IsZero() {
				t.Fatal("a refused release was recorded")
			}
		})
	}
}

// WHAT THE SHRINK COULD NOT DO IS THE STEP'S ERROR: an idle listing that failed, and a
// release that failed for any reason but the allocator's refusals.
func TestAShrinkThatFailedSaysSo(t *testing.T) {
	t.Parallel()
	listing := errors.New("the ledger could not list")
	r := shrinkRig(t, Policy{Floor: 0, Max: 4}, map[string]time.Duration{"o-a": time.Hour})
	r.ledger.idleErr = listing
	if err := r.pool.Step(t.Context()); !errors.Is(err, listing) {
		t.Fatalf("a failed idle listing answered %v", err)
	}

	asking := errors.New("the ledger could not write")
	r = shrinkRig(t, Policy{Floor: 0, Max: 4}, map[string]time.Duration{"o-a": time.Hour})
	r.ledger.idleAnswer = map[string]error{"o-a": asking}
	if err := r.pool.Step(t.Context()); !errors.Is(err, asking) {
		t.Fatalf("a failed release answered %v", err)
	}
}
