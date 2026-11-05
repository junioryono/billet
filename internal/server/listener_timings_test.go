package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// recordedTimings is what a listener told its observer.
type recordedTimings struct {
	mu       sync.Mutex
	tiers    []string
	polls    []time.Duration
	gaps     []time.Duration
	messages []time.Duration
	teardown []time.Duration
}

func (r *recordedTimings) PollTook(tier string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tiers = append(r.tiers, tier)
	r.polls = append(r.polls, took)
}

func (r *recordedTimings) PollGapTook(_ string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.gaps = append(r.gaps, took)
}

func (r *recordedTimings) MessageHandled(_ string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.messages = append(r.messages, took)
}

func (r *recordedTimings) TeardownTook(_ string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.teardown = append(r.teardown, took)
}

// A TEARDOWN IS REPORTED ONCE, WHEN IT SETTLES, FROM WHEN IT WAS PARKED. A destroy that
// fails and is retried is still the one guest staying charged, and an obligation nothing
// parked (a retry of a failed launch, say) says nothing of how long a finished guest stayed.
func TestATeardownIsReportedOnceItSettles(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 256 * config.GiB},
			[]config.Tier{tier("a-work")})

		var (
			mu       sync.Mutex
			attempts int
		)

		runner := &fakeRunner{onDestroy: func(id int64) error {
			if id != 7 {
				return nil
			}

			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n == 1 {
				return errors.New("the node is busy")
			}

			time.Sleep(30 * time.Second)

			return nil
		}}

		recorded := &recordedTimings{}
		timings := &ListenerTimings{}
		timings.Observe(recorded)

		l := NewListener(a, "a-work", &fakeSession{}, WithRunner(runner), WithBackgroundLaunches(),
			WithTimings(timings), WithCleanupRetryPacing(time.Minute, time.Hour))
		l.cleanup = map[int64]*pendingCleanup{
			7: {job: dispatch.Job{RequestID: 7}, parkedAt: time.Now()},
			8: {job: dispatch.Job{RequestID: 8}},
		}

		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan struct{})

		go func() {
			defer close(finished)

			l.cleanupLoop(ctx)
		}()

		l.wakeCleanup()
		synctest.Wait()

		recorded.mu.Lock()
		reported := len(recorded.teardown)
		recorded.mu.Unlock()

		if reported != 0 {
			t.Fatalf("%d teardowns reported with the only parked one failed and pending", reported)
		}

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		cancel()
		<-finished

		recorded.mu.Lock()
		defer recorded.mu.Unlock()

		if len(recorded.teardown) != 1 {
			t.Fatalf("reported %v, want the one parked completion once", recorded.teardown)
		}
		if took := recorded.teardown[0]; took < time.Minute+30*time.Second {
			t.Fatalf("reported %v, want its retry's backoff and its destroy counted", took)
		}
	})
}

// A LISTENER REPORTS WHAT EACH STEP OF ITS LOOP TOOK, with the time a message took to
// handle in the gap before the next poll, which is the stall this exists to show.
func TestAListenerReportsItsLoopsTimes(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string][]Option{
		"beside the loop": {WithBackgroundLaunches()},
		"on the loop":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				tiers := []config.Tier{tier("a-work")}
				a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 256 * config.GiB}, tiers,
					alloc.WithLeaseTTL(24*time.Hour))

				const (
					polled  = 7 * time.Second
					handled = 11 * time.Second
				)

				var (
					mu    sync.Mutex
					polls int
				)

				session := &fakeSession{
					onGet: func() (*Message, error) {
						mu.Lock()
						polls++
						n := polls
						mu.Unlock()

						if n > 2 {
							time.Sleep(time.Hour)

							return nil, ErrNoMessage
						}

						time.Sleep(polled)

						return &Message{MessageID: int64(n)}, nil
					},
					onDelete: func(int64) error {
						time.Sleep(handled)

						return nil
					},
				}

				recorded := &recordedTimings{}
				timings := &ListenerTimings{}
				timings.Observe(recorded)

				l := NewListener(a, tiers[0].Label, session,
					append(slices.Clone(opts), WithTimings(timings))...)
				ctx, cancel := context.WithCancel(t.Context())
				finished := make(chan error, 1)

				go func() { finished <- l.Run(ctx) }()

				t.Cleanup(func() {
					synctest.Wait()
					cancel()
					<-finished
				})

				time.Sleep(time.Minute)
				synctest.Wait()

				recorded.mu.Lock()
				defer recorded.mu.Unlock()

				if want := []time.Duration{polled, polled}; !slices.Equal(recorded.polls, want) {
					t.Fatalf("polls took %v, want %v", recorded.polls, want)
				}
				if recorded.tiers[0] != tiers[0].Label {
					t.Fatalf("reported for tier %q, want %q", recorded.tiers[0], tiers[0].Label)
				}
				if want := []time.Duration{handled, handled}; !slices.Equal(recorded.messages, want) {
					t.Fatalf("messages took %v, want %v", recorded.messages, want)
				}
				if want := []time.Duration{handled, handled}; !slices.Equal(recorded.gaps, want) {
					t.Fatalf("gaps took %v, want the time each message held the loop, %v", recorded.gaps, want)
				}
			})
		})
	}
}

// A POLL THAT FAILED IS LEFT OUT: its time is the client's retries.
func TestAFailedPollIsNotReportedAsAPoll(t *testing.T) {
	t.Parallel()

	recorded := &recordedTimings{}
	timings := &ListenerTimings{}
	timings.Observe(recorded)

	failing := errors.New("the service is unreachable")

	l := NewListener(nil, "a-work", &fakeSession{}, WithTimings(timings))
	session := &fakeSession{onGet: func() (*Message, error) { return nil, failing }}

	if _, err := l.timedPoll(t.Context(), session, 0, 0); !errors.Is(err, failing) {
		t.Fatalf("the poll's error did not reach its caller: %v", err)
	}
	if len(recorded.polls) != 0 {
		t.Fatalf("a failed poll was reported as %v", recorded.polls)
	}

	session = &fakeSession{onGet: func() (*Message, error) { return nil, ErrNoMessage }}

	if _, err := l.timedPoll(t.Context(), session, 0, 0); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("the poll's answer did not reach its caller: %v", err)
	}
	if len(recorded.polls) != 1 {
		t.Fatalf("a poll with nothing to deliver was reported %d times, want once", len(recorded.polls))
	}
}

// NOTHING OBSERVING, OR NO TIMINGS AT ALL, TELLS NOTHING AND DOES NOT PANIC: a listener
// starts before the metrics endpoint connects, and a harness has none.
func TestTimingsWithNoObserverTellNothing(t *testing.T) {
	t.Parallel()

	var none *ListenerTimings

	none.pollTook("a-work", time.Second)
	none.pollGapTook("a-work", time.Second)
	none.messageHandled("a-work", time.Second)
	none.teardownTook("a-work", time.Second)

	timings := &ListenerTimings{}
	timings.pollTook("a-work", time.Second)

	recorded := &recordedTimings{}
	timings.Observe(recorded)
	timings.Observe(nil)
	timings.pollTook("a-work", time.Second)

	if len(recorded.polls) != 0 {
		t.Fatalf("told an observer that was removed: %v", recorded.polls)
	}
}
