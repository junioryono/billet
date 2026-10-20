package uplink

import (
	"testing"
	"time"
)

// lossyRounds steps c through rounds from..to in which two of the three
// reflectors go unanswered, a different pair each round, so the line reads as
// dropping and no delay is measured, with each direction moving as given; it
// reports whether any step cut.
func lossyRounds(c *Controller, from, to int, upMbit, downMbit float64, all bool) bool {
	names := []string{"a", "b", "c"}
	cut := false

	for n := from; n < to; n++ {
		lost := []string{names[n%3], names[(n+1)%3]}
		if all {
			lost = names
		}

		c.Step(probed(n, upMbit, downMbit, of3(0, false, lost...)))
		cut = cut || c.Cause != CauseNone
	}

	return cut
}

// A LOSS THE HOST DID NOT CAUSE CUTS NO DIRECTION CARRYING A TRICKLE: the
// reference node's two such cuts (2026-10-18, #592), replayed. A download moving
// 19.4 Mbit/s under a 905 Mbit/s rate and a line found at 602, with two probes
// in three lost, was cut to 14.5; an upload moving 17.9 under about 600, with
// all three lost while 126 came down, to 13.4. Neither is a share of its line
// that could have filled it.
func TestALossTheHostDidNotCauseCutsNoTrickle(t *testing.T) {
	t.Parallel()

	down := newController()
	down.Down.Rate, down.Down.Limit = 905, 602
	if lossyRounds(down, 0, 12, 0, 19.4, false) || down.Down.Rate != 905 {
		t.Errorf("a download moving 19.4 Mbit/s under a line found at 602 was cut on loss to %.1f (cause %v)",
			down.Down.Rate, down.Cause)
	}

	up := newController()
	up.Up.Rate = 600
	up.Down.Rate = 832
	// WHETHER THE DOWNLOAD IS CUT IS THE BUSY RULE'S TO SAY, not this test's: the
	// upload is what is under test.
	_ = lossyRounds(up, 0, 12, 17.9, 126.3, true)
	if up.Up.Rate != 600 {
		t.Errorf("an upload moving 17.9 Mbit/s under a 600 Mbit/s rate was cut on loss to %.1f", up.Up.Rate)
	}
}

// AND A LOSS UNDER A DIRECTION FILLING ITS LINE STILL CUTS IT: the reference
// node's two loss cuts that found the line, replayed. 602 Mbit/s moving under
// about 1000 with no line found, and 412 under 680 with a line found at 602.
func TestALossUnderADirectionFillingItsLineStillCuts(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name                string
		rate, limit, moving float64
	}{
		{"no line found", 1000, 0, 602},
		{"a line found", 680, 602, 412},
		// THE LINE FOUND IS THE MEASURE WHEN IT IS LOWER THAN THE RATE: 200 is a
		// quarter of the 602 found, though not of the 905 the rate stands at.
		{"a rate above the line found", 905, 602, 200},
	} {
		ctl := newController()
		ctl.Down.Rate, ctl.Down.Limit = c.rate, c.limit

		if !lossyRounds(ctl, 0, 6, 1, c.moving, false) || ctl.Down.Rate >= min(c.rate, c.moving) {
			t.Errorf("%s: a download moving %.0f Mbit/s under %.0f through a dropping line was not cut (rate %.1f)",
				c.name, c.moving, c.rate, ctl.Down.Rate)
		}
	}
}

// A LOSS WITH A RISING ROUND TRIP IS CUT FOR BY THE DELAY RULE, whatever share of
// its line the direction moves: the loss share governs a loss alone.
func TestALossWithADelayIsNotJudgedByTheLossShare(t *testing.T) {
	t.Parallel()

	c := newController()
	c.Down.Rate, c.Down.Limit = 905, 602

	for n := range 4 {
		c.Step(probed(n, 0, 60, of3(40*time.Millisecond, true, []string{"a", "b", "c"}[n%3])))
	}

	if c.Down.Rate >= 60 {
		t.Errorf("a download moving 60 Mbit/s under a delayed, lossy line was left at %.1f; the loss share "+
			"applies to a loss alone", c.Down.Rate)
	}
}
