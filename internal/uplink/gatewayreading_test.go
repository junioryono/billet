package uplink

import (
	"testing"
	"time"
)

// gatewayProber is three idle reflectors and a gateway whose round trip each
// round rtt gives, absent when it returns zero.
func gatewayProber(rtt func(round int) time.Duration) *fakeProber {
	return &fakeProber{reflectors: three, gateway: "192.168.1.254", rtt: func(round int) map[string]time.Duration {
		answers := idle()
		if d := rtt(round); d > 0 {
			answers["192.168.1.254"] = d
		}

		return answers
	}}
}

// A STALL WITH QUIET REPLIES BETWEEN NEVER READS AS CLEARED: the trail that
// decides whether a queue has gone takes every high reading, so the futility
// latch holds against somebody else's stall rather than being reopened by one
// quiet reply. Through the stepper, as the raw readings arrive.
func TestAStallWithQuietRepliesBetweenStillLatches(t *testing.T) {
	t.Parallel()

	// THREE HIGH, TWO QUIET: every window of six that ends on a quiet reply
	// holds three high readings, two of them confirmed. Counting only confirmed
	// stalls in the trail would read each of those windows as cleared.
	r := newRig(t, gatewayProber(func(round int) time.Duration {
		if round == 0 || round%5 >= 3 {
			return time.Millisecond
		}

		return 60 * time.Millisecond
	}), 400)
	r.run(600)

	if cuts := len(r.cuts()); cuts == 0 || cuts > r.controller.Params.Futile+1 {
		t.Errorf("a stall no cut shortened, high three rounds in five, was cut %d times; want a cut, and the "+
			"latch holding after %d", cuts, r.controller.Params.Futile)
	}
}

// A CONFIRMED STALL RARER THAN THE DETECTION WINDOW CUTS NOTHING: two high
// readings in a row once in six rounds, and a lone one beside them, are not a
// queue, however high.
func TestARareConfirmedStallCutsNothing(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch round % 6 {
		case 1, 2, 4:
			if round > 0 {
				return 80 * time.Millisecond
			}
		}

		return time.Millisecond
	}), 400)
	r.run(120)

	if cuts := len(r.cuts()); cuts != 0 {
		t.Errorf("a gateway confirmed stalled one round in six was cut %d times", cuts)
	}
}

// ONE QUIET READING CLEARS A STALL: a gateway that answers quietly once after
// its stall and then not at all, still counted, holds nothing up.
func TestOneQuietReadingClearsAStall(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch {
		case round == 0 || round == 20:
			return time.Millisecond
		case round < 20:
			return 60 * time.Millisecond
		}

		return 0
	}), 49)
	r.controller.Down = NewDirection(100)
	r.controller.Down.Rate = 50

	r.run(21)
	held := r.controller.Down.Rate

	// FEWER THAN silentAfter ROUNDS, so the gateway is still counted.
	r.run(silentAfter - 5)
	if r.controller.Down.Rate <= held {
		t.Errorf("after one quiet reading the stall still held a full download at %.1f", r.controller.Down.Rate)
	}
}

// A GATEWAY THAT RECOVERS WHILE ANSWERING EVERY OTHER PROBE STOPS HOLDING RATES:
// one quiet reading clears its stall, so the rounds it gives no reading in no
// longer read as a stall gone unread.
func TestAGatewayThatRecoversWhileAnsweringHalfLetsRatesClimb(t *testing.T) {
	t.Parallel()

	// A QUIET FIRST READING, which is the gateway's baseline.
	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch {
		case round == 0:
			return time.Millisecond
		case round < 30:
			return 60 * time.Millisecond
		case round%2 == 0:
			return 0
		}

		return time.Millisecond
	}), 49)
	r.controller.Down = NewDirection(100)
	r.controller.Down.Rate = 50

	r.run(30)
	held := r.controller.Down.Rate

	r.run(30)
	if r.controller.Down.Rate <= held {
		t.Errorf("after its stall the gateway answered quietly every other round, and a full download stayed "+
			"at %.1f", r.controller.Down.Rate)
	}
}

// A GATEWAY THAT ANSWERS ONCE A SECOND STILL CONFIRMS A STALL: two readings
// within two seconds pair, so one that never answers two rounds in a row is not
// a signal lost.
func TestAGatewayThatAnswersOnceASecondConfirmsAStall(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch {
		case round == 0:
			return time.Millisecond
		case round%2 == 0:
			return 60 * time.Millisecond
		}

		return 0
	}), 400)
	r.run(30)

	if len(r.cuts()) == 0 {
		t.Error("a gateway stalled 60 ms, answering every other round, under a 400 Mbit/s download cut nothing")
	}
}

// A GATEWAY THAT GOES SILENT MID-STALL STOPS BEING COUNTED, and holds nothing
// up once it has: its stall is not remembered past its silence.
func TestAGatewaySilentMidStallHoldsNothingOnceSilent(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch {
		case round == 0:
			return time.Millisecond
		case round < 10:
			return 60 * time.Millisecond
		}

		return 0
	}), 49)
	r.controller.Down = NewDirection(100)
	r.controller.Down.Rate = 50

	r.run(10 + silentAfter + 2)
	held := r.controller.Down.Rate

	r.run(20)
	if r.controller.Down.Rate <= held {
		t.Errorf("a gateway silent for good after a stall held a full download at %.1f", r.controller.Down.Rate)
	}
}
