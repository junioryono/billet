package uplink

import (
	"math"
	"testing"
	"time"
)

// THE DELAY IS THE MEDIAN RISE OVER EACH REFLECTOR'S OWN BASELINE, so reflectors
// at different distances agree on a queue they all sit behind, and one that
// vanishes or slows alone moves nothing.
func TestTheDelayIsTheMedianRiseOverEachReflectorsBaseline(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"near", "mid", "far"}

	if _, known := d.Observe(asked, map[string]time.Duration{}); known {
		t.Fatal("a delay was reported before any reflector had a baseline")
	}

	d.Observe(asked, map[string]time.Duration{"near": 7 * time.Millisecond, "mid": 15 * time.Millisecond,
		"far": 40 * time.Millisecond})

	delay, known := d.Observe(asked, map[string]time.Duration{"near": 27 * time.Millisecond,
		"mid": 35 * time.Millisecond, "far": 60 * time.Millisecond})
	if !known || delay < 19*time.Millisecond || delay > 20*time.Millisecond {
		t.Fatalf("every reflector 20 ms over its baseline read as %v (known %v)", delay, known)
	}

	delay, _ = d.Observe(asked, map[string]time.Duration{"near": 7 * time.Millisecond,
		"mid": 15 * time.Millisecond})
	if delay > time.Millisecond {
		t.Fatalf("one reflector lost with the others at baseline read as %v; one must not move the median", delay)
	}

	delay, _ = d.Observe(asked, map[string]time.Duration{"near": 7 * time.Millisecond})
	if delay != time.Duration(math.MaxInt64) {
		t.Fatalf("two of three lost read as %v; a full queue drops, and a majority lost is a full queue", delay)
	}
}

// HALF THE REFLECTORS UNREACHABLE IS NOT A FULL QUEUE: one operator's two
// addresses failing together must not read as the line's queue overflowing.
func TestHalfTheReflectorsLostIsNotAQueue(t *testing.T) {
	t.Parallel()

	var d Delays

	asked := []string{"a1", "a2", "b", "c"}
	idle := map[string]time.Duration{"a1": 7 * time.Millisecond, "a2": 8 * time.Millisecond,
		"b": 12 * time.Millisecond, "c": 20 * time.Millisecond}
	d.Observe(asked, idle)

	delay, known := d.Observe(asked, map[string]time.Duration{"b": 12 * time.Millisecond, "c": 20 * time.Millisecond})
	if !known || delay > time.Millisecond {
		t.Fatalf("two of four lost with the others at baseline read as %v (known %v)", delay, known)
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
