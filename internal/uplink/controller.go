// Package uplink keeps a host's own traffic from filling the queue of the
// internet line it shares with the rest of its site.
//
// A fleet pulls images and caches in bursts at whatever the line carries, and a
// residential or office gateway holds one queue for everybody behind it: while
// the fleet fills it, a call stutters and a game lags (the shared-uplink record).
// The host cannot see the gateway's queue, but it can see what the queue does to
// latency. The controller here shapes the host's own egress and ingress with
// CAKE, and moves each rate from two observations: how far the round trip to a
// few public reflectors has risen above its idle baseline, and how much the host
// is moving in that direction.
//
// The approach and most thresholds follow cake-autorate
// (github.com/lynxthecat/cake-autorate), which has tuned them on routers for
// years; no code is taken from it. Two things differ on purpose. Nothing about
// the line is configured: cake-autorate needs a minimum, base and maximum rate
// per direction, while this starts each direction unshaped at the interface's
// speed and learns the line by cutting to below what was flowing when the
// queue rose. And the delay that counts as a queue is 15 ms, not 30, because a
// residential line measured 7 ms idle and 22 to 29 ms full: at 30 the lag a game
// felt would never have been seen.
package uplink

import "time"

// Params are the controller's thresholds. The zero value is not usable; start
// from DefaultParams.
type Params struct {
	// Bloat is how far above its baseline the round trip must rise for a sample
	// to count as delayed.
	Bloat time.Duration
	// Severe is the delay at which a cut is deepest; between Bloat and Severe
	// the cut grows from GentleCut to DeepCut.
	Severe time.Duration
	// Window and Detect: the queue is taken to be full when at least Detect of
	// the last Window samples were delayed, so one slow answer changes nothing.
	Window, Detect int
	// GentleCut and DeepCut are the fractions a busy direction is cut to, at a
	// delay of Bloat and of Severe and beyond.
	GentleCut, DeepCut float64
	// Busy is the share of a direction's recent peak it must be moving to be
	// blamed for a full queue: a delay that rises while the host is quiet is
	// somebody else's traffic, and cutting the host's rate would help nobody.
	Busy float64
	// MinBlame is the least a direction must be moving, in Mbit/s, to be blamed
	// at all. A host carrying a trickle beside somebody else's stream is its own
	// recent peak, and without a floor on the blame it would be cut for a queue
	// it did not fill; nothing is cut below Floor, so a direction moving less
	// than that could gain nothing from a cut, while one just above it, a
	// saturated upload on a slow line, still can.
	MinBlame float64
	// Futile is how many cuts in a row may leave the delay no lower before a
	// direction stops being cut until the queue clears: a cut that does not
	// shorten the queue says the queue is not this host's.
	Futile int
	// Cooldown is the least time between two cuts in one direction, so the
	// queue a cut is meant to drain has the chance to drain before the next.
	Cooldown time.Duration
	// Full is the share of the current rate a direction must be moving, with no
	// full queue, before its rate is raised: a rate is only probed upward while
	// it is what holds the traffic back.
	Full float64
	// Raise is the factor a full direction's rate grows by each step.
	Raise float64
	// PeakWindow is how long a direction's peak throughput is remembered.
	PeakWindow time.Duration
}

// DefaultParams are cake-autorate's defaults where they carry over (a window
// of 6 with 3 delayed, cuts from 0.99 to 0.75 reaching the deepest at 60 ms, a
// high-load share of 0.75, a raise of 1.04), with the threshold a residential
// line calls for. The cooldown is longer than the two seconds throughput is
// measured over, so a cut is never judged on traffic from before it.
func DefaultParams() Params {
	return Params{
		Bloat:      15 * time.Millisecond,
		Severe:     60 * time.Millisecond,
		Window:     6,
		Detect:     3,
		GentleCut:  0.99,
		DeepCut:    0.75,
		Busy:       0.5,
		MinBlame:   Floor,
		Futile:     2,
		Cooldown:   3 * time.Second,
		Full:       0.75,
		Raise:      1.04,
		PeakWindow: time.Minute,
	}
}

// Floor is the lowest rate either direction is ever cut to, in Mbit/s, so a
// misread delay can slow the host but never cut it off.
const Floor = 10.0

// improvement is the least a cut must shorten the queue by to count as having
// helped, when a tenth of the delay is less.
const improvement = 2 * time.Millisecond

// Direction is one direction's shaper.
type Direction struct {
	// Max is the rate the shaper starts at and never exceeds: the interface's
	// own speed, at which CAKE holds nothing back.
	Max float64
	// Rate is the rate CAKE is set to now, in Mbit/s.
	Rate float64

	peak     float64
	peakAt   time.Time
	lastCut  time.Time
	cutSince bool
	cutDelay time.Duration
	futile   int
}

// NewDirection starts a direction unshaped, at the interface's own speed.
func NewDirection(maxMbit float64) Direction {
	if maxMbit < Floor {
		maxMbit = Floor
	}

	return Direction{Max: maxMbit, Rate: maxMbit}
}

// Observation is what one step saw.
type Observation struct {
	At time.Time
	// Interval is how long the byte counts below cover.
	Interval time.Duration
	// SentBits and ReceivedBits are what the interface moved in Interval.
	SentBits, ReceivedBits uint64
	// Delay is how far the round trip has risen above its baseline, and
	// DelayKnown whether the reflectors said anything at all: an unknown delay
	// counts as neither delayed nor clear.
	Delay      time.Duration
	DelayKnown bool
}

// Controller decides both directions' rates.
type Controller struct {
	Params Params
	Up     Direction
	Down   Direction

	recent []bool
}

// Step applies one observation and reports whether either rate changed.
func (c *Controller) Step(o Observation) bool {
	if o.Interval <= 0 {
		return false
	}

	if o.DelayKnown {
		c.recent = append(c.recent, o.Delay > c.Params.Bloat)
		if len(c.recent) > c.Params.Window {
			c.recent = c.recent[len(c.recent)-c.Params.Window:]
		}
	}

	delayed := 0
	for _, d := range c.recent {
		if d {
			delayed++
		}
	}

	full := delayed >= c.Params.Detect
	cut := c.cutFactor(o.Delay)

	seconds := o.Interval.Seconds()
	upChanged, upCut := c.step(&c.Up, float64(o.SentBits)/seconds/1e6, full, cut, o)
	downChanged, downCut := c.step(&c.Down, float64(o.ReceivedBits)/seconds/1e6, full, cut, o)

	// A CUT IS JUDGED ON FRESH EVIDENCE: the samples that caused it are
	// forgotten, so the next cut needs the window to fill with delays seen after
	// it.
	if upCut || downCut {
		c.recent = nil
	}

	return upChanged || downChanged
}

// cutFactor grows the cut with the delay, from GentleCut at Bloat to DeepCut
// at Severe, so a queue that is barely full is trimmed and one that is badly
// full is emptied.
func (c *Controller) cutFactor(delay time.Duration) float64 {
	p := c.Params
	span := float64(p.Severe - p.Bloat)
	share := 1.0

	if span > 0 {
		share = min(1, max(0, float64(delay-p.Bloat)/span))
	}

	return p.GentleCut - (p.GentleCut-p.DeepCut)*share
}

// step moves one direction's rate, and reports whether it changed and whether
// that was a cut.
func (c *Controller) step(d *Direction, moving float64, full bool, cut float64, o Observation) (bool, bool) {
	p := c.Params

	if moving >= d.peak || o.At.Sub(d.peakAt) > p.PeakWindow {
		d.peak, d.peakAt = moving, o.At
	}

	before := d.Rate

	switch {
	case full:
		// BLAMED ONLY WHEN BUSY. The queue is the line's, and a direction carrying
		// a trickle, or little of its own recent peak, is not what filled it.
		if moving < p.MinBlame || moving < p.Busy*d.peak {
			return false, false
		}

		// CUT ONLY ON A QUEUE THAT IS THERE NOW: a window full of delays the queue
		// has since drained says nothing about the next second.
		if !(o.DelayKnown && o.Delay > p.Bloat) {
			return false, false
		}

		if d.cutSince && o.At.Sub(d.lastCut) < p.Cooldown {
			return false, false
		}

		// A CUT THAT DID NOT SHORTEN THE QUEUE IS NOT REPEATED FOREVER: the queue
		// is somebody else's, and cutting on would only take this host to the
		// floor. Shorter means by a margin, because each reflector's baseline
		// creeps toward a round trip held above it, so an unchanged queue reads a
		// little lower every sample.
		if d.cutSince && o.Delay > d.cutDelay-max(improvement, d.cutDelay/10) {
			d.futile++
		} else {
			d.futile = 0
		}

		if d.futile >= p.Futile {
			return false, false
		}

		// CUT BELOW WHAT IS FLOWING, not below the old rate: an unshaped direction
		// starts at the interface's speed, far above anything the line carries,
		// and only what is actually moving says where the line is.
		d.Rate = clamp(min(d.Rate, moving)*cut, d.Max)
		d.lastCut, d.cutSince, d.cutDelay = o.At, true, o.Delay

		return d.Rate != before, true
	case moving >= p.Full*d.Rate && !(o.DelayKnown && o.Delay > p.Bloat) &&
		(!d.cutSince || o.At.Sub(d.lastCut) >= p.Cooldown):
		// PROBED UPWARD ONLY WHILE THE RATE IS WHAT HOLDS THE TRAFFIC BACK and this
		// sample shows no queue, so a rate climbs back toward the line's speed as
		// fast as the line allows and no faster. NOT WITHIN THE COOLDOWN OF A CUT:
		// the throughput window still holds the traffic from before it, which runs
		// above any rate just cut and would undo the cut at once.
		d.Rate = clamp(d.Rate*p.Raise+0.5, d.Max)
		d.futile = 0
	case !(o.DelayKnown && o.Delay > p.Bloat):
		// THE QUEUE CLEARED: a later one starts a new episode, and cuts are judged
		// afresh. Not merely "not full": the window refills after every cut, and
		// forgetting futility while it does would let cuts ratchet on regardless.
		d.futile = 0
	}

	return d.Rate != before, false
}

func clamp(rate, maxMbit float64) float64 {
	return max(Floor, min(rate, maxMbit))
}
