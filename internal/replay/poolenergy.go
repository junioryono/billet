package replay

import (
	"cmp"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// PoolClearance is how long a pool host stays powered after its release is
// asked: the node's own clearance, two empty answers across five minutes, which
// the plane measures on the wall clock and a replay at compressed time does not
// wait for (PoolFleet). A replay that recorded a host's release uses that instead.
const PoolClearance = 5 * time.Minute

// PoolPower is what a replay's pool hosts were powered for over one window, in
// host-hours.
//
// A HOST IS POWERED FROM ITS JOIN TO ITS RELEASE, within the window: the release
// the replay recorded, or the ask plus PoolClearance, or, for a host never asked
// to leave, the window's end. Busy is the part of that in which the host carried
// at least one lease, and Idle the rest, which is the energy a release saves: a
// host carrying nothing still draws its idle power. MostAtOnce is the most hosts
// powered at one instant, by the same intervals.
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
	var steps []loadStep
	if s, e, ok := r.span(); ok {
		steps = r.steps(s, e)
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
		switch {
		case !o.Released.IsZero():
			off = o.Released
		case !o.ReleaseAsked.IsZero():
			off = o.ReleaseAsked.Add(PoolClearance)
		}
		on, off = later(on, from), earlier(off, to)
		if !off.After(on) {
			continue
		}
		out.Hosts++
		out.Powered += off.Sub(on).Hours()
		edges = append(edges, edge{on, 1}, edge{off, -1})

		name := alloc.PoolNodeName(o.Order)
		for _, s := range steps {
			a, b := later(s.at, on), earlier(s.nextFrom, off)
			if b.After(a) && s.perNode[name] > 0 {
				out.Busy += b.Sub(a).Hours()
			}
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
