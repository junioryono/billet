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

// A RUN STARTS EACH DIRECTION AT ITS OWN CEILING, which is the rate CAKE is
// installed with before the first measurement.
func TestARunStartsEachDirectionAtItsCeiling(t *testing.T) {
	t.Parallel()

	c := newRunController(1000, Options{MaxUp: 400, MaxDown: 300, Params: DefaultParams()})

	if c.Up.Rate != 400 || c.Up.Max != 400 || c.Down.Rate != 300 || c.Down.Max != 300 {
		t.Fatalf("up starts at %.0f (max %.0f) and down at %.0f (max %.0f); want 400 and 300",
			c.Up.Rate, c.Up.Max, c.Down.Rate, c.Down.Max)
	}

	if c.Params != DefaultParams() {
		t.Fatal("the run's controller does not carry the options' parameters")
	}

	c = newRunController(1000, Options{Params: DefaultParams()})
	if c.Up.Rate != 1000 || c.Down.Rate != 1000 {
		t.Fatalf("with no ceiling a run starts at %.0f up and %.0f down; want the interface's 1000",
			c.Up.Rate, c.Down.Rate)
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
