package nodeclient

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A DESTROY GIVEN TWICE RUNS ONCE: a plane may hand one over again (a node below
// VersionDestroysBeside, or a poll that raced its report), and its one report
// answers both.
func TestADestroyGivenTwiceRunsOnce(t *testing.T) {
	t.Parallel()

	q := newDestroyQueue(nil)
	gate := make(chan struct{})

	var mu sync.Mutex
	runs := 0
	destroy := func() error {
		<-gate
		mu.Lock()
		runs++
		mu.Unlock()

		return nil
	}

	if !q.submit("c1", 7, nil, destroy) {
		t.Fatal("the first submission was refused")
	}
	if q.submit("c1", 7, nil, destroy) {
		t.Fatal("a destroy already held was queued again")
	}
	if got := q.held(); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("held %v, want [c1]", got)
	}

	close(gate)
	q.wait()

	if runs != 1 {
		t.Fatalf("ran %d times, want once", runs)
	}
	if got := q.held(); len(got) != 0 {
		t.Fatalf("still held after it ran: %v", got)
	}
}

// PENDING NAMES ONLY THE DESTROYS OF A REQUEST QUEUED NOW. A launch waits for those;
// one of the same request queued after it waits for the launch, and a launch that
// waited for that one in turn would wait for itself.
func TestPendingNamesOnlyTheDestroysOfARequestQueuedNow(t *testing.T) {
	t.Parallel()

	q := newDestroyQueue(nil)
	gate := make(chan struct{})
	t.Cleanup(func() { q.wait() })
	t.Cleanup(func() { close(gate) })

	hold := func() error { <-gate; return nil }
	q.submit("a", 1, nil, hold)
	q.submit("b", 2, nil, hold)

	earlier := q.pending(1)
	if len(earlier) != 1 {
		t.Fatalf("%d pending for request 1, want 1", len(earlier))
	}
	if got := q.pending(3); len(got) != 0 {
		t.Fatalf("%d pending for a request with no destroy, want none", len(got))
	}

	q.submit("c", 1, nil, hold)

	if len(earlier) != 1 {
		t.Fatal("a snapshot grew")
	}
	if got := q.pending(1); len(got) != 2 {
		t.Fatalf("%d pending for request 1 after a second destroy, want 2", len(got))
	}
}

// A MAINTENANCE TURN RUNS IN LINE: after the destroys queued before it and before
// those queued after, held by no command.
func TestAMaintenanceTurnRunsBetweenTheDestroysAroundIt(t *testing.T) {
	t.Parallel()

	q := newDestroyQueue(nil)
	gate := make(chan struct{})

	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	q.submit("a", 1, nil, func() error { <-gate; note("a"); return nil })
	q.turn(func() { note("turn") })
	q.submit("b", 2, nil, func() error { note("b"); return nil })

	if got := q.held(); len(got) != 2 {
		t.Fatalf("held %v, want the two destroys alone", got)
	}

	close(gate)
	q.wait()

	if !slices.Equal(order, []string{"a", "turn", "b"}) {
		t.Fatalf("order %v, want [a turn b]", order)
	}
}

// ROOM IS WAITED FOR AT THE MOST A POLL NAMES, counting the destroy running as well as
// those queued, and only until one of them has run, never the whole line. In a bubble,
// so "still waiting" is the scheduler's word, not a deadline's.
func TestRoomIsWaitedForAtTheMostAPollNames(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		q := newDestroyQueue(nil)
		first, rest := make(chan struct{}), make(chan struct{})
		releaseFirst := sync.OnceFunc(func() { close(first) })
		t.Cleanup(func() { releaseFirst(); close(rest); q.wait() })

		// BELOW THE BOUND waitRoom returns at once: were it to wait, nothing in the
		// bubble would wake it and the bubble would report the deadlock.
		for i := range nodeapi.MaxDestroysRunning {
			q.waitRoom()
			gate := rest
			if i == 0 {
				gate = first
			}
			q.submit(strconv.Itoa(i), int64(i), nil, func() error { <-gate; return nil })
		}

		var roomed atomic.Bool
		go func() { q.waitRoom(); roomed.Store(true) }()

		synctest.Wait()
		if roomed.Load() {
			t.Fatalf("room at %d", nodeapi.MaxDestroysRunning)
		}

		releaseFirst()
		synctest.Wait()
		if !roomed.Load() {
			t.Fatal("no room once one destroy had run")
		}
	})
}

// A DESTROY THAT WAITED OUT A LAUNCH DOES NOT START AFTER THE SECOND SIGNAL: it would run
// under a fresh bounded context of its own and hold a forced stop; its command is left
// for the plane to give again.
func TestADestroyWaitingOnALaunchDoesNotStartAfterTheSecondSignal(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		stop, launch := make(chan struct{}), make(chan struct{})
		q := newDestroyQueue(stop)

		var ran atomic.Bool
		q.submit("a", 1, []<-chan struct{}{launch}, func() error { ran.Store(true); return nil })

		synctest.Wait()
		close(stop)
		close(launch)
		q.wait()

		if ran.Load() {
			t.Fatal("a destroy started after the second signal")
		}
		if got := q.held(); len(got) != 0 {
			t.Fatalf("still held after the line ended: %v", got)
		}
	})
}

// THE SECOND SIGNAL ENDS THE LINE: the destroy running finishes, and none queued
// behind it starts. Each would run under a fresh bounded context of its own, so a long
// line would hold a forced stop; the plane gives again what it was not answered.
func TestTheSecondSignalStartsNoQueuedDestroy(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	q := newDestroyQueue(stop)
	gate, started := make(chan struct{}), make(chan struct{})

	var mu sync.Mutex
	var ran []string
	destroy := func(id string) func() error {
		return func() error {
			if id == "a" {
				close(started)
				<-gate
			}
			mu.Lock()
			ran = append(ran, id)
			mu.Unlock()

			return nil
		}
	}

	q.submit("a", 1, nil, destroy("a"))
	q.submit("b", 2, nil, destroy("b"))
	q.submit("c", 3, nil, destroy("c"))
	later := q.pending(3)
	<-started

	close(stop)
	close(gate)
	q.wait()

	if !slices.Equal(ran, []string{"a"}) {
		t.Fatalf("ran %v after the second signal, want only the destroy already running", ran)
	}
	if got := q.held(); len(got) != 0 {
		t.Fatalf("still held after the line ended: %v", got)
	}
	for _, done := range later {
		select {
		case <-done:
		default:
			t.Fatal("a launch waiting on a destroy the line dropped would wait for ever")
		}
	}
}
