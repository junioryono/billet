package uplink

import (
	"testing"
	"time"
)

// A CUT TAKEN ON THE GATEWAY'S STALL HELPED ONLY IF THE STALL SHORTENED: the
// reflectors' delay wandering under Bloat, 12 ms then 8 ms, says nothing of it,
// and crediting it would keep cutting a host for a queue that is not its own.
//
// AND THE OTHER WAY ROUND: a gateway wandering under Bloat says nothing of the
// reflectors' queue. The jitter's period is four rounds and a cut's cooldown six,
// so successive cuts see the wandering source at its two values.
func TestJitterUnderBloatNeverCreditsACut(t *testing.T) {
	t.Parallel()

	jitter := func(n int) time.Duration {
		if n%4 < 2 {
			return 12 * time.Millisecond
		}

		return 8 * time.Millisecond
	}

	for name, observe := range map[string]func(n int) Observation{
		"the reflectors wander under a stalled gateway": func(n int) Observation {
			return withGateway(moving(n, 5, 400, jitter(n)), gatewayRose(60*time.Millisecond))
		},
		"the gateway wanders under stalled reflectors": func(n int) Observation {
			return withGateway(moving(n, 5, 400, 60*time.Millisecond), gatewayRose(jitter(n)))
		},
	} {
		c := newController()
		cuts := 0

		for n := range 600 {
			c.Step(observe(n))
			if c.Cause != CauseNone {
				cuts++
			}
		}

		if cuts > c.Params.Futile+1 {
			t.Errorf("%s: a stall no cut shortened was cut %d times; the futility latch should hold after %d",
				name, cuts, c.Params.Futile)
		}
	}
}

// A GATEWAY BEING COUNTED THAT GAVE NO READING IS NOT A CLEAR ROUND: a stalled
// gateway that misses a few probes has not cleared, so the episode its stall
// latched stays latched, and no rate climbs on it.
func TestAGatewayThatGaveNoReadingIsNotAClearRound(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Down.episode, c.Down.futile = true, c.Params.Futile
	c.Down.lastCut, c.Down.cutSince = at(-100), true

	for n := range 12 {
		c.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayLost()))
	}

	if !c.Down.episode || c.Down.futile != c.Params.Futile {
		t.Errorf("rounds with the gateway unmeasured cleared a latched episode (episode %v, futile %d)",
			c.Down.episode, c.Down.futile)
	}

	r := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
	r.Down.Rate = 50
	for n := range 6 {
		r.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), gatewayLost()))
	}
	if r.Down.Rate != 50 {
		t.Errorf("a full download climbed to %.1f beside a counted gateway that gave no reading", r.Down.Rate)
	}

	// A GATEWAY THAT HAS GONE SILENT IS NOT COUNTED, and a line it no longer
	// measures still clears and climbs.
	s := &Controller{Params: DefaultParams(), Up: NewDirection(100), Down: NewDirection(100)}
	s.Down.Rate = 50
	for n := range 6 {
		s.Step(withGateway(moving(n, 1, 49, 2*time.Millisecond), Sample{}))
	}
	if s.Down.Rate <= 50 {
		t.Errorf("a full download beside a gateway no longer counted did not climb (%.1f)", s.Down.Rate)
	}
}

// FOLLOWING A NEW GATEWAY FORGETS WHAT THE OLD ONE TAUGHT: its delays in the
// windows and the episode its stall latched, so the new gateway, or this host's
// own traffic, can be acted on.
func TestForgettingAGatewayDropsItsWindowsAndEpisodes(t *testing.T) {
	t.Parallel()

	c := newController()
	for n := range 12 {
		c.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(60*time.Millisecond)))
	}
	if !c.Down.episode || len(c.delayTrail) == 0 {
		t.Fatalf("no episode to forget (episode %v, trail %v)", c.Down.episode, c.delayTrail)
	}

	c.forgetGateway()

	for _, d := range []*Direction{&c.Up, &c.Down} {
		if d.episode || d.futile != 0 || d.cutGateway.known {
			t.Errorf("a direction kept the old gateway's episode %v, futility %d or evidence %+v",
				d.episode, d.futile, d.cutGateway)
		}
	}
	if len(c.recent) != 0 || len(c.delayTrail) != 0 {
		t.Errorf("the windows kept the old gateway's delays: recent %v, trail %v", c.recent, c.delayTrail)
	}
}
