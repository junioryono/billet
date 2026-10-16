package uplink

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProber answers each round as rtt says: a reflector with no entry, or one
// whose round trip is longer than the round waits, goes unanswered, as the
// Pinger's answers do.
type fakeProber struct {
	reflectors []string
	rtt        func(round int) map[string]time.Duration
	round      int
	waits      []time.Duration
}

func (f *fakeProber) Reflectors() []string { return f.reflectors }

func (f *fakeProber) Round(_ context.Context, wait time.Duration) (map[string]time.Duration, error) {
	f.waits = append(f.waits, wait)
	answered := map[string]time.Duration{}

	for reflector, rtt := range f.rtt(f.round) {
		if rtt <= wait {
			answered[reflector] = rtt
		}
	}

	f.round++

	return answered, nil
}

// records keeps what a run logged.
type records struct {
	mu   sync.Mutex
	list []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.list = append(r.list, rec.Clone())

	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

// attrs is a record's attributes by key.
func attrs(rec slog.Record) map[string]slog.Value {
	m := map[string]slog.Value{}

	rec.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value

		return true
	})

	return m
}

// rig is a run's stepper over a fake prober, a host sending 5 Mbit/s and
// receiving downMbit, a clock that advances a Tick per round, and a shaper that
// only records.
type rig struct {
	st         *stepper
	log        *records
	sets       [][2]float64
	downMbit   float64
	counters   Counters
	clock      time.Time
	controller *Controller
	ticks      int
	t          *testing.T
}

func newRig(t *testing.T, prober *fakeProber, downMbit float64) *rig {
	t.Helper()

	r := &rig{log: &records{}, downMbit: downMbit, clock: time.Unix(1_000_000, 0),
		controller: newController(), t: t}

	read := func() (Counters, error) {
		bytes := func(mbit float64) uint64 { return uint64(mbit * 1e6 * Tick.Seconds() / 8) }
		r.counters.Sent += bytes(5)
		r.counters.Received += bytes(r.downMbit)

		return r.counters, nil
	}
	set := func(_ context.Context, up, down float64) error {
		r.sets = append(r.sets, [2]float64{up, down})

		return nil
	}
	now := func() time.Time { return r.clock }

	st, err := newStepper(slog.New(r.log), prober, r.controller, read, set, now)
	if err != nil {
		t.Fatal(err)
	}

	r.st = st

	return r
}

// run steps n rounds.
func (r *rig) run(n int) {
	r.t.Helper()

	for range n {
		r.clock = r.clock.Add(Tick)
		r.ticks++

		if err := r.st.tick(r.t.Context()); err != nil {
			r.t.Fatalf("tick %d: %v", r.ticks, err)
		}
	}
}

// cuts are the records of cuts the run logged.
func (r *rig) cuts() []slog.Record {
	var cuts []slog.Record

	for i := range r.log.list {
		if strings.HasSuffix(r.log.list[i].Message, "; cut") {
			cuts = append(cuts, r.log.list[i])
		}
	}

	return cuts
}

var three = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}

// idle is every reflector answering at its idle round trip.
func idle() map[string]time.Duration {
	return map[string]time.Duration{"1.1.1.1": 7 * time.Millisecond, "8.8.8.8": 9 * time.Millisecond,
		"9.9.9.9": 12 * time.Millisecond}
}

// A PROBER THAT LOSES REPLIES, through the run's own step: a gateway that drops
// rather than queues loses one probe in three, from a different reflector each
// round, while the round trip of the rest stays at its idle baseline. A host
// pulling 400 Mbit/s down is cut, the cut is logged as taken on loss, and the
// delay it logs is the one the answers measured, never an invented one.
func TestALosingProberCutsABusyHostOnLoss(t *testing.T) {
	t.Parallel()

	p := &fakeProber{reflectors: three, rtt: func(round int) map[string]time.Duration {
		answers := idle()
		if round >= 4 {
			delete(answers, three[round%3])
		}

		return answers
	}}
	r := newRig(t, p, 400)

	r.run(12)

	if len(r.sets) == 0 || r.sets[0][1] >= 400 {
		t.Fatalf("a host pulling 400 Mbit/s down through a line losing a probe a round was never cut: %v", r.sets)
	}

	cuts := r.cuts()
	if len(cuts) == 0 {
		t.Fatal("the cut was not logged")
	}

	a := attrs(cuts[0])
	if cause := a["cause"].String(); cause != "loss" || cuts[0].Message != cutMessage(CauseLoss) {
		t.Fatalf("the cut was logged as %q, cause %q; want loss", cuts[0].Message, cause)
	}

	if d := a["delay"]; d.Kind() != slog.KindDuration || d.Duration() > time.Millisecond {
		t.Fatalf("the cut logged delay %v; two of three answered at their baselines, so it is about zero", d)
	}

	if lost := a["lost"].String(); lost != "1/3" {
		t.Fatalf("the cut logged %s lost; want 1/3", lost)
	}

	for _, w := range p.waits {
		if w != Tick*8/10 {
			t.Fatalf("a round waited %v; a reply later than %v belongs to no round", w, Tick*8/10)
		}
	}

	// TWO OF THREE LOST leave no delay to measure: the cut is still taken on the
	// loss, and logs its delay as unmeasured, never as a number (2026-10-15:
	// `delay=2562047h47m16.854775807s`).
	p = &fakeProber{reflectors: three, rtt: func(round int) map[string]time.Duration {
		answers := idle()
		if round >= 4 {
			delete(answers, three[round%3])
			delete(answers, three[(round+1)%3])
		}

		return answers
	}}
	r = newRig(t, p, 400)

	r.run(12)

	if cuts = r.cuts(); len(cuts) == 0 {
		t.Fatalf("a line losing two probes in three under 400 Mbit/s down was never cut: %v", r.sets)
	}

	a = attrs(cuts[0])
	if a["cause"].String() != "loss" || a["delay"].Kind() != slog.KindString ||
		a["delay"].String() != "unmeasured" || a["lost"].String() != "2/3" {
		t.Fatalf("the cut was logged with cause %v, delay %v and %v lost; want loss, unmeasured and 2/3",
			a["cause"], a["delay"], a["lost"])
	}
}

// A PROBER THAT RETURNS NO MEASUREMENT AT ALL: no reflector ever answers, so
// no round counts one. With the direction running at its rate below the
// ceiling, a could-not-tell read as a queue would cut and one read as no queue
// would raise; it does neither, and the log says it cannot tell.
func TestAProberThatMeasuresNothingHoldsBothRates(t *testing.T) {
	t.Parallel()

	p := &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration { return nil }}
	r := newRig(t, p, 200)
	r.controller.Down.Rate = 200
	r.st.applied = [2]float64{r.controller.Up.Rate, 200}

	r.run(2 * silentAfter)

	if len(r.sets) != 0 || r.controller.Down.Rate != 200 {
		t.Fatalf("with nothing measured the rates moved: down at %.1f, sets %v", r.controller.Down.Rate, r.sets)
	}

	if len(r.cuts()) != 0 {
		t.Fatalf("a cut was logged with nothing measured: %v", r.cuts()[0].Message)
	}

	told := 0

	for i := range r.log.list {
		if rec := &r.log.list[i]; rec.Level == slog.LevelWarn && strings.Contains(rec.Message, "cannot be told") {
			told++
		}
	}

	if told != 1 {
		t.Fatalf("the could-not-tell was logged %d times; want once, after the reflectors had %d rounds",
			told, silentAfter)
	}
}

// A PROBER THAT DELAYS: replies 30 ms over their baselines are a queue, cut for
// and logged as the 30 ms measured. Replies later than a round waits are no
// measurement at all, never a delay: from a start where every reply came too
// late, a direction running at its rate below the ceiling is not raised.
func TestADelayingProberIsMeasuredInsideTheWaitAndBlindBeyondIt(t *testing.T) {
	t.Parallel()

	queued := &fakeProber{reflectors: three, rtt: func(round int) map[string]time.Duration {
		answers := idle()
		if round >= 4 {
			for reflector, rtt := range answers {
				answers[reflector] = rtt + 30*time.Millisecond
			}
		}

		return answers
	}}
	r := newRig(t, queued, 400)

	r.run(12)

	cuts := r.cuts()
	if len(cuts) == 0 || len(r.sets) == 0 || r.sets[0][1] >= 400 {
		t.Fatalf("replies 30 ms over their baselines with the host pulling 400 Mbit/s cut nothing: %v", r.sets)
	}

	a := attrs(cuts[0])
	if a["cause"].String() != "delay" || a["delay"].Kind() != slog.KindDuration ||
		a["delay"].Duration() < 29*time.Millisecond || a["delay"].Duration() > 30*time.Millisecond {
		t.Fatalf("the cut was logged with cause %v and delay %v; want delay, about 30 ms", a["cause"], a["delay"])
	}

	late := &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration {
		answers := idle()
		for reflector := range answers {
			answers[reflector] = time.Second
		}

		return answers
	}}
	r = newRig(t, late, 200)
	r.controller.Down.Rate = 200
	r.st.applied = [2]float64{r.controller.Up.Rate, 200}

	r.run(2 * silentAfter)

	if len(r.sets) != 0 || r.controller.Down.Rate != 200 {
		t.Fatalf("replies too late to measure moved the rates: down at %.1f, sets %v", r.controller.Down.Rate, r.sets)
	}
}

// count is how many records the run logged with a message containing text.
func (r *rig) count(text string) int {
	n := 0

	for i := range r.log.list {
		if strings.Contains(r.log.list[i].Message, text) {
			n++
		}
	}

	return n
}

// THE COULD-NOT-TELL IS LOGGED WHEN THE REFLECTORS STOP ANSWERING, on the
// round they stop counting and both rates are held, and their return is logged
// when they answer again; each time they fall silent. Answers count, not
// probes: the lost probes of reflectors still being given their time are not
// answers, and neither is a first answer that only sets a baseline.
func TestTheCouldNotTellIsLoggedWhenTheReflectorsStopAndWhenTheyReturn(t *testing.T) {
	t.Parallel()

	answering := true
	p := &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration {
		if !answering {
			return nil
		}

		return idle()
	}}
	r := newRig(t, p, 1)

	r.run(5)

	for episode := 1; episode <= 2; episode++ {
		answering = false

		r.run(silentAfter)

		if n := r.count("cannot be told"); n != episode-1 {
			t.Fatalf("episode %d: after %d silent rounds the could-not-tell was logged %d times in all; "+
				"the reflectors still count", episode, silentAfter, n)
		}

		r.run(1)

		if n := r.count("cannot be told"); n != episode {
			t.Fatalf("episode %d: on the round the silent reflectors stopped counting the could-not-tell "+
				"was logged %d times in all; want %d", episode, n, episode)
		}

		answering = true

		r.run(1)

		if n := r.count("answer again"); n != episode {
			t.Fatalf("episode %d: the reflectors' return was logged %d times in all; want %d", episode, n, episode)
		}
	}

	// From a start no reflector answered: a first answer only sets the
	// baselines, and a lost round after it is no return.
	answering = false
	p = &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration {
		if !answering {
			return nil
		}

		return idle()
	}}
	r = newRig(t, p, 1)

	r.run(silentAfter + 5)

	answering = true
	r.run(1)
	answering = false
	r.run(1)

	if r.count("cannot be told") != 1 || r.count("answer again") != 0 {
		t.Fatalf("a start with no answers, then a baseline and a lost round, logged %d could-not-tells and %d "+
			"returns; want 1 and 0", r.count("cannot be told"), r.count("answer again"))
	}

	answering = true
	r.run(1)

	if r.count("answer again") != 1 {
		t.Fatalf("a counted answer after the start's silence logged %d returns; want 1", r.count("answer again"))
	}

	// A stalled process passes the ten seconds in two rounds, whose reflectors
	// are still counted and whose losses can still cut: the rates are not held,
	// and the journal must not say they are.
	answering = false
	r.run(1)
	r.clock = r.clock.Add(silentAfter * Tick)
	r.run(1)

	if n := r.count("cannot be told"); n != 1 {
		t.Fatalf("two lost rounds across a ten-second stall logged the could-not-tell (%d in all); the "+
			"reflectors still count", n)
	}
}
