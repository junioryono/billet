package uplink

import (
	"testing"
	"time"
)

// A CUT FINDS THE LINE: what the direction was moving when it was cut is its
// Limit, whatever it was before, and the other direction's is untouched.
func TestACutFindsTheLineAtWhatWasMoving(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Down.Limit = 900

	for n := range 3 {
		c.Step(moving(n, 5, 378, 30*time.Millisecond))
	}

	if c.Cause == CauseNone || c.Down.Rate >= 378 {
		t.Fatalf("a full queue under a 378 Mbit/s download did not cut it (rate %.1f)", c.Down.Rate)
	}
	if c.Down.Limit != 378 {
		t.Errorf("the cut found the line at %.1f, want the 378 Mbit/s that was moving", c.Down.Limit)
	}
	if c.Up.Limit != 0 {
		t.Errorf("the upload, never cut, found the line at %.1f", c.Up.Limit)
	}
}

// clearRounds steps c through rounds from..to with a clear line, the download
// moving share of its rate, and reports the rate after each.
func clearRounds(c *Controller, from, to int, share float64) []float64 {
	var rates []float64

	for n := from; n < to; n++ {
		c.Step(moving(n, 1, share*c.Down.Rate, time.Millisecond))
		rates = append(rates, c.Down.Rate)
	}

	return rates
}

// BELOW NEAR OF THE LINE FOUND, A RATE CLIMBS BACK AT RAISE EACH ROUND, under
// Full, and stops at Near of the line rather than a raise past it.
func TestBelowTheLineFoundARateClimbsBackQuickly(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	c := newController()
	c.Down.Rate, c.Down.Limit = 100, 400

	// UNDER FULL, THOUGH NOT UNDER PROBEFULL: what flows needs only to show the
	// rate is what holds it back.
	rates := clearRounds(c, 0, 1, 0.8)
	if want := 100*p.Raise + 0.5; rates[0] != want {
		t.Fatalf("one clear round below the line moved the rate from 100 to %.2f, want %.2f", rates[0], want)
	}

	// THE FIRST RATE AT NEAR OR PAST IT IS NEAR ITSELF, where the creep takes over.
	rates = clearRounds(c, 1, 60, 1)
	for i, rate := range rates {
		if rate >= p.Near*400 {
			if rate != p.Near*400 || i > 40 {
				t.Errorf("the climb below the line first reached %.2f after %d rounds, want exactly Near of it, "+
					"%.0f, within 40", rate, i+1, p.Near*400)
			}

			return
		}
	}

	t.Errorf("a half minute of clear rounds below the line ended at %.2f, short of Near of it",
		rates[len(rates)-1])
}

// FROM NEAR OF THE LINE ON, A RATE CREEPS: ProbeRaise at most once every
// ProbeEvery, and only while what flows follows it (ProbeFull), so a line that
// drops before it queues is overrun now and then rather than at every climb.
func TestNearTheLineARateCreeps(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	c := newController()
	c.Down.Rate, c.Down.Limit = 400, 400

	rounds := int(p.ProbeEvery / Tick)
	rates := clearRounds(c, 0, 4*rounds, 1)

	raises := 0
	for i := 1; i < len(rates); i++ {
		if rates[i] > rates[i-1] {
			raises++

			if want := rates[i-1]*p.ProbeRaise + 0.5; rates[i] != want {
				t.Errorf("a raise near the line went from %.2f to %.2f, want %.2f", rates[i-1], rates[i], want)
			}
		}
	}

	// THE FIRST ROUND RAISES, the next one ProbeEvery after it, and so on.
	if rates[0] <= 400 || raises != 3 {
		t.Errorf("%d rounds near the line raised %d times after the first (first to %.2f), want one every "+
			"%v: 3", len(rates), raises, rates[0], p.ProbeEvery)
	}

	// AND NOT WHEN WHAT FLOWS FALLS SHORT OF PROBEFULL OF THE RATE, though it is
	// over Full: the line, not CAKE, is then what holds the traffic back.
	held := newController()
	held.Down.Rate, held.Down.Limit = 400, 400
	if rates := clearRounds(held, 0, 4*rounds, (p.Full+p.ProbeFull)/2); rates[len(rates)-1] != 400 {
		t.Errorf("a rate near the line, what flows short of ProbeFull of it, rose to %.2f", rates[len(rates)-1])
	}
}

// WITH NO LINE FOUND, A RATE CLIMBS AT RAISE EACH ROUND, but only while what
// flows follows it: a line slower than the rate stops the climb a ninth above
// itself, not a third, as Full alone would.
func TestWithNoLineFoundARateClimbsOnlyWhileWhatFlowsFollows(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	c := newController()
	c.Down.Rate = 100

	if rates := clearRounds(c, 0, 3, 1); rates[2] <= rates[1] || rates[1] <= rates[0] || rates[0] <= 100 {
		t.Fatalf("clear rounds with no line found did not climb every round: %v", rates)
	}

	held := newController()
	held.Down.Rate = 100
	if rates := clearRounds(held, 0, 10, (p.Full+p.ProbeFull)/2); rates[len(rates)-1] != 100 {
		t.Errorf("with no line found a rate whose traffic fell short of ProbeFull of it rose to %.2f",
			rates[len(rates)-1])
	}
}
