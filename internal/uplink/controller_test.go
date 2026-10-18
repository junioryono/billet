package uplink

import (
	"slices"
	"testing"
	"time"
)

// at is the step clock: tick n is n half-seconds after an arbitrary start.
func at(n int) time.Time { return time.Unix(1_000_000, 0).Add(time.Duration(n) * Tick) }

// moving is an observation of these rates in Mbit/s over one tick, with every
// one of three reflectors answering at this delay.
func moving(n int, upMbit, downMbit float64, delay time.Duration) Observation {
	return probed(n, upMbit, downMbit, Sample{Delay: delay, DelayKnown: true,
		Probes: []Probe{{Reflector: "a"}, {Reflector: "b"}, {Reflector: "c"}}})
}

// probed is an observation of these rates with what a round's probes said.
func probed(n int, upMbit, downMbit float64, s Sample) Observation {
	bits := func(mbit float64) uint64 { return uint64(mbit * 1e6 * Tick.Seconds()) }

	return Observation{At: at(n), Interval: Tick, SentBits: bits(upMbit), ReceivedBits: bits(downMbit), Sample: s}
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
		s := d.Observe(asked, map[string]time.Duration{"r": 47 * time.Millisecond})
		before := c.Down.Rate

		c.Step(probed(n, 0, 300, s))

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
// The second, the last before the latch, is the deepest.
func TestCutsThatLeaveTheDelayAloneStop(t *testing.T) {
	t.Parallel()

	c := newController()
	delay := 40 * time.Millisecond
	f := c.cutFactor(delay)

	for n := range 60 {
		c.Step(moving(n, 0, 300, delay))
	}

	if want := 300 * f * c.Params.DeepCut; c.Down.Rate < want*0.999 || c.Down.Rate > want*1.001 {
		t.Fatalf("after thirty seconds of a delay no cut moved, the download is at %.1f; want a cut of %.3f "+
			"and a deep one, %.1f", c.Down.Rate, f, want)
	}
}

// A CUT THAT HELPED IS NOT DEEPENED: the escalation is for a cut that left the
// queue alone, and a queue a gentle cut is shortening keeps being cut gently.
func TestACutThatHelpedIsFollowedByAGentleOne(t *testing.T) {
	t.Parallel()

	c := newController()
	first, second := 40*time.Millisecond, 25*time.Millisecond

	n := 0
	for ; c.Down.Rate >= 300; n++ {
		c.Step(moving(n, 0, 300, first))
	}

	after := c.Down.Rate
	if want := 300 * c.cutFactor(first); after < want*0.999 || after > want*1.001 {
		t.Fatalf("the first cut took the download to %.1f, want %.1f", after, want)
	}

	for ; c.Down.Rate >= after; n++ {
		if n > 60 {
			t.Fatalf("no second cut while the queue stood at %v", second)
		}

		c.Step(moving(n, 0, 300, second))
	}

	if want := after * c.cutFactor(second); c.Down.Rate < want*0.999 || c.Down.Rate > want*1.001 {
		t.Errorf("a cut after one that shortened the queue took the download to %.1f, want a gentle %.1f",
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

// ONE CLEAR ROUND DOES NOT END AN EPISODE. A 30 ms queue no cut moves, with a
// single clear round every six seconds: the round after a cut finds the window
// empty and looks clear, and an episode that ended on it would hand the next
// cut a fresh budget every time. The cuts still stop at two.
func TestAnIsolatedClearRoundDoesNotEndAnEpisode(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0

	for n := range 120 {
		delay := 30 * time.Millisecond
		if n%12 == 3 {
			delay = 0
		}

		before := c.Down.Rate
		c.Step(moving(n, 0, 300, delay))

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a queue no cut moved, clear for one round in twelve, drew %d cuts over a minute; want %d",
			cuts, c.Params.Futile)
	}
}

// latched is a controller whose download a 16 ms queue no cut moved has
// latched after two cuts, and the step after its last.
func latched(t *testing.T) (*Controller, int) {
	t.Helper()

	c := newController()

	n := 0
	for ; n < 40; n++ {
		c.Step(moving(n, 0, 300, 16*time.Millisecond))
	}

	if c.Down.futile < c.Params.Futile {
		t.Fatalf("a queue no cut moved left the download unlatched (futile %d)", c.Down.futile)
	}

	return c, n
}

// A LATCHED DIRECTION IS RAISED ONLY ONCE THE LINE HAS CLEARED, and is then no
// longer latched. The queue clears with the host still running at its rate:
// while the last rounds still hold the queue, a raise could refill it with the
// host's own traffic, and a latched direction could not be cut for it.
func TestALatchedDirectionIsRaisedOnlyOnceTheLineHasCleared(t *testing.T) {
	t.Parallel()

	c, n := latched(t)
	held := c.Down.Rate

	// The trail still holds Detect delayed rounds through this many clear ones.
	still := c.Params.Window - c.Params.Detect

	for round := 1; round <= still; round, n = round+1, n+1 {
		c.Step(moving(n, 0, c.Down.Rate, 0))

		if c.Down.Rate != held {
			t.Fatalf("clear round %d raised the latched download from %.2f to %.2f with the queue still in "+
				"the last %d rounds", round, held, c.Down.Rate, c.Params.Window)
		}
	}

	c.Step(moving(n, 0, c.Down.Rate, 0))

	if c.Down.Rate <= held || c.Down.futile != 0 {
		t.Fatalf("a cleared line left the download at %.2f (futile %d); want it raised and unlatched",
			c.Down.Rate, c.Down.futile)
	}
}

// THE OTHER DIRECTION'S CUT DOES NOT UNLATCH THIS ONE. The download is
// latched; the upload starts moving into the same queue and is cut, which
// empties the windows both directions read. The round after finds them empty
// and the delay gone, but the queue was there a moment ago: the latched
// download, running at its rate, is not raised.
func TestTheOtherDirectionsCutDoesNotRaiseALatchedOne(t *testing.T) {
	t.Parallel()

	c, n := latched(t)
	held := c.Down.Rate

	for upCut := false; !upCut; n++ {
		if n > 200 {
			t.Fatal("the upload moving into the queue was never cut")
		}

		before := c.Up.Rate
		c.Step(moving(n, 50, c.Down.Rate, 16*time.Millisecond))
		upCut = c.Up.Rate < before
	}

	c.Step(moving(n, 50, c.Down.Rate, 0))

	if c.Down.Rate != held {
		t.Fatalf("the round after the upload's cut moved the latched download from %.2f to %.2f",
			held, c.Down.Rate)
	}
}

// AN ISOLATED SPIKE DOES NOT KEEP A DIRECTION LATCHED. After the queue that
// latched it, the line runs clear but for one 16 ms round in six: below what
// detects a queue, and the download climbs back, as it did before a clear
// round had to outlast the trail.
func TestAnIsolatedSpikeDoesNotKeepADirectionLatched(t *testing.T) {
	t.Parallel()

	c, n := latched(t)
	held := c.Down.Rate

	for end := n + 60; n < end; n++ {
		delay := time.Duration(0)
		if n%6 == 5 {
			delay = 16 * time.Millisecond
		}

		c.Step(moving(n, 0, c.Down.Rate, delay))
	}

	if c.Down.Rate <= held {
		t.Fatalf("thirty seconds of a line clear but for one spike in six left the download at %.2f", c.Down.Rate)
	}
}

// A THIN TRAIL JUST AFTER A CUT DOES NOT END AN EPISODE. A 30 ms queue no cut
// moves, delayed at first in only three rounds of six, so the round after the
// first cut sees the oldest of them leave the trail: the queue has not
// cleared, it has had no time to, and the cuts still stop at two.
func TestAThinTrailJustAfterACutDoesNotEndAnEpisode(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0

	for n := range 120 {
		delay := 30 * time.Millisecond
		if n < 7 && n != 0 && n != 3 && n != 5 {
			delay = 0
		}

		before := c.Down.Rate
		c.Step(moving(n, 0, 300, delay))

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a queue no cut moved drew %d cuts over a minute; want %d", cuts, c.Params.Futile)
	}
}

// A NEW EPISODE IS JUDGED AGAINST NOTHING FROM THE LAST ONE. A 30 ms queue no
// cut moves draws two cuts; the queue clears while the host is idle; the same
// queue comes back, and draws two cuts of its own, not one measured against
// the first episode's last cut.
func TestEachEpisodeGetsItsOwnCuts(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0

	for n := range 92 {
		o := moving(n, 0, 300, 30*time.Millisecond)
		if n >= 40 && n < 52 {
			o = moving(n, 0, 1, 0)
		}

		before := c.Down.Rate
		c.Step(o)

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != 2*c.Params.Futile {
		t.Fatalf("two episodes of a queue no cut moved drew %d cuts; want %d each", cuts, c.Params.Futile)
	}
}

// A LOSS CUT THAT LESSENED THE LOSS HELPED: a cut taken on a dropping line keeps
// its tally, so the next cut, on a line still dropping but losing one probe in
// three where it lost two, counts the first as having helped.
func TestALossCutThatLessenedTheLossHelped(t *testing.T) {
	t.Parallel()

	lossy := func(n int, lost ...string) Observation {
		probes := []Probe{{Reflector: "a"}, {Reflector: "b"}, {Reflector: "c"}}
		for i := range probes {
			probes[i].Lost = slices.Contains(lost, probes[i].Reflector)
		}

		return probed(n, 5, 400, Sample{Delay: 2 * time.Millisecond, DelayKnown: true, Probes: probes})
	}

	c := newController()
	cuts := 0
	for n := range 60 {
		o := lossy(n, "a", "b")
		if cuts > 0 {
			o = lossy(n, []string{"a", "b"}[n%2])
		}

		c.Step(o)
		if c.Cause == CauseNone {
			continue
		}

		cuts++
		if cuts == 2 {
			if c.Down.futile != 0 {
				t.Errorf("a loss cut after one that halved the loss was counted futile (%d)", c.Down.futile)
			}

			return
		}
	}

	t.Fatalf("a line dropping a probe in three was cut %d times; want a second cut to judge", cuts)
}
