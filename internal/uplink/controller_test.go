package uplink

import (
	"testing"
	"time"
)

// at is the step clock: tick n is n half-seconds after an arbitrary start.
func at(n int) time.Time { return time.Unix(1_000_000, 0).Add(time.Duration(n) * Tick) }

// moving is an observation of these rates in Mbit/s over one tick.
func moving(n int, upMbit, downMbit float64, delay time.Duration) Observation {
	bits := func(mbit float64) uint64 { return uint64(mbit * 1e6 * Tick.Seconds()) }

	return Observation{At: at(n), Interval: Tick, SentBits: bits(upMbit), ReceivedBits: bits(downMbit),
		Delay: delay, DelayKnown: true}
}

func newController() *Controller {
	return &Controller{Params: DefaultParams(), Up: NewDirection(10000), Down: NewDirection(10000)}
}

// A FULL QUEUE UNDER THIS HOST'S OWN DOWNLOAD CUTS THE DOWNLOAD BELOW WHAT WAS
// FLOWING, and only once the window agrees. Measured at the reference site: the
// node moving 378 Mbit/s down raised the round trip from 7 ms to 22-29 ms.
func TestAFullQueueUnderTheHostsDownloadCutsItBelowWhatFlowed(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 2 {
		if c.Step(moving(n, 5, 378, 22*time.Millisecond)) {
			t.Fatalf("step %d: %d delayed samples of %d cut a rate; one slow answer must change nothing",
				n, n+1, c.Params.Detect)
		}
	}

	if !c.Step(moving(2, 5, 378, 22*time.Millisecond)) {
		t.Fatal("three delayed samples in six while the host downloaded cut nothing")
	}

	if c.Down.Rate >= 378 || c.Down.Rate < 378*c.Params.DeepCut {
		t.Fatalf("the download was cut to %.1f Mbit/s; want just below the 378 that flowed", c.Down.Rate)
	}

	// THE UPLOAD IS ITS OWN PEAK BUT NOT THE LINE'S PROBLEM: five Mbit/s of
	// acknowledgements beside 378 down filled nothing.
	if c.Up.Rate != c.Up.Max {
		t.Fatalf("the upload, 5 Mbit/s beside a 378 Mbit/s download, was cut to %.1f; "+
			"it carried the download's acknowledgements and filled nothing", c.Up.Rate)
	}
}

// A QUEUE THE HOST DID NOT FILL IS NOT THE HOST'S TO EMPTY. Somebody else at the
// site streaming raises the delay while this host is quiet, and cutting it then
// slows the fleet and helps nobody.
func TestADelayWhileTheHostIsQuietCutsNothing(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Step(moving(0, 50, 400, 0))

	for n := 1; n < 8; n++ {
		if c.Step(moving(n, 1, 2, 40*time.Millisecond)) {
			t.Fatalf("step %d: a delay with the host moving 2 of a 400 Mbit/s peak cut its rate to %.1f",
				n, c.Down.Rate)
		}
	}
}

// THE CUT GROWS WITH THE DELAY: barely over the threshold trims, far over empties.
func TestTheCutGrowsWithTheDelay(t *testing.T) {
	t.Parallel()

	c := newController()
	p := c.Params

	if got := c.cutFactor(p.Bloat); got != p.GentleCut {
		t.Errorf("at the threshold the cut is %.3f, want %.3f", got, p.GentleCut)
	}

	if got := c.cutFactor(p.Severe * 2); got != p.DeepCut {
		t.Errorf("far past severe the cut is %.3f, want %.3f", got, p.DeepCut)
	}

	if mid := c.cutFactor((p.Bloat + p.Severe) / 2); mid <= p.DeepCut || mid >= p.GentleCut {
		t.Errorf("halfway the cut is %.3f, want between %.3f and %.3f", mid, p.DeepCut, p.GentleCut)
	}
}

// A RATE RISES ONLY WHILE IT HOLDS THE TRAFFIC BACK AND THE LINE SHOWS NO QUEUE,
// and never above the interface; a cooldown keeps cuts apart.
func TestARateRisesOnlyWhileItIsTheLimitAndCutsKeepTheirDistance(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 3 {
		c.Step(moving(n, 0, 300, 30*time.Millisecond))
	}

	cut := c.Down.Rate
	if cut >= 300 {
		t.Fatalf("no cut to start from: %.1f", cut)
	}

	if c.Step(moving(3, 0, cut, 30*time.Millisecond)) && c.Down.Rate < cut {
		t.Fatalf("a second cut %v after the first; the cooldown is %v", Tick, c.Params.Cooldown)
	}

	// The window still holds delayed samples; clear rounds age them out.
	for n := 4; n < 4+c.Params.Window; n++ {
		c.Step(moving(n, 0, 1, 0))
	}

	if c.Down.Rate != cut {
		t.Fatalf("an idle direction's rate moved from %.1f to %.1f; idle says nothing about the line",
			cut, c.Down.Rate)
	}

	if !c.Step(moving(20, 0, c.Down.Rate, 0)) || c.Down.Rate <= cut {
		t.Fatalf("a direction running at its rate with no queue did not rise from %.1f", cut)
	}

	c.Down.Rate = c.Down.Max
	c.Step(moving(21, 0, c.Down.Max, 0))

	if c.Down.Rate > c.Down.Max {
		t.Fatalf("the rate rose to %.1f, past the interface's %.0f", c.Down.Rate, c.Down.Max)
	}
}

// NO RATE IS EVER CUT BELOW THE FLOOR, however little was moving.
func TestNoCutGoesBelowTheFloor(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 6 {
		c.Step(moving(n, 0, 3, time.Second))
	}

	if c.Down.Rate < Floor {
		t.Fatalf("the download was cut to %.1f Mbit/s, under the %.0f floor", c.Down.Rate, Floor)
	}
}
