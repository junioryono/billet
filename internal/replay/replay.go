// Package replay drives a trace of job arrivals through the real control plane,
// node wire, node runtime and simulated backend at compressed time, and reads
// what billet recorded about each job back out of the ledger.
//
// It exists so a scheduling change can be asked what it does to a workload:
// under this week of arrivals, where did placement put each job, how long did
// jobs queue, and how did two policies differ. `make test` proves invariants on
// single leases and the end-to-end suite proves the wire and the runtime; neither
// runs a thousand jobs through the placer, because every other backend needs a
// guest per job.
//
// THE ALLOCATOR, THE LISTENER, THE PLACER AND THE NODE ARE THE REAL ONES, or the
// harness would measure a model of billet rather than billet. What is scripted
// is GitHub, through internal/fakeactions, and what is steered is time: one
// Clock the allocator and the simulated provider read, moved only between
// events, so a replay is deterministic and a modelled hour costs no wall time.
//
// IT IS A TEST-SIDE CONSUMER AND STAYS ONE. It needs a *testing.T, no production
// code imports it, and boundary_test.go proves that over the module's import
// graph, as the simulated backend and the fake Actions service prove theirs.
package replay

import (
	"log/slog"
	"testing"
	"time"
)

// Options vary a replay beyond its fleet and trace.
type Options struct {
	// Log receives billet's own diagnostics. Nil writes them to the test log.
	Log *slog.Logger
	// SettleWithin bounds, in wall time, how long billet may take to settle
	// after one event before the replay is declared stuck. Zero means a minute.
	SettleWithin time.Duration
}

func (o Options) settleWithin() time.Duration {
	if o.SettleWithin > 0 {
		return o.SettleWithin
	}

	return time.Minute
}

// Run replays a trace against a fleet and reports what the ledger recorded.
//
// ONE EVENT AT A TIME, TO QUIESCENCE. Every arrival, start and completion is
// delivered alone, and the next is not delivered until every listener is parked
// on an empty queue with its escrow settled. Placement happens at escrow, so two
// events in flight at once would let goroutine order decide which host a job
// lands on, and a replay whose placements vary between runs cannot compare two
// policies. Determinism is the property; the seed only shapes a synthetic trace.
func Run(t *testing.T, fleet Fleet, trace Trace, opts Options) *Report {
	t.Helper()

	if err := trace.Validate(); err != nil {
		t.Fatalf("replay: the trace is not replayable: %v", err)
	}

	if err := fleet.validate(trace); err != nil {
		t.Fatalf("replay: the fleet cannot carry the trace: %v", err)
	}

	log := opts.Log
	if log == nil {
		log = testLogger(t)
	}

	tiers := fleet.tiers(trace)

	// Started a minute before the first arrival, so every discovery escrow is
	// dated before any job and the ledger reads in order.
	clock := NewClock(trace.Arrivals[0].At.Add(-time.Minute))

	actions := newPlane(t, clock, fleet, trace)
	st := buildStack(t, log, fleet, tiers, clock, actions)
	stop := st.run(t)

	settle := func(what string) {
		t.Helper()

		timer := time.NewTimer(opts.settleWithin())
		defer timer.Stop()

		// The watcher ends with the wait it bounds. Left selecting on the timer
		// alone it would outlive every settle until the test ended: thousands of
		// goroutines over one replay.
		deadline := make(chan struct{})
		settled := make(chan struct{})

		defer close(settled)

		go func() {
			select {
			case <-timer.C:
				close(deadline)
			case <-t.Context().Done():
				close(deadline)
			case <-settled:
			}
		}()

		if err := actions.AwaitIdle(deadline); err != nil {
			t.Fatalf("replay: after %s: %v", what, err)
		}
	}

	// Every session open and every discovery slot escrowed before the first job
	// arrives, in tier order.
	settle("startup")

	for i := range trace.Arrivals {
		a := &trace.Arrivals[i]
		actions.schedule(event{at: a.At, kind: eventArrival, seq: a.Seq})
	}

	for {
		ev, ok := actions.next()
		if !ok {
			break
		}

		clock.Step(ev.at)
		actions.deliver(ev)
		settle(describe(ev))

		// THE CONTEST FOR ROOM, IN ONE ORDER. A tier with jobs waiting, or with no
		// capacity advertised at all, is given one poll to re-escrow and be offered
		// what waits, tier by tier, each to quiescence before the next. What a
		// tier does on its OWN poll after its own event is billet's: the listener
		// whose job finished tops its discovery slot back up before anything is
		// offered to anyone, exactly as it would in production. What the harness
		// decides is only who is offered the rest, and that is this order.
		for _, label := range actions.order {
			if actions.starved(label) {
				actions.nudge(label)
				settle("a poll for " + label)
			}
		}
	}

	stop()
	st.stopNodes()

	report, err := readReport(t.Context(), st.db, fleet, trace)
	if err != nil {
		t.Fatalf("replay: read the ledger back: %v", err)
	}

	st.closeDB()

	return report
}

func describe(ev event) string {
	switch ev.kind {
	case eventArrival:
		return "the arrival of job " + itoa(ev.seq)
	case eventStarted:
		return "the start of job " + itoa(ev.seq)
	case eventCompleted:
		return "the completion of job " + itoa(ev.seq)
	default:
		return "an event"
	}
}

// testLogger writes billet's diagnostics into the test log, where they appear
// only for a failing test.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)

	return len(p), nil
}
