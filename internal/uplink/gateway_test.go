package uplink

import (
	"strings"
	"testing"
	"time"
)

// gatewayRose is a gateway that answered, risen delay above its baseline.
func gatewayRose(delay time.Duration) Sample {
	return Sample{Delay: delay, DelayKnown: true, Probes: []Probe{{Reflector: "192.168.1.254"}}}
}

// gatewayLost is a gateway that did not answer this round.
func gatewayLost() Sample {
	return Sample{Probes: []Probe{{Reflector: "192.168.1.254", Lost: true}}}
}

// withGateway is an observation with what the gateway's probe said beside it.
func withGateway(o Observation, gateway Sample) Observation {
	o.Gateway = gateway

	return o
}

// A GATEWAY THAT STALLS UNDER THIS HOST'S TRAFFIC IS A QUEUE, though the
// reflectors beyond it see little: a laptop beside the reference node saw its
// gateway go from 1.6 ms to spikes of 128 ms while the round trip to 1.1.1.1
// rose about 11 ms (2026-10-14).
func TestAGatewayThatStallsCutsABusyDirection(t *testing.T) {
	t.Parallel()

	c := newController()

	cut := false
	for n := range 6 {
		cut = c.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(40*time.Millisecond))) || cut
	}

	if !cut || c.Down.Rate >= 400 {
		t.Fatalf("a gateway risen 40 ms in every round under a 400 Mbit/s download cut nothing (down %.1f)",
			c.Down.Rate)
	}
	if c.Cause != CauseNone && c.Cause != CauseDelay {
		t.Errorf("the cut was taken on %s, want delay", c.Cause)
	}

	// WITHOUT THE GATEWAY THE SAME ROUNDS ARE CLEAR.
	reflectorsOnly := newController()
	for n := range 6 {
		if reflectorsOnly.Step(moving(n, 5, 400, 2*time.Millisecond)) && reflectorsOnly.Down.Rate < reflectorsOnly.Down.Max {
			t.Fatalf("round %d cut on reflectors 2 ms above their baseline", n)
		}
	}
}

// THE DEEPER QUEUE DECIDES HOW DEEP THE CUT: a gateway risen past Severe cuts as
// deep as the rule goes, though the reflectors' delay alone would trim.
func TestTheDeeperOfTheTwoDelaysSetsTheCut(t *testing.T) {
	t.Parallel()

	c := newController()
	for n := range 3 {
		c.Step(withGateway(moving(n, 5, 400, 20*time.Millisecond), gatewayRose(80*time.Millisecond)))
	}

	if want := 400 * c.Params.DeepCut; c.Down.Rate > want+0.01 {
		t.Errorf("the download was cut to %.1f, want the deepest cut, %.1f", c.Down.Rate, want)
	}
}

// A STALLED GATEWAY HOLDS A RATE THAT WOULD OTHERWISE CLIMB, as a delayed round
// does: the line beyond it measured clear says nothing of the first hop.
func TestAStalledGatewayHoldsARate(t *testing.T) {
	t.Parallel()

	start := func() *Controller {
		c := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
		c.Down.Rate = 50

		return c
	}

	stalled, quietGateway := start(), start()
	for n := range 6 {
		// A TRICKLE UP, so only the download is full; the gateway's rise is
		// under the window's majority, so it never cuts, only holds.
		gateway := gatewayRose(time.Millisecond)
		if n%3 == 0 {
			gateway = gatewayRose(30 * time.Millisecond)
		}

		stalled.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), gateway))
		quietGateway.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), gatewayRose(time.Millisecond)))
	}

	if quietGateway.Down.Rate <= 50 {
		t.Fatalf("a full download on a clear line and a quiet gateway did not climb (%.1f)", quietGateway.Down.Rate)
	}
	if stalled.Down.Rate >= quietGateway.Down.Rate {
		t.Errorf("a gateway risen 30 ms in a third of the rounds held the rate no lower than a quiet one "+
			"(%.1f against %.1f)", stalled.Down.Rate, quietGateway.Down.Rate)
	}
}

// A GATEWAY THAT DOES NOT ANSWER IS NEITHER A LOSS NOR A QUEUE: many answer ICMP
// only when they have the time, and the reference node's went unanswered about
// 2% of the time with the line quiet (2026-10-16). Read as loss it would cut a
// busy host on every miss; read as a stall it would hold every rate.
func TestAGatewayThatDoesNotAnswerChangesNothing(t *testing.T) {
	t.Parallel()

	lost, quiet := newController(), newController()
	for n := range 12 {
		lost.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayLost()))
		quiet.Step(moving(n, 5, 400, 2*time.Millisecond))
	}

	if lost.Down.Rate != quiet.Down.Rate || lost.Up.Rate != quiet.Up.Rate {
		t.Errorf("a gateway that never answered moved the rates to %.1f/%.1f, want %.1f/%.1f as without it",
			lost.Up.Rate, lost.Down.Rate, quiet.Up.Rate, quiet.Down.Rate)
	}

	// AND ONE THAT STOPPED ANSWERING FOR GOOD GOES SILENT AND STOPS COUNTING
	// (delay.go's silentAfter), after which a rate below its ceiling climbs as
	// it would without it; while it is still counted, a round it gave no reading
	// in is not clear (TestAGatewayThatGaveNoReadingIsNotAClearRound).
	var gw Delays
	for range silentAfter + 1 {
		gw.Observe([]string{"192.168.1.254"}, nil)
	}
	gw.Observe([]string{"192.168.1.254"}, map[string]time.Duration{"192.168.1.254": time.Millisecond})
	for range silentAfter + 1 {
		gw.Observe([]string{"192.168.1.254"}, nil)
	}
	silent := gw.Observe([]string{"192.168.1.254"}, nil)
	if len(silent.Probes) != 0 {
		t.Fatalf("a gateway unanswered %d rounds in a row is still counted: %+v", silentAfter+2, silent)
	}

	c := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
	c.Down.Rate = 50
	for n := range 6 {
		c.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), silent))
	}
	if c.Down.Rate <= 50 {
		t.Errorf("a full download beside a gateway gone silent did not climb (%.1f)", c.Down.Rate)
	}
}

// A CUT THAT SHORTENED THE GATEWAY'S STALL HELPED, measured on the deeper of the
// two delays: the futility rule compares the queue the cut was taken on.
func TestAShorterGatewayStallCountsAsHelping(t *testing.T) {
	t.Parallel()

	// A CUT TAKEN WITH THE GATEWAY AT 60 MS AND THE REFLECTORS AT 20, through
	// the controller, so the evidence it keeps is what a cut records.
	c := newController()
	for n := range 3 {
		c.Step(withGateway(moving(n, 5, 400, 20*time.Millisecond), gatewayRose(60*time.Millisecond)))
	}
	d := &c.Down
	if !d.episode || d.cutGateway != (measured{60 * time.Millisecond, true}) ||
		d.cutReflectors != (measured{20 * time.Millisecond, true}) {
		t.Fatalf("the cut kept gateway %+v and reflectors %+v, want 60 ms and 20 ms, both measured",
			d.cutGateway, d.cutReflectors)
	}

	// EACH READING BESIDE THE ONE BEFORE IT, half a second earlier, as the
	// controller pairs them: the gateway is evidence only on two readings.
	ms := time.Millisecond
	round := func(reflectors, before time.Duration, now Sample) (verdict, Observation) {
		k := &Controller{Params: c.Params, lastGateway: measured{before, true}, lastGatewayAt: at(8)}
		o := withGateway(moving(9, 5, 400, reflectors), now)

		return verdict{gateway: k.readGateway(o)}, o
	}

	for name, tc := range map[string]struct {
		reflectors, before time.Duration
		now                Sample
		forgetReflectors   bool
		shortened, judged  bool
	}{
		"the gateway's stall fell to 20 ms":   {20 * ms, 22 * ms, gatewayRose(20 * ms), false, true, true},
		"the gateway's stall stayed at 59 ms": {20 * ms, 61 * ms, gatewayRose(59 * ms), false, false, true},
		// A FIFTH OF ITS 60 MS, NOT A TENTH: one ping wanders more than a median.
		"the gateway's stall read 50 ms": {20 * ms, 52 * ms, gatewayRose(50 * ms), false, false, true},
		// JUDGED BY THE PAIR'S LESSER READING, as the cut was: 70 beside 45.
		"the gateway's stall wandered 45 to 70 ms": {20 * ms, 45 * ms, gatewayRose(70 * ms), false, true, true},
		// SOURCE AGAINST SOURCE: the reflectors' unchanged 20 ms is not a shorter
		// 60 ms stall, though the gateway went unanswered.
		"the gateway went unanswered and the reflectors stayed": {
			20 * ms, 60 * ms, gatewayLost(), false, false, true},
		"the reflectors fell to 12 ms beside a gateway still stalled": {
			12 * ms, 60 * ms, gatewayRose(60 * ms), false, true, true},
		// ONE REPLY SLIPPING THROUGH A STALL IS NOT THE STALL GONE; TWO ARE.
		"one quiet reply beside a stalled one": {20 * ms, 60 * ms, gatewayRose(ms), false, false, true},
		"two quiet replies":                    {20 * ms, 2 * ms, gatewayRose(ms), false, true, true},
		// NEITHER SOURCE MEASURED THEN AND NOW: a cut that kept no reflectors'
		// delay, and a gateway that gave no reading.
		"nothing measured both then and now": {20 * ms, 60 * ms, gatewayLost(), true, false, false},
		"only the gateway measured both":     {20 * ms, 61 * ms, gatewayRose(59 * ms), true, false, true},
	} {
		v, o := round(tc.reflectors, tc.before, tc.now)
		cut := *d
		if tc.forgetReflectors {
			cut.cutReflectors = measured{}
		}

		if shortened, judged := helped(&cut, v, o); shortened != tc.shortened || judged != tc.judged {
			t.Errorf("%s: helped %v, judged %v; want %v, %v", name, shortened, judged, tc.shortened, tc.judged)
		}
	}

	// A QUIET PAIR IS JUDGED BY ITS GREATER READING: 14 ms beside 1 ms is not a
	// fifth under a stall of 17 ms cut on just past Bloat.
	cut := *d
	cut.cutReflectors, cut.cutGateway = measured{}, measured{17 * ms, true}
	v, o := round(20*ms, 14*ms, gatewayRose(ms))
	if shortened, judged := helped(&cut, v, o); shortened || !judged {
		t.Errorf("a quiet pair of 14 and 1 ms against a 17 ms stall judged helped %v, judged %v; want false, true",
			shortened, judged)
	}

	// THE SAME REFLECTORS ASKED THEN AND NOW JUDGE A CUT BY THEIR LOSS, though no
	// delay was measured both times.
	cut = *d
	cut.cutReflectors = measured{}
	v, o = round(20*ms, 60*ms, gatewayLost())
	v.tally = cut.cutTally
	if len(cut.cutTally) == 0 {
		t.Fatal("the cut kept no tally of its reflectors' probes")
	}
	if shortened, judged := helped(&cut, v, o); shortened || !judged {
		t.Errorf("an unchanged loss judged helped %v, judged %v; want false, true", shortened, judged)
	}
}

// A ROUND THAT COUNTED NO REFLECTOR NEITHER CUTS NOR RAISES, GATEWAY OR NOT: a
// line whose far end has gone quiet is a could-not-tell, and its first hop alone
// says nothing of it.
func TestAGatewayAloneNeverCutsOrRaises(t *testing.T) {
	t.Parallel()

	c := newController()
	for n := range 8 {
		c.Step(withGateway(probed(n, 5, 400, Sample{}), gatewayRose(40*time.Millisecond)))
	}
	if c.Down.Rate != c.Down.Max {
		t.Errorf("a gateway stalled 40 ms with no reflector counted cut the download to %.1f", c.Down.Rate)
	}

	// NOR DOES A GATEWAY THAT ANSWERED, WITH THE REFLECTORS UNMEASURED, RAISE: a
	// round is clear only when the reflectors measured the line.
	// THE FIRST LOSSY ROUND IN THE WINDOW, so the loss rule is not yet in force:
	// clean rounds climb, and that one, with the reflectors unmeasured, must not.
	r := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
	r.Down.Rate = 50
	for n := range 5 {
		r.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), gatewayRose(time.Millisecond)))
	}
	before := r.Down.Rate
	if before <= 50 {
		t.Fatalf("clean full rounds did not climb (%.1f)", before)
	}

	unmeasured := Sample{Probes: []Probe{{Reflector: "a", Lost: true}, {Reflector: "b", Lost: true}, {Reflector: "c"}}}
	r.Step(withGateway(probed(5, 1, 49, unmeasured), gatewayRose(time.Millisecond)))
	if r.Down.Rate != before {
		t.Errorf("a round with only the gateway measured moved the rate from %.1f to %.1f", before, r.Down.Rate)
	}
}

// A SINGLE GATEWAY SPIKE IS NOT A STALL: its rise is the one two rounds in a row
// reach, so a gateway that hiccups every other round cuts nothing, while one that
// stays stalled does.
func TestASingleGatewaySpikeCutsNothing(t *testing.T) {
	t.Parallel()

	spiky := &fakeProber{reflectors: three, gateway: "192.168.1.254", rtt: func(round int) map[string]time.Duration {
		answers := idle()
		answers["192.168.1.254"] = time.Millisecond
		if round > 0 && round%2 == 0 {
			answers["192.168.1.254"] = 80 * time.Millisecond
		}

		return answers
	}}
	r := newRig(t, spiky, 400)
	r.run(30)
	if len(r.cuts()) != 0 {
		t.Errorf("a gateway spiking every other round cut %d times", len(r.cuts()))
	}

	stalled := &fakeProber{reflectors: three, gateway: "192.168.1.254", rtt: func(round int) map[string]time.Duration {
		answers := idle()
		answers["192.168.1.254"] = time.Millisecond
		if round > 0 {
			answers["192.168.1.254"] = 80 * time.Millisecond
		}

		return answers
	}}
	r = newRig(t, stalled, 400)
	r.run(30)
	if len(r.cuts()) == 0 {
		t.Error("a gateway stalled 80 ms in every round cut nothing")
	}
}

// A ROUND ASKS THE GATEWAY BESIDE THE REFLECTORS, ONCE, in the spelling its
// reply comes from; a gateway that is not an IPv4 address is refused.
func TestARoundAsksTheGatewayBesideTheReflectors(t *testing.T) {
	t.Parallel()

	reflectors, gateway, targets, err := probeTargets([]string{"1.1.1.1", "8.8.8.8"}, "::ffff:192.168.1.254")
	if err != nil {
		t.Fatal(err)
	}
	if gateway != "192.168.1.254" || strings.Join(reflectors, ",") != "1.1.1.1,8.8.8.8" ||
		strings.Join(targets, ",") != "1.1.1.1,8.8.8.8,192.168.1.254" {
		t.Errorf("reflectors %v, gateway %q, targets %v", reflectors, gateway, targets)
	}

	// A GATEWAY THAT IS ALSO A REFLECTOR IS ASKED ONCE, AS A REFLECTOR ONLY: in
	// both roles its misses would reach the loss rule.
	if _, gateway, targets, err := probeTargets([]string{"1.1.1.1"}, "1.1.1.1"); err != nil || len(targets) != 1 ||
		gateway != "" {
		t.Errorf("a gateway that is also a reflector is asked as %v, gateway %q (%v), want once and as a "+
			"reflector only", targets, gateway, err)
	}
	if _, _, targets, err := probeTargets([]string{"1.1.1.1"}, ""); err != nil || strings.Join(targets, ",") != "1.1.1.1" {
		t.Errorf("no gateway asked %v (%v)", targets, err)
	}
	if _, _, _, err := probeTargets([]string{"1.1.1.1"}, "fe80::1"); err == nil {
		t.Error("a gateway that is not IPv4 was accepted")
	}
}
