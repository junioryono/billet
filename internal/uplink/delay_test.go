package uplink

import (
	"slices"
	"testing"
	"time"
)

// THE DELAY IS THE RISE A MAJORITY OF REFLECTORS REACH OVER THEIR OWN BASELINES,
// so reflectors at different distances agree on a queue they all sit behind,
// and one that vanishes or slows alone moves nothing.
func TestTheDelayIsTheRiseAMajorityOfReflectorsReach(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"near", "mid", "far"}

	if s := d.Observe(asked, map[string]time.Duration{}); s.DelayKnown || len(s.Probes) != 0 {
		t.Fatalf("a round before any reflector had a baseline measured %+v; it can tell nothing", s)
	}

	d.Observe(asked, map[string]time.Duration{"near": 7 * time.Millisecond, "mid": 15 * time.Millisecond,
		"far": 40 * time.Millisecond})

	s := d.Observe(asked, map[string]time.Duration{"near": 27 * time.Millisecond,
		"mid": 35 * time.Millisecond, "far": 60 * time.Millisecond})
	if !s.DelayKnown || s.Delay < 19*time.Millisecond || s.Delay > 20*time.Millisecond || s.Lost() != 0 {
		t.Fatalf("every reflector 20 ms over its baseline read as %+v", s)
	}

	s = d.Observe(asked, map[string]time.Duration{"near": 7 * time.Millisecond, "mid": 15 * time.Millisecond})
	if !s.DelayKnown || s.Delay > time.Millisecond || s.Lost() != 1 {
		t.Fatalf("one reflector lost with the others at baseline read as %+v; one must not move the delay", s)
	}

	s = d.Observe(asked, map[string]time.Duration{"near": 47 * time.Millisecond, "mid": 15 * time.Millisecond})
	if !s.DelayKnown || s.Delay > time.Millisecond {
		t.Fatalf("one of two answers 40 ms high, with the third lost, read as %+v; a delay needs a majority "+
			"of the three to see it", s)
	}
}

// A LOST PROBE IS A LOSS, NEVER A DELAY. Two of three reflectors unanswered
// leave no majority to measure a delay with, and the round says so, rather than
// reading the missing answers as an unbounded round trip (2026-10-15: cuts
// logged with `delay=2562047h47m16.854775807s`, math.MaxInt64).
func TestALostProbeIsALossAndNeverADelay(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"a", "b", "c"}
	idle := map[string]time.Duration{"a": 7 * time.Millisecond, "b": 9 * time.Millisecond, "c": 12 * time.Millisecond}
	d.Observe(asked, idle)

	s := d.Observe(asked, map[string]time.Duration{"a": 7 * time.Millisecond})
	if s.DelayKnown {
		t.Fatalf("two of three lost read as a measured delay of %v", s.Delay)
	}

	if s.Lost() != 2 || len(s.Probes) != 3 {
		t.Fatalf("two of three lost counted %d lost of %d probes; want 2 of 3", s.Lost(), len(s.Probes))
	}

	s = d.Observe(asked, map[string]time.Duration{})
	if s.DelayKnown || s.Lost() != 3 {
		t.Fatalf("a round with every reflector lost read as %+v; want every probe lost and no delay", s)
	}
}

// HALF THE REFLECTORS UNREACHABLE IS NOT A MEASUREMENT: one operator's two
// addresses failing together leave no majority, so the delay is not known, and
// is not read as clear either.
func TestHalfTheReflectorsLostMeasuresNoDelay(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"a1", "a2", "b", "c"}
	idle := map[string]time.Duration{"a1": 7 * time.Millisecond, "a2": 8 * time.Millisecond,
		"b": 12 * time.Millisecond, "c": 20 * time.Millisecond}
	d.Observe(asked, idle)

	s := d.Observe(asked, map[string]time.Duration{"b": 12 * time.Millisecond, "c": 20 * time.Millisecond})
	if s.DelayKnown || s.Lost() != 2 || len(s.Probes) != 4 {
		t.Fatalf("two of four lost read as %+v; want no delay and 2 of 4 lost", s)
	}

	s = d.Observe(asked, map[string]time.Duration{"a1": 7 * time.Millisecond, "b": 32 * time.Millisecond,
		"c": 40 * time.Millisecond})
	if !s.DelayKnown || s.Delay > time.Millisecond {
		t.Fatalf("three of four answering, one at baseline, read as %+v; three of four must see a delay", s)
	}
}

// A REFLECTOR THAT HAS GONE SILENT STOPS BEING COUNTED, after the ten seconds
// cake-autorate gives a reflector, and is counted again the round it answers:
// one that stopped answering ICMP for good is not a line losing a third of
// its packets forever.
func TestASilentReflectorStopsBeingCountedUntilItAnswers(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"a", "b", "c"}
	all := map[string]time.Duration{"a": 7 * time.Millisecond, "b": 9 * time.Millisecond, "c": 12 * time.Millisecond}
	twoOfThree := map[string]time.Duration{"a": 7 * time.Millisecond, "b": 9 * time.Millisecond}
	d.Observe(asked, all)

	for n := range silentAfter {
		s := d.Observe(asked, twoOfThree)
		if s.Lost() != 1 || len(s.Probes) != 3 {
			t.Fatalf("round %d of c's silence counted %d lost of %d; a reflector is given %d rounds", n+1,
				s.Lost(), len(s.Probes), silentAfter)
		}
	}

	s := d.Observe(asked, twoOfThree)
	if s.Lost() != 0 || len(s.Probes) != 2 || slices.ContainsFunc(s.Probes, func(p Probe) bool {
		return p.Reflector == "c"
	}) {
		t.Fatalf("after %d silent rounds c is still counted: %+v", silentAfter, s.Probes)
	}

	if s = d.Observe(asked, all); len(s.Probes) != 3 || s.Lost() != 0 {
		t.Fatalf("c answered again and is not counted: %+v", s.Probes)
	}
}

// A BASELINE FOLLOWS A LONGER ROUTE SLOWLY AND A SHORTER ONE AT ONCE.
func TestABaselineRisesSlowlyAndFallsAtOnce(t *testing.T) {
	t.Parallel()

	var d Delays

	one := []string{"r"}
	d.Observe(one, map[string]time.Duration{"r": 10 * time.Millisecond})

	for range 10 {
		d.Observe(one, map[string]time.Duration{"r": 40 * time.Millisecond})
	}

	if base := d.baseline["r"]; base > 11*time.Millisecond {
		t.Fatalf("five seconds of a full queue lifted the baseline to %v; it would hide the queue", base)
	}

	d.Observe(one, map[string]time.Duration{"r": 6 * time.Millisecond})

	if base := d.baseline["r"]; base != 6*time.Millisecond {
		t.Fatalf("a faster round trip left the baseline at %v", base)
	}
}

// A REFLECTOR IS ASKED IN THE SPELLING ITS REPLY COMES FROM, once: a reply is
// matched by the source address's dotted quad, so `::ffff:1.1.1.1` would never
// be answered, and a reflector named twice would be counted twice.
func TestAReflectorIsAskedInTheSpellingItsReplyComesFrom(t *testing.T) {
	t.Parallel()

	got, err := reflectorAddrs([]string{"::ffff:1.1.1.1", "8.8.8.8", "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"1.1.1.1", "8.8.8.8"}; !slices.Equal(got, want) {
		t.Fatalf("reflectors %v, want %v", got, want)
	}

	for _, bad := range [][]string{{"2606:4700::1111"}, {"one.one.one.one"}, {" 1.1.1.1"}, {}} {
		if _, err := reflectorAddrs(bad); err == nil {
			t.Errorf("reflectors %q were accepted", bad)
		}
	}
}
