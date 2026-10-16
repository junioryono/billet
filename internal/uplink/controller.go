// Package uplink keeps a host's own traffic from filling the queue of the
// internet line it shares with the rest of its site.
//
// A fleet pulls images and caches in bursts at whatever the line carries, and a
// residential or office gateway holds one queue for everybody behind it: while
// the fleet fills it, a call stutters and a game lags (the shared-uplink record).
// The host cannot see the gateway's queue, but it can see what the queue does to
// a few public reflectors' answers. The controller here shapes the host's own
// egress and ingress with CAKE, and moves each rate from three observations: how
// far the round trip to the reflectors has risen above its idle baseline, how
// many of their answers the line lost, and how much the host is moving in that
// direction.
//
// The approach and most thresholds follow cake-autorate
// (github.com/lynxthecat/cake-autorate), which has tuned them on routers for
// years; no code is taken from it. Three things differ on purpose. Nothing about
// the line has to be configured: cake-autorate needs a minimum, base and maximum
// rate per direction, while this starts each direction at a ceiling (the
// interface's speed unless one is set) and learns the line by cutting to below
// what was flowing when the queue rose. The delay that counts as a queue is
// 15 ms, not 30, because a residential line measured 7 ms idle and 22 to 29 ms
// full: at 30 the lag a game felt would never have been seen.
//
// And loss is a signal of its own. cake-autorate detects bufferbloat from delay
// alone and treats a reflector's missing answers as the reflector misbehaving
// (an answer later than a second is an offence, three offences in a minute
// rotate the reflector out). That fits a gateway that queues. A gateway that
// drops rather than queues barely moves the round trip as the line fills, and
// loses packets instead: on one 500 Mbit/s site a host moving 378 Mbit/s down
// and 180 up gave every device there 2.5% loss, and on another day the round
// trip rose only about 11 ms while the host sent up to 888 Mbit/s and a laptop
// behind the same gateway lost its connections (the shared-uplink record).
// Delay-based transports answer loss as well as delay: LEDBAT (RFC 6817) backs
// off on loss as TCP does, and BBRv2 takes loss above 2% in a round trip as the
// line's limit. Three probes a round cannot resolve a rate like 2% in a few
// seconds, so the rule is stated in reflectors and rounds, chosen so one broken
// reflector and one stray drop are never enough: the line is dropping when at
// least LossRounds of the last Window rounds lost a probe, the reflectors that
// went unanswered in them are a strict majority of those counted, and this
// round lost one too. Simulated with three reflectors and independent losses,
// 10% probe loss is seen in about 5 s on average, 5% in about 13 s and 2.5% in
// about 40 s; a background loss of 0.1% sets it off by chance about once in
// five hours (0.5%, about every eleven minutes), and even then it cuts only a
// direction busy with traffic of its own. A loss cut is the deepest one,
// DeepCut, as TCP's halving and BBR's 0.7 are: a dropping line says the queue
// has already overflowed, and gives no measure of by how much.
//
// A round whose delay was not measured, because most of the reflectors went
// unanswered, is never read as a delay: it counts as neither delayed nor clear,
// so it never raises a rate and cuts only by the loss rule. A round that counted
// no reflector at all is a could-not-tell, and neither cuts nor raises.
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
	// the last Window rounds came back delayed, so one slow answer changes
	// nothing; a round with no measured delay is not a delayed one.
	Window, Detect int
	// LossRounds is how many of the last Window rounds must have lost a probe,
	// from a strict majority of the reflectors between them, for the line to be
	// taken as dropping: one round's blip, or one reflector that stopped
	// answering, is never enough.
	LossRounds int
	// GentleCut and DeepCut are the fractions a busy direction is cut to, at a
	// delay of Bloat and of Severe and beyond; a cut on loss is DeepCut.
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
	// Futile is how many cuts in a row may leave the delay and the loss no
	// lower before a direction stops being cut until the queue clears: a cut
	// that does not shorten the queue says the queue is not this host's.
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
// line calls for and the loss rule the package comment explains. The cooldown
// is longer than the two seconds throughput is measured over, so a cut is never
// judged on traffic from before it.
func DefaultParams() Params {
	return Params{
		Bloat:      15 * time.Millisecond,
		Severe:     60 * time.Millisecond,
		Window:     6,
		Detect:     3,
		LossRounds: 2,
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

// Cause is what a step's cut was taken on.
type Cause int

const (
	// CauseNone is a step that cut nothing.
	CauseNone Cause = iota
	// CauseDelay is a cut on a measured delay.
	CauseDelay
	// CauseLoss is a cut on probes the line lost.
	CauseLoss
	// CauseDelayAndLoss is a cut on both at once.
	CauseDelayAndLoss
)

func (c Cause) String() string {
	switch c {
	case CauseDelay:
		return "delay"
	case CauseLoss:
		return "loss"
	case CauseDelayAndLoss:
		return "delay and loss"
	}

	return "none"
}

// Direction is one direction's shaper.
type Direction struct {
	// Max is the rate the shaper starts at and never exceeds: the interface's
	// own speed, at which CAKE holds nothing back, or the ceiling configured
	// below it.
	Max float64
	// Rate is the rate CAKE is set to now, in Mbit/s.
	Rate float64

	peak     float64
	peakAt   time.Time
	lastCut  time.Time
	cutSince bool
	// episode is whether a cut has been taken since the queue last cleared;
	// cutDelay, cutDelayKnown and cutTally are that cut's evidence, which the
	// next cut in the same episode must improve on.
	episode       bool
	cutDelay      time.Duration
	cutDelayKnown bool
	cutTally      tally
	futile        int
}

// NewDirection starts a direction at its ceiling.
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
	// Sample is what the round's probes said. A delay that is not known counts
	// as neither delayed nor clear, and a round with no probes measured
	// nothing.
	Sample
}

// Controller decides both directions' rates.
type Controller struct {
	Params Params
	Up     Direction
	Down   Direction
	// Cause is what the last step's cut was taken on, CauseNone when it cut
	// nothing.
	Cause Cause

	// recent and losses are the rounds' delays and probes since the last cut,
	// which a cut is judged on; delayTrail and trail are the last Window
	// rounds', kept across cuts, which say whether the line has stopped
	// queueing and dropping. A cut forgets the evidence that caused it, but not
	// that the queue is still there: a round after it that forgot would read as
	// clear and end a futile episode.
	recent, delayTrail []bool
	losses, trail      [][]Probe
}

// verdict is what one step's evidence says, the same for both directions.
type verdict struct {
	// full is a window that says the line is full, by delay or by loss.
	full bool
	// onDelay and onLoss are a full window whose evidence is there in this
	// round too: a cut is taken only on a queue that is there now.
	onDelay, onLoss bool
	// clear is a round that measured the line and found no queue: a known delay
	// under Bloat, and a line that has not been dropping over the last Window
	// rounds, cuts or no cuts.
	clear bool
	// cleared is a clear round whose trail, kept across cuts, is not full of
	// delays either, which ends an episode of cuts. The windows a cut empties
	// are not enough: the round after a cut would find them empty with the
	// queue still there, and an episode that ended on that would never latch.
	cleared bool
	// tally is each reflector's probes and misses since the last cut.
	tally tally
	cut   float64
}

// Step applies one observation and reports whether either rate changed.
func (c *Controller) Step(o Observation) bool {
	c.Cause = CauseNone

	if o.Interval <= 0 {
		return false
	}

	p := c.Params

	// EVERY ROUND AGES THE WINDOWS, one that measured nothing included, so what
	// was seen before an outage is not still evidence a minute later. A round
	// with no measured delay is not a delayed one, and one that counted no
	// reflector lost nothing.
	delayed := o.DelayKnown && o.Delay > p.Bloat
	c.recent = lastRounds(append(c.recent, delayed), p.Window)
	c.delayTrail = lastRounds(append(c.delayTrail, delayed), p.Window)
	c.losses = lastRounds(append(c.losses, o.Probes), p.Window)
	c.trail = lastRounds(append(c.trail, o.Probes), p.Window)

	v := c.judge(o)

	seconds := o.Interval.Seconds()
	upChanged, upCut := c.step(&c.Up, float64(o.SentBits)/seconds/1e6, v, o)
	downChanged, downCut := c.step(&c.Down, float64(o.ReceivedBits)/seconds/1e6, v, o)

	// A CUT IS JUDGED ON FRESH EVIDENCE: the samples that caused it are
	// forgotten, so the next cut needs the windows to fill with delays and
	// losses seen after it.
	if upCut || downCut {
		c.recent, c.losses = nil, nil

		switch {
		case v.onDelay && v.onLoss:
			c.Cause = CauseDelayAndLoss
		case v.onLoss:
			c.Cause = CauseLoss
		default:
			c.Cause = CauseDelay
		}
	}

	return upChanged || downChanged
}

func countTrue(window []bool) int {
	n := 0

	for _, b := range window {
		if b {
			n++
		}
	}

	return n
}

func lastRounds[T any](rounds []T, n int) []T {
	if len(rounds) > n {
		return rounds[len(rounds)-n:]
	}

	return rounds
}

// judge reads the windows and this round into what both directions act on.
func (c *Controller) judge(o Observation) verdict {
	p := c.Params

	delayFull := countTrue(c.recent) >= p.Detect
	stillDelayed := countTrue(c.delayTrail) >= p.Detect
	dropping := c.dropping(c.losses, o)
	stillDropping := c.dropping(c.trail, o)
	delayedNow := o.DelayKnown && o.Delay > p.Bloat

	v := verdict{
		full:    delayFull || dropping,
		onDelay: delayFull && delayedNow,
		onLoss:  dropping && o.Lost() > 0,
		clear:   o.DelayKnown && !delayedNow && !stillDropping,
		tally:   tallyOf(c.losses),
		cut:     1,
	}
	v.cleared = v.clear && !stillDelayed

	if v.onDelay {
		v.cut = c.cutFactor(o.Delay)
	}

	if v.onLoss {
		v.cut = min(v.cut, p.DeepCut)
	}

	return v
}

// dropping reports whether a loss window says the line is dropping. Only the
// reflectors this round counts are read, so one that has since gone silent
// lends the window none of its misses. It needs both kinds of breadth: losses
// in at least LossRounds rounds, so one instant's blip is not a full line, and
// from a strict majority of the counted reflectors, so one reflector that
// stopped answering is not either.
func (c *Controller) dropping(window [][]Probe, o Observation) bool {
	counted := make(map[string]bool, len(o.Probes))
	for _, probe := range o.Probes {
		counted[probe.Reflector] = true
	}

	rounds := 0
	missed := map[string]bool{}

	for _, round := range window {
		lossy := false

		for _, probe := range round {
			if counted[probe.Reflector] && probe.Lost {
				lossy = true
				missed[probe.Reflector] = true
			}
		}

		if lossy {
			rounds++
		}
	}

	return rounds >= c.Params.LossRounds && 2*len(missed) > len(o.Probes)
}

// tally is, for each reflector, how many probes a window sent it and how many
// of them it lost.
type tally map[string]struct{ probes, lost int }

func tallyOf(window [][]Probe) tally {
	t := tally{}

	for _, round := range window {
		for _, probe := range round {
			n := t[probe.Reflector]
			n.probes++

			if probe.Lost {
				n.lost++
			}

			t[probe.Reflector] = n
		}
	}

	return t
}

// lessLossThan is whether this tally lost less than then, by a tenth: each
// reflector's own loss rate, over the reflectors this round counts that both
// of them asked, weighed in both by the fewer probes either window sent it. A
// reflector that joined or left in between, or was asked more often in one
// window than the other, changes a pooled share without the line changing at
// all; and one asked once in either window is one probe's worth of evidence,
// not a third of the verdict.
func (t tally) lessLossThan(then tally, counted []Probe) bool {
	var nowLoss, thenLoss, weight float64

	for _, probe := range counted {
		now, ok := t[probe.Reflector]
		before, was := then[probe.Reflector]

		w := float64(min(now.probes, before.probes))
		if !ok || !was || w == 0 {
			continue
		}

		weight += w
		nowLoss += w * float64(now.lost) / float64(now.probes)
		thenLoss += w * float64(before.lost) / float64(before.probes)
	}

	return weight > 0 && nowLoss < thenLoss*0.9
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
func (c *Controller) step(d *Direction, moving float64, v verdict, o Observation) (bool, bool) {
	p := c.Params

	if moving >= d.peak || o.At.Sub(d.peakAt) > p.PeakWindow {
		d.peak, d.peakAt = moving, o.At
	}

	before := d.Rate

	// THE QUEUE CLEARED, by the trails a cut does not empty: a later one starts
	// a new episode, and its cuts are judged afresh, against nothing from this
	// one. Not within the cooldown of this direction's last cut, while the
	// queue that cut is meant to drain still has the time to show it has not:
	// the trail can thin out under a queue that never left. The trails hold
	// every round the windows do, so a cleared line is never a full one, and
	// this never meets a cut below.
	if v.cleared && (!d.cutSince || o.At.Sub(d.lastCut) >= p.Cooldown) {
		d.futile, d.episode = 0, false
	}

	switch {
	case v.full:
		// BLAMED ONLY WHEN BUSY. The queue is the line's, and a direction carrying
		// a trickle, or little of its own recent peak, is not what filled it.
		if moving < p.MinBlame || moving < p.Busy*d.peak {
			return false, false
		}

		// CUT ONLY ON A QUEUE THAT IS THERE NOW: a window full of delays or losses
		// the queue has since drained says nothing about the next second, and a
		// round that measured nothing says nothing at all.
		if !v.onDelay && !v.onLoss {
			return false, false
		}

		if d.cutSince && o.At.Sub(d.lastCut) < p.Cooldown {
			return false, false
		}

		// A CUT THAT DID NOT SHORTEN THE QUEUE IS NOT REPEATED FOREVER: the queue
		// is somebody else's, and cutting on would only take this host to the
		// floor. Shorter means by a margin, because each reflector's baseline
		// creeps toward a round trip held above it, so an unchanged queue reads a
		// little lower every sample; and a delay is compared only with a delay
		// that was measured. And once futile, LATCHED until a clear sample ends
		// the episode: over a minute the creep alone crosses any margin.
		if d.futile >= p.Futile {
			return false, false
		}

		if d.episode && !helped(d, v, o) {
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
		d.Rate = clamp(min(d.Rate, moving)*v.cut, d.Max)
		d.lastCut, d.cutSince, d.episode = o.At, true, true
		d.cutDelay, d.cutDelayKnown, d.cutTally = o.Delay, o.DelayKnown, v.tally

		return d.Rate != before, true
	case v.clear && moving >= p.Full*d.Rate && d.futile < p.Futile &&
		(!d.cutSince || o.At.Sub(d.lastCut) >= p.Cooldown):
		// PROBED UPWARD ONLY WHILE THE RATE IS WHAT HOLDS THE TRAFFIC BACK and this
		// round measured no queue, so a rate climbs back toward the line's speed as
		// fast as the line allows and no faster, and never on a round that could
		// not tell. NOT WITHIN THE COOLDOWN OF A CUT: the throughput window still
		// holds the traffic from before it, which runs above any rate just cut and
		// would undo the cut at once. NOR WHILE LATCHED: a raise could refill the
		// queue with this host's own traffic, and a latched direction could not be
		// cut for it; the latch lifts once the line has cleared.
		d.Rate = clamp(d.Rate*p.Raise+0.5, d.Max)
	}

	return d.Rate != before, false
}

// helped is whether the queue is shorter now than at the direction's last cut:
// a measured delay lower by a margin than the measured delay then, or a share
// of the same reflectors' probes lost that is lower by a tenth.
func helped(d *Direction, v verdict, o Observation) bool {
	if o.DelayKnown && d.cutDelayKnown && o.Delay <= d.cutDelay-max(improvement, d.cutDelay/10) {
		return true
	}

	return v.tally.lessLossThan(d.cutTally, o.Probes)
}

func clamp(rate, maxMbit float64) float64 {
	return max(Floor, min(rate, maxMbit))
}
