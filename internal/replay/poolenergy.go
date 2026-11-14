package replay

import (
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// PoolClearance is how long a pool host stays powered after its release is
// asked: the node's own clearance, two empty answers across five minutes, which
// the plane measures on the wall clock and a replay at compressed time does not
// wait for (PoolFleet). A replay that recorded a host's release uses that instead.
const PoolClearance = 5 * time.Minute

// PoolPower is what a replay's pool hosts were powered for, in host-hours.
//
// A HOST IS POWERED FROM ITS JOIN TO ITS RELEASE: the release the replay recorded,
// or the ask plus PoolClearance, or, for a host never asked to leave, the end of the
// replay's span. Busy is the part of that in which the host carried at least one
// lease, and Idle the rest, which is the energy a release saves: a host carrying
// nothing still draws its idle power.
type PoolPower struct {
	Hosts       int
	Powered     float64
	Busy        float64
	Idle        float64
	SpanHours   float64
	MostAtOnce  int
	ReleasesAsk int
}

// PoolPowered measures the pool hosts' powered, busy and idle host-hours, each
// host from its join to its release. A replay with no pool, or no span, has none.
func (r *Report) PoolPowered() PoolPower {
	from, to, ok := r.span()
	if r.Pool == nil || !ok {
		return PoolPower{}
	}

	out := PoolPower{SpanHours: to.Sub(from).Hours(), ReleasesAsk: len(r.Pool.ReleasesAsked)}
	for _, a := range r.Pool.MostJoined {
		out.MostAtOnce += a
	}
	steps := r.steps(from, to)

	for i := range r.Pool.Ordered {
		o := &r.Pool.Ordered[i]
		if o.Joined.IsZero() {
			continue
		}
		// NOT CUT TO THE SPAN: a host joins before the first job it carries and
		// stays powered past the last, through idle_after and its clearance, which
		// is the energy the release is weighed against. Only a host never asked to
		// leave is cut, at the span's end, where the replay stops seeing it.
		on, off := o.Joined, to
		switch {
		case !o.Released.IsZero():
			off = o.Released
		case !o.ReleaseAsked.IsZero():
			off = o.ReleaseAsked.Add(PoolClearance)
		}
		if !off.After(on) {
			continue
		}
		out.Hosts++
		out.Powered += off.Sub(on).Hours()

		name := alloc.PoolNodeName(o.Order)
		for _, s := range steps {
			a, b := s.at, s.nextFrom
			if a.Before(on) {
				a = on
			}
			if b.After(off) {
				b = off
			}
			if b.After(a) && s.perNode[name] > 0 {
				out.Busy += b.Sub(a).Hours()
			}
		}
	}
	out.Idle = out.Powered - out.Busy

	return out
}
