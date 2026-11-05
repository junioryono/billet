package e2e

import (
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider/simulated"
)

// listenerTimes counts what a stack's listeners told.
type listenerTimes struct {
	mu                               sync.Mutex
	polls, gaps, messages, teardowns int
}

func (l *listenerTimes) PollTook(string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.polls++
}

func (l *listenerTimes) PollGapTook(string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.gaps++
}

func (l *listenerTimes) MessageHandled(string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.messages++
}

func (l *listenerTimes) TeardownTook(string, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.teardowns++
}

func (l *listenerTimes) all() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.polls > 0 && l.gaps > 0 && l.messages > 0 && l.teardowns > 0
}

// A CONTROL PLANE'S LISTENERS REPORT THEIR TIMES, through what its metrics endpoint
// connects to. A listener built without them would simply report nothing, and a dashboard
// over the metrics would read that as a tier with nothing to say.
func TestAControlPlanesListenersReportTheirTimes(t *testing.T) {
	s := newStack(t, withBackend(config.ProviderSimulated, simulatedBackend(simulated.NewHost())))

	times := &listenerTimes{}
	s.timings.Observe(times)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 4001, "push", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 4001, "push", testTier))
	s.awaitOneRunning(t)

	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 4001, "push", testTier))
	s.awaitGone(t)

	deadline = time.Now().Add(30 * time.Second)

	for !times.all() {
		if time.Now().After(deadline) {
			times.mu.Lock()
			polls, gaps, messages, teardowns := times.polls, times.gaps, times.messages, times.teardowns
			times.mu.Unlock()

			t.Fatalf("told %d polls, %d gaps, %d messages and %d teardowns, want each at least once",
				polls, gaps, messages, teardowns)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
