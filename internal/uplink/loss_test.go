package uplink

import (
	"slices"
	"testing"
	"time"
)

// of3 is a round's sample with three reflectors counted, these of them lost,
// and the delay the others measured; known is false when too few answered.
func of3(delay time.Duration, known bool, lost ...string) Sample {
	s := Sample{Delay: delay, DelayKnown: known}

	for _, r := range []string{"a", "b", "c"} {
		s.Probes = append(s.Probes, Probe{Reflector: r, Lost: slices.Contains(lost, r)})
	}

	return s
}

// A LINE THAT DROPS RATHER THAN QUEUES IS SEEN BY ITS LOSS. The round trip
// barely rises, but one probe in three is lost round after round, across the
// reflectors: a busy direction is cut, as deeply as a cut goes, and the cut
// says it was taken on loss.
func TestLossAcrossTheReflectorsCutsABusyDirection(t *testing.T) {
	t.Parallel()

	c := newController()

	c.Step(probed(0, 5, 400, of3(time.Millisecond, true)))

	if c.Step(probed(1, 5, 400, of3(time.Millisecond, true, "a"))) {
		t.Fatal("one lost probe cut a rate; one stray drop must change nothing")
	}

	if !c.Step(probed(2, 5, 400, of3(time.Millisecond, true, "b"))) {
		t.Fatal("probes lost in two rounds from two of three reflectors, with the host moving 400 Mbit/s " +
			"down, cut nothing")
	}

	if c.Cause != CauseLoss {
		t.Fatalf("the cut was taken on %v; want loss", c.Cause)
	}

	if want := 400 * c.Params.DeepCut; c.Down.Rate != want {
		t.Fatalf("the download was cut to %.1f; a cut on loss goes to DeepCut of what flowed, %.1f",
			c.Down.Rate, want)
	}

	if c.Up.Rate != c.Up.Max {
		t.Fatalf("the 5 Mbit/s upload was cut to %.1f; it filled nothing", c.Up.Rate)
	}
}

// ONE REFLECTOR THAT STOPPED ANSWERING IS NOT A DROPPING LINE: its probe is
// lost every round while the other two answer at their baselines, and a busy
// direction running at its rate is not cut, and still rises.
func TestOneReflectorLostEveryRoundIsNotADroppingLine(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Down.Rate = 300

	for n := range 12 {
		c.Step(probed(n, 0, c.Down.Rate, of3(time.Millisecond, true, "c")))

		if c.Down.Rate < 300 {
			t.Fatalf("step %d: one silent reflector cut the download to %.1f", n, c.Down.Rate)
		}
	}

	if c.Down.Rate <= 300 {
		t.Fatal("a direction at its rate with one reflector silent and no queue did not rise from 300")
	}
}

// ONE ROUND THAT LOST EVERYTHING IS A BLIP, not a dropping line: loss must
// persist past one round before it cuts.
func TestOneRoundOfTotalLossIsNotADroppingLine(t *testing.T) {
	t.Parallel()

	c := newController()

	c.Step(probed(0, 0, 400, of3(time.Millisecond, true)))

	if c.Step(probed(1, 0, 400, of3(0, false, "a", "b", "c"))) {
		t.Fatalf("one round with every probe lost cut the download to %.1f", c.Down.Rate)
	}

	if !c.Step(probed(2, 0, 400, of3(0, false, "a", "b", "c"))) || c.Cause != CauseLoss {
		t.Fatalf("a second round with every probe lost cut nothing (cause %v)", c.Cause)
	}
}

// LOSS WHILE THIS HOST IS QUIET IS SOMEBODY ELSE'S: the line dropping under
// another machine's traffic does not cut a host moving a trickle.
func TestLossWhileTheHostCarriesATrickleCutsNothing(t *testing.T) {
	t.Parallel()

	c := newController()

	for n := range 12 {
		lost := []string{"a", "b", "c"}[n%3]

		if c.Step(probed(n, 1, 8, of3(time.Millisecond, true, lost))) {
			t.Fatalf("step %d: loss with the host moving 8 Mbit/s cut it to %.1f/%.1f", n, c.Up.Rate, c.Down.Rate)
		}
	}
}

// A ROUND THAT MEASURED NOTHING NEITHER CUTS NOR RAISES, with the delay window
// already holding delays and a direction running at its rate: it is a
// could-not-tell, neither a queue nor its absence. Nor does a round whose delay
// was not measured raise a rate.
func TestARoundThatMeasuredNothingNeitherCutsNorRaises(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Down.Rate = 300

	for n := range 2 {
		c.Step(moving(n, 0, 300, 30*time.Millisecond))
	}

	for n := 2; n < 40; n++ {
		if c.Step(probed(n, 0, 300, Sample{})) {
			t.Fatalf("step %d: a round that measured nothing moved the download to %.1f", n, c.Down.Rate)
		}
	}

	// One reflector answering of three, lost from a single reflector pair that
	// never spreads to a majority over the window: no delay, no dropping line.
	for n := 40; n < 80; n++ {
		c.Step(probed(n, 0, 300, of3(0, false, "a", "b")))

		if c.Down.Rate > 300 {
			t.Fatalf("step %d: a round with no measured delay raised the download to %.1f", n, c.Down.Rate)
		}
	}
}

// A CUT ON LOSS THAT LEAVES THE LOSS WHERE IT WAS COUNTS AS FUTILE, as a cut on
// delay does: the line is dropping under somebody else's traffic, and two such
// cuts are all the host takes until the line clears.
func TestCutsThatLeaveTheLossAloneStop(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0
	names := []string{"a", "b", "c"}

	for n := range 120 {
		before := c.Down.Rate

		// Two of three lost every round: no delay is measured, and the loss never
		// falls.
		c.Step(probed(n, 0, 300, of3(0, false, names[n%3], names[(n+1)%3])))

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a loss that never moved drew %d cuts over a minute; want %d before they stop", cuts, c.Params.Futile)
	}
}

// A LOSS THE LINE HAS STOPPED IS NOT CUT FOR. Probes were lost while the host
// was quiet, and the window still holds them when its traffic starts, but this
// round lost nothing: the drops were somebody else's and are over.
func TestALossTheLineHasStoppedIsNotCutFor(t *testing.T) {
	t.Parallel()

	c := newController()

	c.Step(probed(0, 0, 8, of3(time.Millisecond, true)))
	c.Step(probed(1, 0, 8, of3(time.Millisecond, true, "a")))
	c.Step(probed(2, 0, 8, of3(time.Millisecond, true, "b")))

	if c.Step(probed(3, 0, 400, of3(time.Millisecond, true))) {
		t.Fatalf("a round that lost nothing cut the download to %.1f for losses the window still held",
			c.Down.Rate)
	}
}

// A CUT FORGETS WHAT CAUSED IT, NOT THAT THE LINE IS STILL DROPPING. One probe
// in three lost every round, from a different reflector each time, with the
// answers at their baselines: the round after a cut has only one lossy round
// since it, and must still not read as a clear line, or it would end the
// futile episode and the cuts would never stop.
func TestCutsThatLeaveARotatingLossAloneStop(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0
	names := []string{"a", "b", "c"}

	for n := range 120 {
		before := c.Down.Rate

		c.Step(probed(n, 0, 300, of3(time.Millisecond, true, names[n%3])))

		if c.Down.Rate < before {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a loss that never moved drew %d cuts over a minute; want %d before they stop", cuts, c.Params.Futile)
	}
}

// WHAT WAS SEEN BEFORE AN OUTAGE IS NOT EVIDENCE AFTER IT. Two reflectors
// missed a probe each, and two rounds came back delayed, while the host was
// quiet; then for five seconds no reflector counted. A single miss and a
// single delayed round as the host gets busy are one round's evidence, not a
// window's.
func TestEvidenceFromBeforeAnOutageExpires(t *testing.T) {
	t.Parallel()

	for _, after := range []Sample{of3(time.Millisecond, true, "a"), of3(30*time.Millisecond, true)} {
		c := newController()

		c.Step(probed(0, 0, 8, of3(time.Millisecond, true)))
		c.Step(probed(1, 0, 8, of3(30*time.Millisecond, true, "a")))
		c.Step(probed(2, 0, 8, of3(30*time.Millisecond, true, "b")))

		for n := 3; n < 13; n++ {
			c.Step(probed(n, 0, 8, Sample{}))
		}

		if c.Step(probed(13, 0, 400, after)) {
			t.Fatalf("one round of %+v after the outage cut the download to %.1f, on evidence from before it",
				after, c.Down.Rate)
		}
	}
}

// A REFLECTOR LEAVING IS NOT A CUT HELPING. One reflector lost every probe and
// stopped counting soon after the first cut, while the other two kept losing
// one probe a round between them: the share over every reflector the window
// asked fell, but the loss of the ones still asked did not, and the cuts still
// stop at two.
func TestAReflectorLeavingDoesNotReadAsACutHelping(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0
	firstCut := -1

	for n := range 120 {
		lost := []string{"b", "c"}[n%2]

		// a is still counted, and losing, for two rounds after the first cut.
		var s Sample
		if firstCut < 0 || n <= firstCut+2 {
			s = of3(0, false, "a", lost)
		} else {
			s = Sample{Probes: []Probe{{Reflector: "b", Lost: lost == "b"}, {Reflector: "c", Lost: lost == "c"}}}
		}

		before := c.Down.Rate
		c.Step(probed(n, 0, 300, s))

		if c.Down.Rate < before {
			cuts++

			if firstCut < 0 {
				firstCut = n
			}
		}
	}

	if cuts != c.Params.Futile {
		t.Fatalf("a loss the remaining reflectors never shed drew %d cuts over a minute; want %d", cuts, c.Params.Futile)
	}
}

// A REFLECTOR THAT HAS STOPPED COUNTING LENDS THE WINDOW NONE OF ITS MISSES.
// Two reflectors went silent while the host was quiet and are no longer
// counted; the one left misses a single probe as the host gets busy, which is
// one lossy round, not a dropping line.
func TestABenchedReflectorsMissesDoNotMakeALoss(t *testing.T) {
	t.Parallel()

	c := newController()

	c.Step(probed(0, 0, 8, of3(time.Millisecond, true)))

	for n := 1; n < 5; n++ {
		c.Step(probed(n, 0, 8, of3(0, false, "a", "b")))
	}

	alone := Sample{Probes: []Probe{{Reflector: "c", Lost: true}}}
	if c.Step(probed(5, 0, 400, alone)) {
		t.Fatalf("one miss from the only counted reflector cut the download to %.1f, on misses of reflectors "+
			"no longer counted", c.Down.Rate)
	}
}
