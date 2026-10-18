package uplink

import (
	"testing"
	"time"
)

func TestTheCeilingIsTheConfiguredMaximumBelowTheInterface(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		speed, configured, want float64
	}{
		{1000, 0, 1000},
		{1000, 400, 400},
		{1000, 1000, 1000},
		{1000, 2500, 1000},
		{1000, -5, 1000},
	} {
		if got := Ceiling(c.speed, c.configured); got != c.want {
			t.Errorf("Ceiling(%v, %v) = %v, want %v", c.speed, c.configured, got, c.want)
		}
	}
}

// A RUN WITH NOTHING LEARNED STARTS EACH DIRECTION LOW, at Start under its own
// ceiling, which is the rate CAKE is installed with before the first
// measurement: never at the interface's speed, which on 2026-10-14 let a node
// send 888 Mbit/s into a 500 Mbit/s line before anything was measured.
func TestARunStartsEachDirectionLowUnderItsCeiling(t *testing.T) {
	t.Parallel()

	p := DefaultParams()

	c := newRunController(1000, Options{MaxUp: 400, MaxDown: 300, Params: p})
	if c.Up.Rate != p.Start || c.Up.Max != 400 || c.Down.Rate != p.Start || c.Down.Max != 300 {
		t.Fatalf("up starts at %.0f (max %.0f) and down at %.0f (max %.0f); want %.0f under 400 and 300",
			c.Up.Rate, c.Up.Max, c.Down.Rate, c.Down.Max, p.Start)
	}

	if c.Params != p || c.Up.Limit != 0 || c.Down.Limit != 0 {
		t.Fatal("the run's controller does not carry the options' parameters, or starts with a line found")
	}

	c = newRunController(1000, Options{Params: p})
	if c.Up.Rate != p.Start || c.Down.Rate != p.Start || c.Up.Max != 1000 {
		t.Fatalf("with no ceiling a run starts at %.0f up and %.0f down under %.0f; want %.0f under the "+
			"interface's 1000", c.Up.Rate, c.Down.Rate, c.Up.Max, p.Start)
	}

	// A CEILING BELOW START IS THE START, and the floor holds under both: an
	// interface that reports 5 Mbit/s starts at the floor.
	c = newRunController(1000, Options{MaxUp: 15, Params: p})
	if c.Up.Rate != 15 {
		t.Fatalf("under a 15 Mbit/s ceiling the upload starts at %.1f", c.Up.Rate)
	}

	c = newRunController(5, Options{Params: p})
	if c.Up.Rate != Floor || c.Down.Rate != Floor {
		t.Fatalf("on a 5 Mbit/s interface the run starts at %.1f/%.1f, want the %.0f floor", c.Up.Rate,
			c.Down.Rate, Floor)
	}
}

// A CEILING HOLDS UNDER TRAFFIC THAT WOULD RAISE ANY RATE: a direction moving
// all it is let through, with no queue, climbs to the ceiling and stops there,
// however long it runs.
func TestARateNeverRisesPastItsCeiling(t *testing.T) {
	t.Parallel()

	c := &Controller{Params: DefaultParams(), Up: NewDirection(Ceiling(1000, 400)),
		Down: NewDirection(Ceiling(1000, 400))}
	c.Up.Rate, c.Down.Rate = 200, 200

	for n := range 600 {
		c.Step(moving(n, c.Up.Rate, c.Down.Rate, time.Millisecond))

		if c.Up.Rate > 400 || c.Down.Rate > 400 {
			t.Fatalf("at step %d the rates rose to %.1f up and %.1f down, past the 400 Mbit/s ceiling",
				n, c.Up.Rate, c.Down.Rate)
		}
	}

	if c.Up.Rate != 400 || c.Down.Rate != 400 {
		t.Fatalf("after five minutes at their rates the directions run at %.1f up and %.1f down, "+
			"want both at the 400 Mbit/s ceiling", c.Up.Rate, c.Down.Rate)
	}
}
