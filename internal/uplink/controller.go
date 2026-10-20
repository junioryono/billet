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
// rate per direction, while this starts each direction low, climbs while the
// line stays clear, and learns the line by cutting to below what was flowing
// when the queue rose or the line dropped. The delay that counts as a queue is
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
// unanswered, is never read as the reflectors' delay: it counts as neither
// delayed nor clear by them, so it never raises a rate and cuts only by the loss
// rule or a confirmed gateway stall. A round that counted no reflector at all is
// a could-not-tell, and neither cuts nor raises.
//
// The default route's gateway is asked each round too, against a baseline of
// its own, because a host beside the reference node saw its gateway stall (1.6
// ms to spikes of 128 ms) while the round trip to the reflectors rose about 11
// ms (2026-10-14). Its rise past Bloat confirmed by another high reading at
// most four rounds before it (readGateway), a reply slipped between them or
// not, though a quiet pair between ends the stall, makes a round delayed for a
// cut as the reflectors' does, so its stall cuts as their delay would, save a
// lone spike; with no reply read between them, the pair covers at most one
// earlier round too, the nearest that counted a reflector, its first reading's
// included, so a gateway answering one probe in three still cuts and three high
// readings in a row cut on the third; but never more, so two pings alone never
// do. One high reading keeps an episode from ending and a rate from rising, and
// a quiet reading clears a stall unless a high one stands just before it, one
// reply slipping through. All of it only in a round that counted a reflector,
// and while the gateway stalls a round it gave no reading in is not clear. A
// cut's effect is judged on two readings too, and on the reflectors' loss only
// when the line was dropping at the cut. Its unanswered probes are never loss,
// because a gateway answers ICMP when it has the time (the reference node's
// went unanswered about 2% of the time with the line quiet, 2026-10-16), and
// read as loss they would cut a busy host on every miss.
//
// A rate near the interface's speed is not a signal. The shaper cannot tell a
// line as fast as the interface, where such a rule would throttle a healthy host
// for good, from a slower one, where a gateway that drops shows as loss to the
// reflectors, which the loss rule reads. What a configured ceiling once stood in
// for is where the line was last found full, each direction's Limit: what it
// was moving at its last cut. A rate climbs back to near it quickly and past it
// only slowly, and near it or with none found only while CAKE is what holds the
// traffic back, so a gateway that drops before its round trip rises is overrun
// for the seconds the loss rule takes to see it, now and then, rather than at
// every climb; and a run with nothing learned starts low (Params.Start), never
// at the interface's speed, which on 2026-10-14 let a node send 888 Mbit/s into
// a 500 Mbit/s line. A ceiling (MaxUp, MaxDown) remains for an operator who
// wants one, and nothing needs it.
//
// And what a run learned outlives it: the rates CAKE was last set to, and each
// direction's Limit, are kept under the unit's state directory, and the next run
// starts from them as they were (learned.go), where every restart, and so every
// upgrade, used to start at the ceiling again.
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
	// delay of Bloat and of Severe and beyond; a cut on loss, and one after a cut
	// that did not shorten the queue, is DeepCut.
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
	// Raise is the factor a full direction's rate grows by each step, below
	// Near of the line it found.
	Raise float64
	// Start is the rate, in Mbit/s, a direction a run has learned nothing for
	// starts at, under its ceiling: low enough that a host never comes up
	// filling the line it shares, and climbed from at Raise.
	Start float64
	// Near is the share of a direction's Limit from which its rate is probed
	// upward slowly: ProbeRaise at most once every ProbeEvery.
	Near float64
	// ProbeFull is the share of the current rate a direction must be moving
	// for a raise where the line has not been found or is near: CAKE is then
	// what holds the traffic back, and a rate the line itself stops short of
	// is never raised further.
	ProbeFull float64
	// ProbeRaise and ProbeEvery are how a rate near or above its Limit grows:
	// ProbeRaise at least the two percent a rise must reach to be applied
	// (worthApplying), so what ProbeFull compares what flows with is what CAKE
	// holds, never a raise it was not given.
	ProbeRaise float64
	ProbeEvery time.Duration
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
		Start:      25,
		Near:       0.9,
		ProbeFull:  0.9,
		ProbeRaise: 1.02,
		ProbeEvery: 10 * time.Second,
		PeakWindow: time.Minute,
	}
}

// Floor is the lowest rate either direction is ever cut to, in Mbit/s, so a
// misread delay can slow the host but never cut it off.
const Floor = 10.0

// foundShare is the least share of its rate a direction must be moving at a
// cut for what it moves to be taken as the line: half, well under the three
// quarters a direction filling the line moves at the least (Full), so a line
// that has slowed under a high rate is still found, while one moving a little
// of a high rate is not.
const foundShare = 0.5

// ackShare is the least share of the other direction's throughput a direction
// must be moving at a cut for what it moves to be taken as the line. The
// acknowledgements a download draws are about 2 to 3% of it, and a cut leaves
// an upload carrying only them with a rate they fill, so no share of its own
// rate tells them from a full upload; an upload filling its own 50 Mbit/s
// beside a 600 Mbit/s download on a cable line is 8% of it. Under a twentieth
// (a 1000/35 line) nothing tells them apart beside a busy download, so such an
// upload's line is found while the download is quiet, and kept.
const ackShare = 0.05

// lossShare is the least share of its line a direction must be moving to be
// cut when the reflectors measured no rise in the round trip, on loss or a
// gateway stall: of its rate, or of the line it found when that is lower.
// Busy is judged against the direction's own last minute, which after a quiet
// minute is the trickle itself, so a loss somebody else caused cut a direction
// moving 19 Mbit/s under a 905 Mbit/s rate and a line found at 602, and
// another moving 18 under about 600, each to about 14 (#592): 3% of the line,
// which a tenth refuses with room to spare. A gateway's stall beside such a loss
// is no more the trickle's doing. A rise in the reflectors' round trip is cut
// for as before, by its own evidence, which also covers a line that queues.
//
// WHAT A TENTH GIVES UP, all on a line that drops rather than queues and stands
// under a tenth of the rate: one that shrank since the last climb, one under a
// rate that LAN traffic raised with no line found yet (the interface's counters
// and CAKE's ingress carry both), and traffic that overruns it in bursts too
// short to reach a tenth of the line over the two-second window, which no
// average of the counters tells from a trickle. Their loss cuts nothing until
// the rate comes down.
const lossShare = 0.1

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
	// Max is the rate the shaper never exceeds: the interface's own speed, at
	// which CAKE holds nothing back, or the ceiling configured below it.
	Max float64
	// Rate is the rate CAKE is set to now, in Mbit/s.
	Rate float64
	// Limit is what this direction was moving, in Mbit/s, when its last cut was
	// taken: the line as last found full, zero until a cut finds it.
	Limit float64

	peak      float64
	peakAt    time.Time
	lastCut   time.Time
	cutSince  bool
	lastRaise time.Time
	// episode is whether a cut has been taken since the queue last cleared;
	// cutReflectors, cutGateway and cutTally are that cut's evidence, which the
	// next cut in the same episode must improve on.
	episode       bool
	cutReflectors measured
	cutGateway    measured
	cutTally      tally
	futile        int
}

// measured is one source's delay at a cut, and whether that source measured
// it.
type measured struct {
	delay time.Duration
	known bool
}

// shorter is whether now is shorter than then by a margin, both measured: at
// least improvement, and at least one part in share of the delay then.
func (now measured) shorter(then measured, share time.Duration) bool {
	return now.known && then.known && now.delay <= then.delay-max(improvement, then.delay/share)
}

// The shares of a delay a cut must take off to have helped: a tenth of the
// reflectors' median, and a fifth of the gateway's, whose one ping a round
// wanders more (a steady 60 ms stall read 50 to 70 ms).
const (
	reflectorsShare = 10
	gatewayShare    = 5
)

// NewDirection starts a direction at its ceiling; a run starts it at
// Params.Start instead (newRunController).
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
	// Gateway is what the round's probe of the first hop said: its rise above
	// its own baseline when it answered. Its loss is never read, because many
	// gateways answer ICMP only when they have the time.
	Gateway Sample
}

// gatewayCounts reports whether the gateway's rise is evidence this round: it
// answered, and the round counted a reflector. A round that counted none is a
// could-not-tell that neither cuts nor raises, gateway or not, so a line whose
// far end has gone quiet is never shaped by its first hop alone.
func (o Observation) gatewayCounts() bool {
	return o.Gateway.DelayKnown && len(o.Probes) > 0
}

// gatewayUnmeasured reports whether a gateway being counted (it has a baseline
// and has not gone silent) gave no rise this round. While it stalls, such a
// round is not clear: a stalled gateway that misses a few probes has not
// cleared, and a round that cannot tell must neither end an episode nor raise a
// rate. One whose stall a quiet reading has cleared holds nothing up, so a
// gateway that answers only some of its probes slows no raise.
func (o Observation) gatewayUnmeasured() bool {
	return len(o.Gateway.Probes) > 0 && !o.Gateway.DelayKnown
}

// gatewayPairRounds is how many rounds apart two of the gateway's readings may
// be and still pair: four, two seconds at Tick, so a gateway that answers ICMP
// once a second, never two rounds in a row, still confirms a stall. Counted in
// rounds, not time: a round's time is taken once its probes are in, up to 400
// ms after its tick, so readings four rounds apart are 1.6 to 2.4 s apart.
const gatewayPairRounds = 4

// gatewayRound is what one round's gateway reading says, beside the one before.
type gatewayRound struct {
	// high is a counted reading over Bloat, and quiet one at or under it; a
	// round may be neither, when the gateway gave no counted reading.
	high, quiet bool
	// confirmed is a high reading with another high one at most
	// gatewayPairRounds before it, a reply slipped between them or not, though a
	// quiet pair between ends the stall: a stall, not one ping's spike. rise is
	// the lesser of the two, the stall both reached. spans is how many rounds
	// before this one the stall covers, the first reading's included, when the
	// gateway gave no counted reading between the two.
	confirmed bool
	rise      time.Duration
	spans     int
	// calm is a quiet reading whose last counted reading, at most
	// gatewayPairRounds before it, was quiet too, and calmRise the greater of
	// the two: the stall gone. slipped is a quiet one whose last, as near, was
	// high: one reply slipped through a stall, which has not cleared.
	calm     bool
	calmRise time.Duration
	slipped  bool
}

// reflectorsDelayed is whether the reflectors measured a delay past Bloat.
func (o Observation) reflectorsDelayed(bloat time.Duration) bool {
	return o.DelayKnown && o.Delay > bloat
}

// queue is the deepest queue the round measured, the reflectors' delay or the
// gateway's confirmed rise, and whether it measured either.
func (o Observation) queue(g gatewayRound) (time.Duration, bool) {
	delay, known := o.Delay, o.DelayKnown

	if g.confirmed && (!known || g.rise > delay) {
		delay, known = g.rise, true
	}

	return delay, known
}

// Controller decides both directions' rates.
type Controller struct {
	Params Params
	Up     Direction
	Down   Direction
	// Cause is what the last step's cut was taken on, CauseNone when it cut
	// nothing.
	Cause Cause
	// cutStall is the confirmed gateway stall the last step's cut was taken
	// beside, for its log; not known when there was none.
	cutStall measured

	// recent and losses are the rounds' delays and probes since the last cut,
	// which a cut is judged on; delayTrail and trail are the last Window
	// rounds', kept across cuts, which say whether the line has stopped
	// queueing and dropping. A cut forgets the evidence that caused it, but not
	// that the queue is still there: a round after it that forgot would read as
	// clear and end a futile episode.
	recent, delayTrail []bool
	losses, trail      [][]Probe
	// round counts the rounds stepped. lastGateway and lastHigh are the
	// gateway's last counted reading and its last one over Bloat since its last
	// quiet pair, with the rounds they were read in, which a reading now pairs
	// with; gatewayStalled is whether the last counted reading was over Bloat, or
	// quiet beside one that was, which decides what a round it gave no reading in
	// says.
	round                           int
	lastGateway, lastHigh           measured
	lastGatewayRound, lastHighRound int
	gatewayStalled                  bool
}

// readGateway judges this round's gateway reading against the last counted one.
func (c *Controller) readGateway(o Observation) gatewayRound {
	if !o.gatewayCounts() {
		return gatewayRound{}
	}

	bloat, reading := c.Params.Bloat, o.Gateway.Delay
	g := gatewayRound{high: reading > bloat, quiet: reading <= bloat}

	if g.high && c.lastHigh.known && c.round-c.lastHighRound <= gatewayPairRounds {
		g.confirmed, g.rise = true, min(reading, c.lastHigh.delay)

		// A REPLY BETWEEN SAYS WHAT THE ROUNDS BETWEEN WERE, so only a stall read
		// at both ends with nothing in the middle spans them, its first reading's
		// round included.
		if c.lastGatewayRound == c.lastHighRound {
			g.spans = c.round - c.lastHighRound
		}
	}

	if g.quiet && c.lastGateway.known && c.round-c.lastGatewayRound <= gatewayPairRounds {
		if last := c.lastGateway.delay; last <= bloat {
			g.calm, g.calmRise = true, max(reading, last)
		} else {
			g.slipped = true
		}
	}

	return g
}

// evidence is the gateway's reading as a cut keeps it and as helped judges it:
// a confirmed stall's lesser reading, or a quiet pair's greater one. One reading
// alone is evidence of neither, so a reply that slips through a stall does not
// read as the stall gone.
func (g gatewayRound) evidence() measured {
	switch {
	case g.confirmed:
		return measured{g.rise, true}
	case g.calm:
		return measured{g.calmRise, true}
	}

	return measured{}
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
	// tally is each reflector's probes and misses since the last cut, and
	// dropping whether they say the line is dropping.
	tally    tally
	dropping bool
	// gateway is what this round's gateway reading said.
	gateway gatewayRound
	cut     float64
}

// Step applies one observation and reports whether either rate changed.
func (c *Controller) Step(o Observation) bool {
	c.Cause, c.cutStall = CauseNone, measured{}

	if o.Interval <= 0 {
		return false
	}

	p := c.Params

	// EVERY ROUND AGES THE WINDOWS, one that measured nothing included, so what
	// was seen before an outage is not still evidence a minute later. A round
	// with no measured delay is not a delayed one, and one that counted no
	// reflector lost nothing.
	//
	// A CUT WAITS FOR A CONFIRMED GATEWAY STALL, AN EPISODE ENDS ONLY WITHOUT
	// ANY HIGH READING: the windows that decide a cut take the gateway's stall
	// once two readings confirm it, so one spike cuts nothing, while the trail
	// that decides whether a queue has gone takes every high reading, so a stall
	// with one quiet reply in four never reads as cleared.
	//
	// A CONFIRMED STALL COVERS ONE OF THE EARLIER ROUNDS IT SPANS, nearest first:
	// its first reading's, when the two are in a row, so a stall high one second
	// in two fills a window as the reflectors' delay in the same rounds would;
	// or the nearest it went unread in, so a stalled gateway that answers one
	// probe in three still does, rather than holding every rate with no cut ever
	// coming. Only the rounds that counted a reflector, as for any gateway
	// reading, and never so many that one pair alone fills the window: a gateway
	// that drops its replies as it delays them would otherwise cut a host on two
	// pings.
	c.round++
	g := c.readGateway(o)
	reflectors := o.reflectorsDelayed(p.Bloat)
	c.recent = lastRounds(append(c.recent, reflectors || g.confirmed), p.Window)
	c.delayTrail = lastRounds(append(c.delayTrail, reflectors || g.high), p.Window)
	c.losses = lastRounds(append(c.losses, o.Probes), p.Window)
	c.trail = lastRounds(append(c.trail, o.Probes), p.Window)

	if most := p.Detect - 2; g.spans > 0 && most > 0 {
		spanned(c.recent, c.losses, g.spans, most)
		spanned(c.delayTrail, c.trail, g.spans, most)
	}

	v := c.judge(o, g)

	if o.gatewayCounts() {
		c.lastGateway, c.lastGatewayRound = measured{o.Gateway.Delay, true}, c.round
		// A CALM PAIR IS THE STALL GONE, so a high reading after it is a fresh
		// one: spikes three rounds apart with quiet pairs between are not one
		// stall, while the rounds between them read clear.
		switch {
		case g.high:
			c.lastHigh, c.lastHighRound = c.lastGateway, c.round
		case g.calm:
			c.lastHigh = measured{}
		}

		c.gatewayStalled = g.high || g.slipped
	}

	seconds := o.Interval.Seconds()
	sent, received := float64(o.SentBits)/seconds/1e6, float64(o.ReceivedBits)/seconds/1e6
	upChanged, upCut := c.step(&c.Up, sent, received, v, o)
	downChanged, downCut := c.step(&c.Down, received, sent, v, o)

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

// spanned marks up to most of the n rounds before a window's last as delayed,
// nearest first and as many as it still holds, but only those whose probes, the
// same rounds counted from the end of a window kept beside it, counted a
// reflector.
func spanned(window []bool, probes [][]Probe, n, most int) {
	for k, marked := 1, 0; k <= n && marked < most; k++ {
		i, j := len(window)-1-k, len(probes)-1-k
		if i < 0 || j < 0 {
			return
		}

		if len(probes[j]) > 0 {
			window[i] = true
			marked++
		}
	}
}

func lastRounds[T any](rounds []T, n int) []T {
	if len(rounds) > n {
		return rounds[len(rounds)-n:]
	}

	return rounds
}

// judge reads the windows and this round into what both directions act on.
func (c *Controller) judge(o Observation, g gatewayRound) verdict {
	p := c.Params

	delayFull := countTrue(c.recent) >= p.Detect
	stillDelayed := countTrue(c.delayTrail) >= p.Detect
	dropping := c.dropping(c.losses, o)
	stillDropping := c.dropping(c.trail, o)
	reflectors := o.reflectorsDelayed(p.Bloat)
	stallUnread := o.gatewayUnmeasured() && c.gatewayStalled

	// CLEAR ONLY WHEN THE REFLECTORS MEASURED THE LINE UNDER BLOAT, and the
	// gateway shows no high reading, no quiet one slipped through a stall, and
	// did not go unread while stalled: a gateway alone says nothing about the
	// line beyond it, one high reading not yet confirmed is no evidence the queue
	// has gone, and one reply slipping through a stall is none that it has.
	v := verdict{
		full:     delayFull || dropping,
		onDelay:  delayFull && (reflectors || g.confirmed),
		onLoss:   dropping && o.Lost() > 0,
		clear:    o.DelayKnown && !reflectors && !g.high && !g.slipped && !stillDropping && !stallUnread,
		tally:    tallyOf(c.losses),
		dropping: dropping,
		gateway:  g,
		cut:      1,
	}
	v.cleared = v.clear && !stillDelayed

	if v.onDelay {
		queue, _ := o.queue(g)
		v.cut = c.cutFactor(queue)
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
// not a third of the verdict. The second answer is whether the tally could judge
// at all: a reflector asked in both, and a loss at the cut, as a delay is
// evidence of a cut only when the cut saw one; and a cut keeps a tally only from
// a window that was dropping. No loss then and none now is not a cut that
// failed.
func (t tally) lessLossThan(then tally, counted []Probe) (bool, bool) {
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

	return weight > 0 && nowLoss < thenLoss*0.9, weight > 0 && thenLoss > 0
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

// step moves one direction's rate, given what it and the other direction are
// moving, and reports whether it changed and whether that was a cut.
func (c *Controller) step(d *Direction, moving, other float64, v verdict, o Observation) (bool, bool) {
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

		// WITHOUT A RISE IN THE REFLECTORS' ROUND TRIP THAT THE WINDOW CONFIRMS
		// AND THIS ROUND MEASURED, A DIRECTION IS BLAMED ONLY WHILE IT FILLS A
		// SHARE OF ITS LINE (lossShare), for a loss or a gateway's stall alike:
		// one round over Bloat is not the queue Detect rounds are, and would
		// bring back the cut of a trickle beside a loss.
		if !v.onDelay || !o.reflectorsDelayed(p.Bloat) {
			line := d.Rate
			if d.Limit > 0 {
				line = min(line, d.Limit)
			}

			if moving < lossShare*line {
				return false, false
			}
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

		// A CUT WHOSE EFFECT COULD NOT BE JUDGED, no source measured both then and
		// now, still counts toward the latch, so sources that take turns cannot
		// cut a host to the floor; but it is not proof that a cut failed.
		failed := false
		if d.episode {
			shortened, judged := helped(d, v, o)
			failed = judged && !shortened

			if shortened {
				d.futile = 0
			} else {
				d.futile++
			}
		} else {
			d.futile = 0
		}

		if d.futile >= p.Futile {
			return false, false
		}

		// A CUT THAT FOLLOWS ONE PROVED NOT TO HAVE HELPED IS THE DEEPEST: a gentle
		// cut that left the queue alone may only have been too gentle for a queue
		// that is this host's, and the latch would leave it full for good.
		cut := v.cut
		if failed {
			cut = min(cut, p.DeepCut)
		}

		// CUT BELOW WHAT IS FLOWING, not below the old rate: a rate may stand far
		// above anything the line carries, and only what is actually moving says
		// where the line is.
		d.Rate = clamp(min(d.Rate, moving)*cut, d.Max)
		d.lastCut, d.cutSince, d.episode = o.At, true, true
		// WHAT FLOWED IS THE LINE, FOUND FULL: the rate climbs back to near it
		// quickly and past it only slowly, so a gateway that drops rather than
		// queues is overrun seldom and briefly rather than at every climb. BUT
		// ONLY WHERE THIS DIRECTION WAS FILLING IT, moving a share of what it was
		// let through and more than the other direction's acknowledgements: both
		// directions are cut on one verdict, and an upload carrying a download's,
		// 12 Mbit/s beside 510, says nothing of the line, where taking it as one
		// held the upload near 12 for many minutes after every busy download.
		if moving >= foundShare*before && moving >= ackShare*other {
			d.Limit = moving
		}
		// ONLY A SOURCE THAT SHOWED THE QUEUE IS EVIDENCE OF IT: a reflector delay
		// under Bloat that wanders from 12 ms to 8 ms says nothing of the gateway's
		// stall the cut was taken on, and crediting it would keep cutting for a
		// queue that is not this host's. Nor does a stray lost probe in a window
		// that was not dropping, which at the 1.7% a reflector loses on a quiet
		// line would credit about one cut in five that changed nothing.
		d.cutReflectors = measured{o.Delay, o.DelayKnown && o.Delay > p.Bloat}
		d.cutGateway = measured{v.gateway.rise, v.gateway.confirmed}
		c.cutStall = d.cutGateway
		d.cutTally = nil
		if v.dropping {
			d.cutTally = v.tally
		}

		return d.Rate != before, true
	case v.clear && d.futile < p.Futile && (!d.cutSince || o.At.Sub(d.lastCut) >= p.Cooldown):
		// PROBED UPWARD ONLY WHILE THE RATE IS WHAT HOLDS THE TRAFFIC BACK and this
		// round measured no queue, so a rate climbs back toward the line's speed as
		// fast as the line allows and no faster, and never on a round that could
		// not tell. NOT WITHIN THE COOLDOWN OF A CUT: the throughput window still
		// holds the traffic from before it, which runs above any rate just cut and
		// would undo the cut at once. NOR WHILE LATCHED: a raise could refill the
		// queue with this host's own traffic, and a latched direction could not be
		// cut for it; the latch lifts once the line has cleared.
		c.raise(d, moving, o.At)
	}

	return d.Rate != before, false
}

// raise grows a clear direction's rate by how near it is to the line it found.
//
// BELOW NEAR OF THE LIMIT, the line has carried more, so the rate climbs back at
// Raise each round, never past Near of it. FROM THERE ON, ProbeRaise at most
// once every ProbeEvery: a line that drops rather than queues shows no rise in
// the round trip before it is full, and is found only by overrunning it, so the
// rate creeps toward and past what it carried last, where a climb at Raise would
// overrun it at every recovery. WITH NO LIMIT, nothing found yet, at Raise from
// Start.
//
// AND NEAR THE LINE OR WITH NONE FOUND, ONLY WHILE CAKE IS WHAT HOLDS THE
// TRAFFIC BACK (ProbeFull): once the rate passes what the line itself carries,
// what flows stops following it, and the raise stops a ninth above the line
// rather than a third, as Full alone would let it.
func (c *Controller) raise(d *Direction, moving float64, at time.Time) {
	p := c.Params
	near := p.Near * d.Limit

	switch {
	case d.Limit > 0 && d.Rate < near:
		if moving < p.Full*d.Rate {
			return
		}

		d.Rate = clamp(min(d.Rate*p.Raise+0.5, near), d.Max)
	case d.Limit > 0:
		if moving < p.ProbeFull*d.Rate || at.Sub(d.lastRaise) < p.ProbeEvery {
			return
		}

		d.Rate = clamp(d.Rate*p.ProbeRaise+0.5, d.Max)
	default:
		if moving < p.ProbeFull*d.Rate {
			return
		}

		d.Rate = clamp(d.Rate*p.Raise+0.5, d.Max)
	}

	d.lastRaise = at
}

// resting is whether d's rate has come to rest where a climb stops: its
// ceiling, or Near of the line it found, where the fast climb ends exactly.
func (c *Controller) resting(d *Direction) bool {
	return d.Rate >= d.Max || d.Limit > 0 && d.Rate == c.Params.Near*d.Limit
}

// helped is whether the queue is shorter now than at the direction's last cut:
// a delay lower by a margin than the same source's delay then, the reflectors'
// against the reflectors' and the gateway's against the gateway's, or a share
// of the same reflectors' probes lost that is lower by a tenth; and whether
// any source measured both then and now, so could judge it at all.
//
// SOURCE AGAINST SOURCE: the gateway goes unanswered now and then, and a cut
// taken on its stall judged later against the reflectors' smaller delay would
// read as having helped, and the reverse as having failed.
//
// AND LIKE AGAINST LIKE, ON TWO READINGS: a cut keeps a confirmed stall's
// lesser reading, so the gateway now is judged by a pair too (evidence). A raw
// reading would judge a stall wandering 50 to 70 ms after a cut at 80 by its 70
// and latch a direction whose queue a cut had shortened, and one quiet reply
// slipping through a stall would credit a cut that changed nothing.
func helped(d *Direction, v verdict, o Observation) (bool, bool) {
	reflectors := measured{o.Delay, o.DelayKnown}
	gateway := v.gateway.evidence()
	lessLoss, lossJudged := v.tally.lessLossThan(d.cutTally, o.Probes)

	shortened := reflectors.shorter(d.cutReflectors, reflectorsShare) ||
		gateway.shorter(d.cutGateway, gatewayShare) || lessLoss
	judged := reflectors.known && d.cutReflectors.known ||
		gateway.known && d.cutGateway.known || lossJudged

	return shortened, judged
}

// forgetGateway drops what the controller learned from a gateway it no longer
// asks: the delay windows the old gateway filled and each direction's episode,
// whose futility latch it may have set. Rare, since a route seldom moves, and
// the safe way: a queue still there is seen again within Detect rounds.
func (c *Controller) forgetGateway() {
	c.recent, c.delayTrail, c.gatewayStalled = nil, nil, false
	c.lastGateway, c.lastHigh = measured{}, measured{}

	for _, d := range []*Direction{&c.Up, &c.Down} {
		d.episode, d.futile, d.cutGateway = false, 0, measured{}
	}
}

func clamp(rate, maxMbit float64) float64 {
	return max(Floor, min(rate, maxMbit))
}
