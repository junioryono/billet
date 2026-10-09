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
// site streaming raises the delay while this host carries a trickle, which is
// its own recent peak; cutting it then slows the fleet and helps nobody. From
// the first step, with no larger peak to compare against.
func TestADelayWhileTheHostCarriesATrickleCutsNothing(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 12 {
		if c.Step(moving(n, 1, 8, 40*time.Millisecond)) {
			t.Fatalf("step %d: a delay with the host moving 8 Mbit/s cut it to %.1f/%.1f",
				n, c.Up.Rate, c.Down.Rate)
		}
	}
}

// A SATURATED UPLOAD ON AN ASYMMETRIC LINE IS BLAMED, though the download beside
// it is far larger and the upload is under 20 Mbit/s: a 15 Mbit/s upload fills a
// 500/15 line on its own, and can still be cut above the floor.
func TestASaturatedUploadIsBlamedBesideALargerDownload(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 3 {
		c.Step(moving(n, 15, 250, 30*time.Millisecond))
	}

	if c.Up.Rate >= 15 || c.Up.Rate < Floor {
		t.Fatalf("a 15 Mbit/s upload under a full queue was left at %.1f", c.Up.Rate)
	}
}

// AN UNCHANGED QUEUE READ THROUGH THE BASELINES IS STILL UNCHANGED. Each
// reflector's baseline creeps toward a round trip held above it, so somebody
// else's queue holding the round trip at 47 ms reads a little lower every
// sample; that drift is not a cut helping, and the cuts still stop after two.
func TestBaselineDriftDoesNotReadAsACutHelping(t *testing.T) {
	t.Parallel()

	var d Delays

	c := newController()
	asked := []string{"r"}

	d.Observe(asked, map[string]time.Duration{"r": 7 * time.Millisecond})

	cuts := 0

	for n := range 120 {
		delay, known := d.Observe(asked, map[string]time.Duration{"r": 47 * time.Millisecond})
		before := c.Down.Rate

		o := moving(n, 0, 300, delay)
		o.DelayKnown = known
		c.Step(o)

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a queue that never moved drew %d cuts over a minute; want %d before they stop", cuts, c.Params.Futile)
	}
}

// A QUEUE THAT DRAINED BEFORE THE COOLDOWN ENDED IS NOT CUT FOR. Three delayed
// rounds after a cut and then three clear ones leave the window full when the
// cooldown expires, but the queue is gone.
func TestAQueueThatDrainedIsNotCutForWhenTheCooldownEnds(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 3 {
		c.Step(moving(n, 0, 300, 30*time.Millisecond))
	}

	cut := c.Down.Rate

	for n := 3; n < 6; n++ {
		c.Step(moving(n, 0, 250, 30*time.Millisecond))
	}

	for n := 6; n < 12; n++ {
		c.Step(moving(n, 0, 250, 0))
	}

	if c.Down.Rate < cut {
		t.Fatalf("the download was cut again, to %.1f from %.1f, with the queue already drained", c.Down.Rate, cut)
	}
}

// CUTS THAT DO NOT SHORTEN THE QUEUE STOP: a delay that stays where it was
// through two cuts is not this host's, and cutting on would take it to the floor.
func TestCutsThatLeaveTheDelayAloneStop(t *testing.T) {
	t.Parallel()

	c := newController()
	delay := 40 * time.Millisecond
	f := c.cutFactor(delay)

	for n := range 60 {
		c.Step(moving(n, 0, 300, delay))
	}

	if want := 300 * f * f; c.Down.Rate < want*0.999 || c.Down.Rate > want*1.001 {
		t.Fatalf("after thirty seconds of a delay no cut moved, the download is at %.1f; want two cuts, %.1f",
			c.Down.Rate, want)
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

// CUTS THAT DO SHORTEN THE QUEUE GO ON TO THE FLOOR AND NOT PAST IT: a line far
// slower than the host is followed all the way down.
func TestCutsThatHelpReachTheFloorAndNoFurther(t *testing.T) {
	t.Parallel()

	c := newController()

	// The delay falls by a third or more between cuts, every cut helping.
	for n := range 60 {
		c.Step(moving(n, 0, 25, 2*time.Second*6/time.Duration(6+n)))
	}

	if c.Down.Rate != Floor {
		t.Fatalf("the download ended at %.2f Mbit/s; want exactly the %.0f floor", c.Down.Rate, Floor)
	}
}
