package uplink

import (
	"slices"
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

	if cuts := len(r.cuts()); cuts != r.controller.Params.Futile {
		t.Errorf("a stall no cut shortened, high three rounds in five, was cut %d times; want exactly %d, "+
			"then the latch", cuts, r.controller.Params.Futile)
	}
}

// A CONFIRMED STALL RARER THAN THE DETECTION WINDOW CUTS NOTHING: two high
// readings in a row once in six rounds are not a queue, however high, as the
// reflectors' delay two rounds in six is not.
func TestARareConfirmedStallCutsNothing(t *testing.T) {
	t.Parallel()

	// AND AFTER A FEW UNANSWERED ROUNDS, which a stall confirmed long after them
	// does not span.
	for _, unanswered := range []int{0, 4} {
		r := newRig(t, gatewayProber(func(round int) time.Duration {
			if round > 0 && round <= unanswered {
				return 0
			}

			switch round % 6 {
			case 1, 2:
				if round > 0 {
					return 80 * time.Millisecond
				}
			}

			return time.Millisecond
		}), 400)
		r.run(120)

		if cuts := len(r.cuts()); cuts != 0 {
			t.Errorf("after %d unanswered rounds, a gateway confirmed stalled one round in six was cut %d times",
				unanswered, cuts)
		}
	}
}

// A QUIET READING CLEARS A STALL UNLESS A HIGH ONE STANDS BESIDE IT: a gateway
// that answers quietly after its stall and then not at all, still counted,
// holds nothing up once two quiet readings, or one with no stall within
// gatewayPairRounds before it, say the stall has gone; one reply slipping
// through a stall says nothing of it.
func TestAQuietReadingClearsAStallUnlessAHighOneStandsBesideIt(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		quiet []int
		last  int
		clear bool
	}{
		"one quiet reading straight after the stall":                            {[]int{20}, 20, false},
		"two quiet readings":                                                    {[]int{20, 21}, 21, true},
		"one quiet reading after the stall went unread past the pairing window": {[]int{25}, 25, true},
	} {
		r := newRig(t, gatewayProber(func(round int) time.Duration {
			switch {
			case round == 0 || slices.Contains(tc.quiet, round):
				return time.Millisecond
			case round < 20:
				return 60 * time.Millisecond
			}

			return 0
		}), 49)
		r.controller.Down = NewDirection(100)
		r.controller.Down.Rate = 50

		r.run(tc.last + 1)
		held := r.controller.Down.Rate

		// FEWER THAN silentAfter ROUNDS SINCE THE LAST READING, so the gateway is
		// still counted.
		r.run(silentAfter - 5)
		if cleared := r.controller.Down.Rate > held; cleared != tc.clear {
			t.Errorf("%s: a full download went from %.1f to %.1f; want the stall cleared %v", name, held,
				r.controller.Down.Rate, tc.clear)
		}
	}
}

// A GATEWAY THAT RECOVERS WHILE ANSWERING EVERY OTHER PROBE STOPS HOLDING RATES:
// its second quiet reading, two rounds after the first, is a calm pair that
// clears its stall, so the rounds it gives no reading in no longer read as a
// stall gone unread.
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

// A CONFIRMED STALL THAT A CUT SHORTENED HELPED, judged like against like: the
// gateway wandering 50 to 70 ms after a cut taken at 80, and then 30 to 50 ms,
// is shorter by its confirmed reading each time, and the cuts go on. Judged by
// its raw reading, whichever cut the cooldown lets fall on a 70 or a 50 would
// read as futile, and the latch would hold the stall.
func TestAConfirmedStallACutShortenedHelped(t *testing.T) {
	t.Parallel()

	for _, phase := range []int{0, 1} {
		c := newController()
		cuts := 0
		wander := func(n int, low, high time.Duration) time.Duration {
			if (n+phase)%2 == 0 {
				return low
			}

			return high
		}

		for n := range 120 {
			rise := 80 * time.Millisecond
			switch {
			case cuts == 1:
				rise = wander(n, 50*time.Millisecond, 70*time.Millisecond)
			case cuts >= 2:
				rise = wander(n, 30*time.Millisecond, 50*time.Millisecond)
			}

			c.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(rise)))
			if c.Cause == CauseNone {
				// THE STALL A CUT WAS TAKEN BESIDE IS ITS STEP'S ALONE, for its log.
				if c.cutStall.known {
					t.Fatalf("phase %d, round %d: a step that cut nothing still names a stall, %v", phase, n,
						c.cutStall.delay)
				}

				continue
			}

			if c.cutStall != c.Down.cutGateway || !c.cutStall.known {
				t.Errorf("phase %d: the cut names the stall %+v, not the %+v it kept", phase, c.cutStall,
					c.Down.cutGateway)
			}

			cuts++
			if cuts <= 3 && c.Down.futile != 0 {
				t.Errorf("phase %d: cut %d, on a stall the cut before had shortened by more than a fifth, "+
					"was counted futile (%d)", phase, cuts, c.Down.futile)
			}
		}

		if cuts < 3 {
			t.Errorf("phase %d: a stall each cut shortened by more than a fifth was cut %d times, want three",
				phase, cuts)
		}
	}
}

// A STALL CONFIRMS ONLY ACROSS TWO HIGH READINGS AT MOST gatewayPairRounds
// APART: one further is a fresh first reading, so a gateway that stalls, falls
// silent and comes back stalled needs a second reading before it counts toward
// a cut. Rounds, as the controller steps them, not timestamps.
func TestAStallConfirmsOnlyWithinThePairingWindow(t *testing.T) {
	t.Parallel()

	stalled := func(n int) Observation {
		return withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(60*time.Millisecond))
	}
	unread := func(n int) Observation { return withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayLost()) }

	for name, tc := range map[string]struct {
		gap  int
		want bool
	}{
		"at the window":     {gatewayPairRounds, true},
		"one round past it": {gatewayPairRounds + 1, false},
	} {
		c := newController()
		c.Step(stalled(0))
		for n := 1; n < tc.gap; n++ {
			c.Step(unread(n))
		}
		c.Step(stalled(tc.gap))

		if got := c.recent[len(c.recent)-1]; got != tc.want {
			t.Errorf("%s: a stall %d rounds after the last confirmed %v, want %v", name, tc.gap, got, tc.want)
		}
	}

	// ROUNDS, NOT THE CLOCK: four rounds apart pair though the last round's
	// probes came in 400 ms after its tick, 2.4 s after the first reading.
	c := newController()
	c.Step(stalled(0))
	for n := 1; n < gatewayPairRounds; n++ {
		c.Step(unread(n))
	}
	late := stalled(gatewayPairRounds)
	late.At = late.At.Add(Tick * 8 / 10)
	c.Step(late)
	if !c.recent[len(c.recent)-1] {
		t.Errorf("a stall four rounds after the last, %v by the clock, was not confirmed", late.At.Sub(at(0)))
	}

	// A REPLY SLIPPED BETWEEN TWO HIGH READINGS DOES NOT BREAK THEIR PAIR.
	c = newController()
	quiet := func(n int) Observation {
		return withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(time.Millisecond))
	}
	for _, o := range []Observation{stalled(0), quiet(1), stalled(2)} {
		c.Step(o)
	}
	if !c.recent[len(c.recent)-1] || c.recent[len(c.recent)-2] {
		t.Errorf("a stall read either side of one quiet reply was confirmed %v, the reply delayed %v; want a "+
			"stall and a reply that is not", c.recent[len(c.recent)-1], c.recent[len(c.recent)-2])
	}

	// THROUGH THE CONTROLLER: a stall, silence past the window, and then high
	// readings in a row, which pair with none before the silence: two are under
	// the Detect of three a cut needs, and the third cuts, as the reflectors'
	// delay in three rounds in a row would.
	c = newController()
	c.Step(stalled(0))

	n := 1
	for ; n <= gatewayPairRounds; n++ {
		c.Step(unread(n))
	}

	for range 2 {
		c.Step(stalled(n))
		if c.Cause != CauseNone {
			t.Fatalf("round %d: the gateway was cut on, back from a silence past the pairing window with "+
				"two readings", n)
		}
		n++
	}

	c.Step(stalled(n))
	if c.Cause == CauseNone {
		t.Error("a third stalled reading in a row cut nothing")
	}
}

// A STALL HIGH ONE SECOND IN TWO CUTS: the first reading of each pair in a row
// counts once the second confirms it, so two high readings, two quiet ones and
// two high again fill a window as the reflectors' delay in the same rounds
// would. The quiet pair between ends each stall, so counting only what confirms
// would never fill one.
func TestAStallHighOneSecondInTwoCuts(t *testing.T) {
	t.Parallel()

	c := newController()
	for n := range 30 {
		rise := 60 * time.Millisecond
		if n%4 >= 2 {
			rise = time.Millisecond
		}

		c.Step(withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(rise)))
		if c.Cause != CauseNone {
			return
		}
	}

	t.Error("a gateway stalled 60 ms one second in two, under a 400 Mbit/s download, cut nothing")
}

// SPIKES WITH A QUIET PAIR BETWEEN ARE NOT ONE STALL: a calm pair is the stall
// gone, so a spike three rounds after the last is a lone one again, and beside
// the reflectors delayed one round in six, under the three a cut needs, tips
// nothing. Paired across the calm rounds, every spike would confirm.
func TestSpikesWithAQuietPairBetweenAreNotOneStall(t *testing.T) {
	t.Parallel()

	c := newController()
	for n := range 120 {
		delay, rise := 2*time.Millisecond, time.Millisecond
		if n%6 == 1 {
			delay = 30 * time.Millisecond
		}
		if n > 0 && n%3 == 0 {
			rise = 80 * time.Millisecond
		}

		c.Step(withGateway(moving(n, 5, 400, delay), gatewayRose(rise)))
		if c.Cause != CauseNone {
			t.Fatalf("round %d: a cut on gateway spikes three rounds apart with quiet pairs between", n)
		}
	}
}

// A STRAY LOST PROBE AT A CUT CREDITS NO LATER CUT: a window that was not
// dropping keeps no tally, so a stall no cut shortens latches after Futile cuts
// though the window at the first cut lost one probe and none since did.
func TestAStrayLostProbeAtACutCreditsNoLaterCut(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0

	for n := range 120 {
		o := withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(60*time.Millisecond))
		if n == 1 {
			o.Probes = []Probe{{Reflector: "a", Lost: true}, {Reflector: "b"}, {Reflector: "c"}}
		}

		c.Step(o)
		if c.Cause != CauseNone {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Errorf("a stall no cut shortened, one probe lost before the first cut, was cut %d times; want "+
			"exactly %d, then the latch", cuts, c.Params.Futile)
	}
}

// A STALL WITH A QUIET REPLY IN THREE CUTS: its high readings pair across the
// reply, so two in three confirmed fill the window as the reflectors' delay in
// two rounds of three would. Paired only with the reading before, nothing would
// ever confirm but one round in three.
func TestAStallWithAQuietReplyInThreeCuts(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		if round == 0 || round%3 == 0 {
			return time.Millisecond
		}

		return 60 * time.Millisecond
	}), 400)
	r.run(30)

	if len(r.cuts()) == 0 {
		t.Error("a gateway stalled 60 ms two rounds in three, under a 400 Mbit/s download, cut nothing")
	}
}

// A STALLED GATEWAY THAT ANSWERS ONE PROBE IN THREE STILL CUTS: its readings
// are three rounds apart, within gatewayPairRounds, and each confirmed stall
// spans one of the rounds it went unread, so its third reading fills the window.
func TestAStalledGatewayAnsweringOneProbeInThreeCuts(t *testing.T) {
	t.Parallel()

	r := newRig(t, gatewayProber(func(round int) time.Duration {
		switch {
		case round == 0:
			return time.Millisecond
		case round%3 == 0:
			return 60 * time.Millisecond
		}

		return 0
	}), 400)
	r.run(30)

	if len(r.cuts()) == 0 {
		t.Error("a gateway stalled 60 ms, answering one round in three, under a 400 Mbit/s download cut nothing")
	}
}

// AND ITS QUIET REPLIES DO NOT END THE EPISODE: the trail spans the unread
// rounds too, and a quiet reply slipping through the stall is not a clear round,
// so a stall answering one probe in three, or in four, one reading in four of
// them quiet, still reads as a queue the latch holds against.
func TestASparseStallWithQuietRepliesStillLatches(t *testing.T) {
	t.Parallel()

	for _, every := range []int{3, 4} {
		r := newRig(t, gatewayProber(func(round int) time.Duration {
			switch {
			case round == 0 || round%(4*every) == 0:
				return time.Millisecond
			case round%every == 0:
				return 60 * time.Millisecond
			}

			return 0
		}), 400)
		r.run(600)

		if cuts := len(r.cuts()); cuts != r.controller.Params.Futile {
			t.Errorf("a stall answering one probe in %d, no cut shortening it, was cut %d times; want exactly %d, "+
				"then the latch", every, cuts, r.controller.Params.Futile)
		}
	}
}

// A SPAN FILLS ONLY THE ROUNDS THAT COUNTED A REFLECTOR, AND NEVER ENOUGH FOR
// ONE PAIR ALONE TO CUT: two stalled readings either side of rounds whose
// reflectors all went uncounted lend those rounds nothing and count the first
// reading instead, and two either side of unread rounds that counted reflectors
// fill the nearest, so a gateway that drops its replies as it delays them cuts
// on a third reading, not on two pings.
func TestASpanSkipsRoundsThatCountedNoReflectorAndNeverFillsAWindowAlone(t *testing.T) {
	t.Parallel()

	stalled := func(n int) Observation {
		return withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayRose(60*time.Millisecond))
	}
	empty := func(n int) Observation {
		return withGateway(probed(n, 5, 400, Sample{}), gatewayRose(60*time.Millisecond))
	}
	unread := func(n int) Observation { return withGateway(moving(n, 5, 400, 2*time.Millisecond), gatewayLost()) }

	for name, tc := range map[string]struct {
		rounds []Observation
		want   []bool
	}{
		"across rounds that counted no reflector": {
			[]Observation{stalled(0), empty(1), empty(2), stalled(3)}, []bool{true, false, false, true}},
		"across three unread rounds": {
			[]Observation{stalled(0), unread(1), unread(2), unread(3), stalled(4)},
			[]bool{false, false, false, true, true}},
		"past a round that counted no reflector": {
			[]Observation{stalled(0), unread(1), empty(2), stalled(3)}, []bool{false, true, false, true}},
	} {
		c := newController()
		for n, o := range tc.rounds {
			c.Step(o)
			if c.Cause != CauseNone {
				t.Fatalf("%s: round %d cut on one pair of stalled readings", name, n)
			}
		}

		if !slices.Equal(c.recent, tc.want) {
			t.Errorf("%s: the window is %v, want %v", name, c.recent, tc.want)
		}
	}

	// AND A THIRD READING CUTS.
	c := newController()
	for _, o := range []Observation{stalled(0), unread(1), unread(2), stalled(3), unread(4), stalled(5)} {
		c.Step(o)
	}
	if c.Cause == CauseNone {
		t.Error("three stalled readings, each two unread rounds apart, cut nothing")
	}

	// THE TRAIL SPANS WHAT THE WINDOW DOES, so a quiet pair straight after a
	// span, a sparse gateway answering every probe again, does not end a latched
	// episode while the window still says the line is full. A trickle, so nothing
	// is cut and the window keeps its rounds.
	c = newController()
	c.Down.episode, c.Down.futile = true, c.Params.Futile
	trickle := func(n int, s Sample) Observation { return withGateway(moving(n, 0.1, 0.1, 2*time.Millisecond), s) }
	for n, s := range []Sample{gatewayRose(60 * time.Millisecond), gatewayLost(), gatewayLost(),
		gatewayRose(60 * time.Millisecond), gatewayRose(time.Millisecond), gatewayRose(time.Millisecond)} {
		c.Step(trickle(n, s))
	}

	if !c.Down.episode || c.Down.futile != c.Params.Futile {
		t.Errorf("a quiet pair beside a window still full ended a latched episode (episode %v, futile %d, "+
			"trail %v)", c.Down.episode, c.Down.futile, c.delayTrail)
	}
}

// A CUT WHOSE EFFECT NO SOURCE COULD JUDGE IS NOT PROOF IT FAILED: it counts
// toward the latch, but the cut after it is as deep as its delay says, not the
// deep cut that follows one proved not to have helped.
func TestACutNothingCouldJudgeIsNotDeepened(t *testing.T) {
	t.Parallel()

	// AN EPISODE WHOSE LAST CUT WAS TAKEN ON THE GATEWAY ALONE, long enough ago
	// for the cooldown, and now the reflectors' queue with the gateway unread.
	c := newController()
	c.Down.Rate = 300
	c.Down.episode, c.Down.lastCut, c.Down.cutSince = true, at(-100), true
	c.Down.cutGateway = measured{60 * time.Millisecond, true}
	c.Down.cutTally = tally{"a": {6, 0}, "b": {6, 0}, "c": {6, 0}}

	delay := 20 * time.Millisecond
	for n := range 3 {
		c.Step(withGateway(moving(n, 0, 300, delay), gatewayLost()))
	}

	if c.Cause == CauseNone {
		t.Fatal("no cut on three rounds of the reflectors' queue")
	}

	if want := 300 * c.cutFactor(delay); c.Down.Rate < want*0.999 || c.Down.Rate > want*1.001 {
		t.Errorf("a cut after one nothing could judge took the download to %.1f, want %.1f", c.Down.Rate, want)
	}

	if c.Down.futile != 1 {
		t.Errorf("a cut nothing could judge left futile at %d, want it counted toward the latch", c.Down.futile)
	}
}

// ONE QUIET REPLY IN A STALL DOES NOT CREDIT A CUT: beside reflectors queued by
// somebody else, a gateway stalled three rounds in five keeps the latch at the
// cuts a stall with no quiet replies would take.
func TestAQuietReplyInAStallCreditsNoCut(t *testing.T) {
	t.Parallel()

	c := newController()
	cuts := 0

	for n := range 600 {
		rise := 60 * time.Millisecond
		if n%5 >= 3 {
			rise = time.Millisecond
		}

		c.Step(withGateway(moving(n, 5, 400, 30*time.Millisecond), gatewayRose(rise)))
		if c.Cause != CauseNone {
			cuts++
		}
	}

	if cuts != c.Params.Futile {
		t.Errorf("a queue no cut shortened was cut %d times beside a gateway quiet two rounds in five; the latch "+
			"should hold after exactly %d", cuts, c.Params.Futile)
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
