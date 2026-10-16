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

	// AND A RATE BELOW ITS CEILING STILL CLIMBS beside a gateway that does not
	// answer.
	c := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
	c.Down.Rate = 50
	for n := range 6 {
		c.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), gatewayLost()))
	}
	if c.Down.Rate <= 50 {
		t.Errorf("a full download beside a gateway that did not answer did not climb (%.1f)", c.Down.Rate)
	}
}

// A CUT THAT SHORTENED THE GATEWAY'S STALL HELPED, measured on the deeper of the
// two delays: the futility rule compares the queue the cut was taken on.
func TestAShorterGatewayStallCountsAsHelping(t *testing.T) {
	t.Parallel()

	d := &Direction{cutDelay: 60 * time.Millisecond, cutDelayKnown: true}
	shorter := withGateway(moving(0, 5, 400, 2*time.Millisecond), gatewayRose(20*time.Millisecond))
	unchanged := withGateway(moving(0, 5, 400, 2*time.Millisecond), gatewayRose(59*time.Millisecond))

	if !helped(d, verdict{}, shorter) {
		t.Error("a cut after which the gateway's stall fell from 60 ms to 20 ms did not count as helping")
	}
	if helped(d, verdict{}, unchanged) {
		t.Error("a cut after which the gateway's stall stayed at 59 ms counted as helping")
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

	if _, _, targets, err := probeTargets([]string{"1.1.1.1"}, "1.1.1.1"); err != nil || len(targets) != 1 {
		t.Errorf("a gateway that is also a reflector is asked as %v (%v), want once", targets, err)
	}
	if _, _, targets, err := probeTargets([]string{"1.1.1.1"}, ""); err != nil || strings.Join(targets, ",") != "1.1.1.1" {
		t.Errorf("no gateway asked %v (%v)", targets, err)
	}
	if _, _, _, err := probeTargets([]string{"1.1.1.1"}, "fe80::1"); err == nil {
		t.Error("a gateway that is not IPv4 was accepted")
	}
}
