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
	c.Down.Rate, c.Down.Limit = 400, 900

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

// A DOWNLOAD THAT FILLS THE LINE CUTS THE UPLOAD TOO, both directions being cut
// on one verdict, but the acknowledgements the upload carries are not its line:
// taken as one, an upload moving 12 Mbit/s of them under a 400 Mbit/s rate was
// held near 12 for many minutes after every busy download.
func TestAnUploadOfAcknowledgementsKeepsTheLineItFound(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Up.Rate, c.Up.Limit = 400, 450
	c.Down.Rate, c.Down.Limit = 520, 500

	for n := range 3 {
		c.Step(moving(n, 12, 510, 30*time.Millisecond))
	}

	if c.Down.Rate >= 510 || c.Down.Limit != 510 {
		t.Fatalf("a full queue under the download left it at %.1f with the line at %.1f, want a cut that "+
			"finds the line at the 510 moving", c.Down.Rate, c.Down.Limit)
	}
	if c.Up.Rate >= 400 {
		t.Fatalf("the upload, busy with its acknowledgements, was not cut (%.1f): the case this is about "+
			"did not arise", c.Up.Rate)
	}
	if c.Up.Limit != 450 {
		t.Errorf("the cut took the upload's acknowledgements as its line: %.1f, want the 450 it had found",
			c.Up.Limit)
	}

	// AND THROUGH EVERY DOWNLOAD AFTER: the cut left the upload at a rate its
	// acknowledgements fill, so by the next one they are most of its rate, and
	// only the download's throughput says what they are.
	for episode := range 3 {
		base := 10 + episode*40
		for n := base; n < base+30; n++ {
			c.Step(moving(n, min(12, c.Up.Rate), 400, time.Millisecond))
		}

		upBefore := c.Up.Rate
		for n := base + 30; n < base+33; n++ {
			c.Step(moving(n, min(12, c.Up.Rate), 510, 30*time.Millisecond))
		}

		if c.Up.Rate >= upBefore || c.Down.Limit != 510 {
			t.Fatalf("download %d did not cut both directions (upload %.1f from %.1f, download's line %.1f): "+
				"the case this is about did not arise", episode+2, c.Up.Rate, upBefore, c.Down.Limit)
		}
		if c.Up.Limit != 450 {
			t.Fatalf("download %d took the upload's acknowledgements as its line: %.1f, want the 450 it had "+
				"found", episode+2, c.Up.Limit)
		}
	}

	// AND WITH NO LINE FOUND, NONE IS FOUND FROM THEM.
	fresh := newController()
	fresh.Up.Rate, fresh.Down.Rate = 400, 520
	for n := range 3 {
		fresh.Step(moving(n, 12, 510, 30*time.Millisecond))
	}
	if fresh.Up.Limit != 0 || fresh.Down.Limit != 510 {
		t.Errorf("with no line found the cut found the upload's at %.1f and the download's at %.1f, want 0 "+
			"and 510", fresh.Up.Limit, fresh.Down.Limit)
	}
}

// A RAISE NEAR THE LINE IS ONE CAKE IS GIVEN, through the real stepper and its
// two-percent threshold for telling tc: what ProbeFull compares what flows with
// is then what CAKE holds. With raises smaller than the threshold the
// controller's rate ran ahead of CAKE's, and a host moving 90.5% of CAKE's rate
// stopped climbing for good a step above its line. And a rise that comes to
// rest at the ceiling, short of two percent, is applied all the same.
func TestARaiseNearTheLineIsOneCAKEIsGiven(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name     string
		max      float64
		rate     float64
		wantPast float64
	}{
		{"with room above", 10000, 400, 440},
		{"under a ceiling just above", 410, 400, 410},
		// THE FAST CLIMB'S LAST STEP TO NEAR IS UNDER TWO PERCENT, and only
		// resting gives it to CAKE.
		{"climbing back to Near", 10000, 300, 361},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			r := newRig(t, &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration { return idle() }}, 0)
			r.controller.Down = Direction{Max: c.max, Rate: c.rate, Limit: 400}
			r.st.applied[1] = c.rate

			// WHAT FLOWS IS 90.5% OF WHAT CAKE WAS LAST GIVEN, read each round.
			applied := func() float64 {
				if len(r.sets) == 0 {
					return c.rate
				}

				return r.sets[len(r.sets)-1][1]
			}

			for range 5 * minute {
				r.downMbit = 0.905 * applied()
				r.run(1)
			}

			if got := applied(); got < c.wantPast {
				t.Errorf("five minutes near the line moving 90.5%% of CAKE's rate left CAKE at %.1f "+
					"(the controller at %.1f), want at least %.0f", got, r.controller.Down.Rate, c.wantPast)
			}
			if r.controller.Down.Rate != applied() {
				t.Errorf("the controller's rate %.2f is not what CAKE was given, %.2f",
					r.controller.Down.Rate, applied())
			}
		})
	}
}
