package replay

import (
	"cmp"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// PoolClearance is how long a pool host stays powered after its release is
// asked: the node's own clearance, two empty answers across five minutes.
//
// THE MODEL'S, NEVER THE REPLAY'S: the plane times a clearance on the wall clock
// while a replay's ledger runs on simulated time, so the release a replay records
// lands wherever the plane's loop happened to run, and a host's powered time ends
// at its ask plus this instead.
const PoolClearance = 5 * time.Minute

// PoolPower is what a replay's pool hosts were powered for over one window, in
// host-hours.
//
// A HOST IS POWERED FROM ITS JOIN TO ITS RELEASE ASK PLUS PoolClearance, within the
// window, or to the window's end for a host never asked to leave. Busy is the part
// of that in which the host carried at least one lease, a lease the ledger never
// ended running to the window's end, and Idle the rest, which is the energy a
// release saves: a host carrying nothing still draws its idle power. MostAtOnce is
// the most hosts powered at one instant, by the same intervals.
type PoolPower struct {
	Hosts       int
	Powered     float64
	Busy        float64
	Idle        float64
	MostAtOnce  int
	ReleasesAsk int
}

// PoolPowered measures the pool hosts' powered, busy and idle host-hours within
// [from, to), a window the caller fixes so that replays of one trace under
// several pools are weighed over the same hours. A replay with no pool has none.
func (r *Report) PoolPowered(from, to time.Time) PoolPower {
	if r.Pool == nil || !to.After(from) {
		return PoolPower{}
	}

	out := PoolPower{ReleasesAsk: len(r.Pool.ReleasesAsked)}

	// Each host's leases, an open one running to the window's end.
	type span struct{ from, to time.Time }
	leases := map[string][]span{}
	for _, c := range r.charges() {
		end := c.to
		if c.open() {
			end = to
		}
		leases[c.node] = append(leases[c.node], span{c.from, end})
	}

	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge

	for i := range r.Pool.Ordered {
		o := &r.Pool.Ordered[i]
		if o.Joined.IsZero() {
			continue
		}
		on, off := o.Joined, to
		if !o.ReleaseAsked.IsZero() {
			off = o.ReleaseAsked.Add(PoolClearance)
		}
		on, off = later(on, from), earlier(off, to)
		if !off.After(on) {
			continue
		}
		out.Hosts++
		out.Powered += off.Sub(on).Hours()
		edges = append(edges, edge{on, 1}, edge{off, -1})

		// BUSY ONCE HOWEVER MANY LEASES OVERLAP: the host's leases, cut to its
		// powered time and merged.
		own := leases[alloc.PoolNodeName(o.Order)]
		slices.SortFunc(own, func(x, y span) int { return x.from.Compare(y.from) })
		var cur span
		flush := func() {
			if a, b := later(cur.from, on), earlier(cur.to, off); b.After(a) {
				out.Busy += b.Sub(a).Hours()
			}
		}
		for j, s := range own {
			switch {
			case j == 0:
				cur = s
			case !s.from.After(cur.to):
				cur.to = later(cur.to, s.to)
			default:
				flush()
				cur = s
			}
		}
		if len(own) > 0 {
			flush()
		}
	}
	out.Idle = out.Powered - out.Busy

	// A HOST LEAVING AT THE INSTANT ANOTHER JOINS is not two at once.
	slices.SortFunc(edges, func(x, y edge) int { return cmp.Or(x.at.Compare(y.at), cmp.Compare(x.delta, y.delta)) })
	now := 0
	for _, e := range edges {
		now += e.delta
		out.MostAtOnce = max(out.MostAtOnce, now)
	}

	return out
}

// later is the later of two instants.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}
