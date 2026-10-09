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
	// Share is the part of the host's busier direction a direction must be
	// moving to be blamed. A round trip cannot say which direction's queue rose,
	// and a download filling the line drags its own acknowledgements upstream: an
	// upload of a few Mbit/s beside a download of hundreds did not fill anything,
	// and cutting it would slow the download's acknowledgements as well.
	Share float64
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
// high-load share of 0.75, a raise of 1.04), with the threshold and cooldown
// this host's tick and a residential line call for.
func DefaultParams() Params {
	return Params{
		Bloat:      15 * time.Millisecond,
		Severe:     60 * time.Millisecond,
		Window:     6,
		Detect:     3,
		GentleCut:  0.99,
		DeepCut:    0.75,
		Busy:       0.5,
		Share:      0.3,
		Cooldown:   time.Second,
		Full:       0.75,
		Raise:      1.04,
		PeakWindow: time.Minute,
	}
}

// Floor is the lowest rate either direction is ever cut to, in Mbit/s, so a
// misread delay can slow the host but never cut it off.
const Floor = 10.0

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
	sent := float64(o.SentBits) / seconds / 1e6
	received := float64(o.ReceivedBits) / seconds / 1e6
	busier := max(sent, received)

	up := c.step(&c.Up, sent, busier, full, cut, o)
	down := c.step(&c.Down, received, busier, full, cut, o)

	return up || down
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

func (c *Controller) step(d *Direction, moving, busier float64, full bool, cut float64, o Observation) bool {
	p := c.Params

	if moving >= d.peak || o.At.Sub(d.peakAt) > p.PeakWindow {
		d.peak, d.peakAt = moving, o.At
	}

	before := d.Rate

	switch {
	case full:
		// BLAMED ONLY WHEN BUSY. The queue is the line's, and a direction carrying
		// little of its recent peak, or little beside the other direction, is not
		// what filled it.
		if d.peak <= 0 || moving < p.Busy*d.peak || moving < p.Share*busier {
			return false
		}

		if d.cutSince && o.At.Sub(d.lastCut) < p.Cooldown {
			return false
		}

		// CUT BELOW WHAT IS FLOWING, not below the old rate: an unshaped direction
		// starts at the interface's speed, far above anything the line carries,
		// and only what is actually moving says where the line is.
		d.Rate = clamp(min(d.Rate, moving)*cut, d.Max)
		d.lastCut, d.cutSince = o.At, true
	case moving >= p.Full*d.Rate:
		// PROBED UPWARD ONLY WHILE THE RATE IS WHAT HOLDS THE TRAFFIC BACK and the
		// line shows no queue, so a rate climbs back toward the line's speed as
		// fast as the line allows and no faster.
		d.Rate = clamp(d.Rate*p.Raise+0.5, d.Max)
	}

	return d.Rate != before
}

func clamp(rate, maxMbit float64) float64 {
	return max(Floor, min(rate, maxMbit))
}
