package uplink

import (
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"
)

// simLine is a site's line as the shaper meets it, for a host whose download
// always wants more than it is let through: what flows is the lesser of CAKE's
// rate and the line's capacity, and a rate past the capacity overruns the line,
// which shows the way its gateway does. One that queues raises every reflector's
// round trip; one that drops loses each probe with dropShare and leaves the
// round trip alone, the shape of the reference site's (2026-10-14, a node
// sending up to 888 Mbit/s raised it about 11 ms). Not overrun, each probe is
// lost with quietShare, the reflectors' own noise.
type simLine struct {
	capacity   func(round int) float64
	drops      bool
	dropShare  float64
	quietShare float64
	rng        *rand.Rand
}

// simRun is what a stretch of rounds did: how many overran the line, what
// flowed against the capacity, and the highest rate against it.
type simRun struct {
	rounds, overran   int
	carried, capacity float64
	highest           float64
}

func (r simRun) overrunShare() float64 { return float64(r.overran) / float64(r.rounds) }
func (r simRun) carriedShare() float64 { return r.carried / r.capacity }

// drive steps c through rounds from..to of l, a trickle up and the download
// wanting all it is let through, the probes read through the real Delays, and
// tallies the rounds from tally on. What the controller sees moving is the
// average of the last rateWindow's rounds, as the stepper gives it, so a climb
// is judged on throughput that lags it.
func (l *simLine) drive(c *Controller, delays *Delays, from, to, tally int) simRun {
	reflectors := []string{"a", "b", "c"}
	idle := map[string]time.Duration{"a": 7 * time.Millisecond, "b": 9 * time.Millisecond,
		"c": 12 * time.Millisecond}

	var run simRun

	var window []float64

	for n := from; n < to; n++ {
		capacity := l.capacity(n)
		overrun := c.Down.Rate > capacity
		flowing := min(c.Down.Rate, capacity)

		answered := map[string]time.Duration{}
		for _, r := range reflectors {
			share := l.quietShare
			if overrun && l.drops {
				share = l.dropShare
			}

			if l.rng.Float64() < share {
				continue
			}

			answered[r] = idle[r] + time.Duration(l.rng.IntN(2000))*time.Microsecond
			if overrun && !l.drops {
				answered[r] += 40 * time.Millisecond
			}
		}

		window = lastRounds(append(window, flowing), int(rateWindow/Tick))
		seen := 0.0
		for _, f := range window {
			seen += f / float64(len(window))
		}

		c.Step(probed(n, 1, seen, delays.Observe(reflectors, answered)))

		if n >= tally {
			run.rounds++
			run.carried += flowing
			run.capacity += capacity
			run.highest = max(run.highest, c.Down.Rate/capacity)

			if overrun {
				run.overran++
			}
		}
	}

	return run
}

func steady(mbit float64) func(int) float64 { return func(int) float64 { return mbit } }

// minute is how many rounds a minute is.
const minute = int(time.Minute / Tick)

// A HOST WITH NO CEILING AND NOTHING LEARNED FINDS A LINE HALF ITS INTERFACE'S
// SPEED AND STAYS UNDER IT, on a line that queues and on one that drops, the
// kind that needed a ceiling of 150 Mbit/s by hand (2026-10-16). It starts low,
// climbs, finds the line by the cut that overruns it, and after that overruns it
// only now and then, briefly, while still carrying most of what it can.
//
// THE BOUNDS ARE THIS MODEL'S, not a measurement of any real line: every
// overrun here shows at once, at a loss share a real gateway may not reach, and
// what flows follows CAKE's rate exactly. They pin the shape: without the slow
// climb near the line, a rate cut on loss is back over it within seconds, and
// the overrun share is several times these.
func TestWithNoCeilingTheShaperFindsTheLineAndStaysUnderIt(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name  string
		drops bool
	}{
		{"a line that queues", false},
		{"a line that drops", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			line := &simLine{capacity: steady(500), drops: c.drops, dropShare: 0.1, quietShare: 0.002,
				rng: rand.New(rand.NewPCG(1, 2))}
			ctl := newRunController(1000, Options{Params: DefaultParams()})
			delays := &Delays{}

			// THE FIRST TWO MINUTES CLIMB FROM START AND FIND THE LINE.
			first := line.drive(ctl, delays, 0, 2*minute, 0)
			if ctl.Down.Limit <= 0 {
				t.Fatalf("two minutes of a full download found no line: rate %.1f", ctl.Down.Rate)
			}
			// A NINTH ABOVE, AND THE ONE RAISE THAT CROSSES IT: once what flows is
			// under ProbeFull of the rate, no raise follows.
			p := DefaultParams()
			if bound := p.Raise/p.ProbeFull + 0.01; first.highest > bound {
				t.Errorf("climbing from %.0f the rate reached %.2f of the line, want at most %.2f: the climb "+
					"should stop once what flows stops following it", p.Start, first.highest, bound)
			}

			// AND THE NEXT HALF HOUR STAYS UNDER IT.
			rest := line.drive(ctl, delays, 2*minute, 32*minute, 2*minute)
			if share := rest.overrunShare(); share > 0.1 {
				t.Errorf("the rate overran the line in %.1f%% of the rounds after it was found, want at most 10%%",
					100*share)
			}
			if share := rest.carriedShare(); share < 0.8 {
				t.Errorf("the host carried %.1f%% of what the line could, want at least 80%%", 100*share)
			}
			if bound := p.ProbeRaise/p.ProbeFull + 0.01; rest.highest > bound {
				t.Errorf("the rate reached %.2f of the line after finding it, want at most %.2f", rest.highest,
					bound)
			}

			t.Logf("%s: overran %.1f%% of rounds, carried %.1f%%, highest %.2f of the line, found at %.1f",
				c.name, 100*rest.overrunShare(), 100*rest.carriedShare(), rest.highest, ctl.Down.Limit)
		})
	}
}

// A LINE AS FAST AS THE INTERFACE IS NEVER THROTTLED FOR LONG: the climb from
// Start reaches the interface's speed within a minute and stays there, with
// nothing found to slow it. The case a rule on rate alone would throttle for
// good.
func TestALineFasterThanTheInterfaceIsClimbedToItsSpeed(t *testing.T) {
	t.Parallel()

	line := &simLine{capacity: steady(2000), quietShare: 0.002, rng: rand.New(rand.NewPCG(3, 4))}
	ctl := newRunController(1000, Options{Params: DefaultParams()})
	delays := &Delays{}

	line.drive(ctl, delays, 0, minute, 0)

	if ctl.Down.Rate != 1000 || ctl.Down.Limit != 0 {
		t.Fatalf("a minute of a full download on a line faster than the interface left the rate at %.1f "+
			"(found at %.1f), want the interface's 1000 with nothing found", ctl.Down.Rate, ctl.Down.Limit)
	}
}

// A LINE THAT CHANGED IS FOUND AGAIN, either way: one that doubles is climbed to
// within minutes, slowly, and one that halves is cut to at once.
func TestALineThatChangesIsFoundAgain(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name          string
		before, after float64
		within        int
	}{
		{"a line that doubles", 300, 600, 10 * minute},
		{"a line that halves", 600, 300, minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			change := 10 * minute
			line := &simLine{drops: true, dropShare: 0.1, quietShare: 0.002, rng: rand.New(rand.NewPCG(5, 6)),
				capacity: func(n int) float64 {
					if n < change {
						return c.before
					}

					return c.after
				}}
			ctl := newRunController(1000, Options{Params: DefaultParams()})
			delays := &Delays{}

			line.drive(ctl, delays, 0, change, 0)
			line.drive(ctl, delays, change, change+c.within, change)
			after := line.drive(ctl, delays, change+c.within, change+c.within+5*minute, change+c.within)

			if share := after.carriedShare(); share < 0.8 {
				t.Errorf("%d minutes after the line went from %.0f to %.0f the host carried %.1f%% of it",
					c.within/minute, c.before, c.after, 100*share)
			}
			if share := after.overrunShare(); share > 0.1 {
				t.Errorf("%d minutes after the line went from %.0f to %.0f the rate overran it in %.1f%% of "+
					"the rounds", c.within/minute, c.before, c.after, 100*share)
			}
		})
	}
}

// A RESTARTED SHAPER STARTS WHERE THE LAST ONE LEFT THE LINE, not low again and
// not at the interface's speed, and no higher than Near of it: stopped while it
// probed past the line, as here, the next run does not start overrunning it,
// and the minutes after a restart overrun it no more than any others.
func TestARestartedShaperStartsWhereTheLineWasLeft(t *testing.T) {
	fakeRuntimeDir(t)

	line := &simLine{capacity: steady(500), drops: true, dropShare: 0.1, quietShare: 0.002,
		rng: rand.New(rand.NewPCG(7, 8))}
	ctl := newRunController(1000, Options{Params: DefaultParams()})
	delays := &Delays{}
	line.drive(ctl, delays, 0, 10*minute, 0)

	// STOPPED PAST THE LINE, the case the cap is for.
	stop := 10 * minute
	for ; ctl.Down.Rate <= 500 && stop < 20*minute; stop++ {
		line.drive(ctl, delays, stop, stop+1, stop+1)
	}
	if ctl.Down.Rate <= 500 || ctl.Down.Limit == 0 {
		t.Fatalf("ten minutes more never probed past the line: rate %.1f, found at %.1f", ctl.Down.Rate,
			ctl.Down.Limit)
	}

	at := nic{"eno2np1", "2", testMAC, "boot-a"}
	rememberer(slog.New(&records{}), ctl, at)(ctl.Up.Rate, ctl.Down.Rate)

	next := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(&records{}), next, at)

	// WHERE THE RUN LEFT IT, BUT NO HIGHER THAN NEAR OF THE LINE: one stopped
	// while it probed past the line would otherwise start the next overrunning.
	want := min(ctl.Down.Rate, DefaultParams().Near*ctl.Down.Limit)
	if next.Down.Rate != want || next.Down.Limit != ctl.Down.Limit || next.Down.Limit == 0 {
		t.Fatalf("the next run started at %.1f with the line at %.1f, want %.1f and the %.1f left",
			next.Down.Rate, next.Down.Limit, want, ctl.Down.Limit)
	}

	restarted := &Delays{}
	if first := line.drive(next, restarted, stop, stop+20*int(time.Second/Tick), stop); first.overran != 0 {
		t.Errorf("the first 20 s after a restart overran the line in %d rounds, so it resumed past it",
			first.overran)
	}

	after := line.drive(next, restarted, stop+20*int(time.Second/Tick), stop+5*minute, stop)
	if share := after.overrunShare(); share > 0.1 {
		t.Errorf("the five minutes after a restart overran the line in %.1f%% of the rounds", 100*share)
	}
	if share := after.carriedShare(); share < 0.8 {
		t.Errorf("the five minutes after a restart carried %.1f%% of the line, so it started low again",
			100*share)
	}
}
