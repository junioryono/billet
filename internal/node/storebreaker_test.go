package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/lease"
	storecontract "github.com/junioryono/billet/internal/store"
)

var errStoreGone = errors.New("rbd: exit status 110")

func testBreaker() (*storeBreaker, *atomic.Int32) {
	changes := new(atomic.Int32)

	return newStoreBreaker("ceph", slog.New(slog.DiscardHandler),
		func() { changes.Add(1) }), changes
}

func TestThreeFailuresWithinTheWindowOpenTheBreakerAndTwoDoNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, changes := testBreaker()

		b.outcome("clone", errStoreGone)
		b.outcome("clone", errStoreGone)
		if !b.allow() {
			t.Fatal("two failures opened the breaker")
		}
		if changes.Load() != 0 {
			t.Fatal("two failures reported a change")
		}

		b.outcome("clone", errStoreGone)
		if b.allow() {
			t.Fatal("three failures left the breaker closed")
		}
		if got := b.report(); got.State != lease.CacheStoreOpen || got.Failures != 3 ||
			got.Reason != lease.CacheStoreError || got.Store != "ceph" {
			t.Fatalf("report = %+v", got)
		}
		if changes.Load() != 1 {
			t.Fatalf("opening reported %d changes", changes.Load())
		}
	})
}

func TestAFailureOutsideTheWindowDoesNotCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := testBreaker()

		b.outcome("clone", errStoreGone)
		b.outcome("clone", errStoreGone)
		time.Sleep(breakerWindow)
		b.outcome("clone", errStoreGone)
		if !b.allow() {
			t.Fatal("failures a window apart opened the breaker")
		}
		time.Sleep(time.Second)
		b.outcome("clone", errStoreGone)
		b.outcome("clone", errStoreGone)
		if b.allow() {
			t.Fatal("three failures within the window left it closed")
		}
	})
}

func TestWhatTheStoreAnsweredIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, err := range []error{
			nil,
			fmt.Errorf("wrapped: %w", storecontract.ErrMiss),
			fmt.Errorf("wrapped: %w", storecontract.ErrConflict),
			&storecontract.WriterHeldError{Key: "k", Holder: "h", Expires: time.Now()},
			// The caller went away, which says nothing about the store.
			fmt.Errorf("wrapped: %w", context.Canceled),
		} {
			b, _ := testBreaker()
			for range breakerFailures {
				b.outcome("clone", err)
			}
			if !b.allow() {
				t.Errorf("%v opened the breaker", err)
			}
		}
	})
}

func TestADeadlineCountsAsATimeoutAndAnythingElseAsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for want, err := range map[lease.CacheStoreReason]error{
			lease.CacheStoreTimeout: fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
			lease.CacheStoreError:   errStoreGone,
		} {
			b, _ := testBreaker()
			for range breakerFailures {
				b.outcome("clone", err)
			}
			if got := b.report().Reason; got != want {
				t.Errorf("%v: reason = %q, want %q", err, got, want)
			}
		}
	})
}

// A CALL THAT NEVER RETURNS STILL COUNTS, from its limit on, which is the whole of what
// a store that hangs rather than refuses leaves anything to go on.
func TestACallThatNeverReturnsCountsAtItsLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := testBreaker()

		for range breakerFailures {
			b.watch("clone", time.Minute)
		}
		time.Sleep(time.Minute - time.Second)
		synctest.Wait()
		if !b.allow() {
			t.Fatal("calls still inside their limit opened the breaker")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if b.allow() {
			t.Fatal("calls past their limit left the breaker closed")
		}
		if got := b.report().Reason; got != lease.CacheStoreTimeout {
			t.Fatalf("reason = %q, want a timeout", got)
		}
	})
}

// A CALL THAT RETURNS IS COUNTED ONCE, however it lands against its limit.
func TestAWatchedCallCountsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Returned in time, with an answer: nothing counts, and the timer does not
		// count it later.
		b, _ := testBreaker()
		for range breakerFailures {
			b.watch("clone", time.Minute)(nil)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if !b.allow() {
			t.Fatal("calls that returned an answer counted when their timers would have fired")
		}

		// Past its limit and then failed: one failure, not two.
		b, _ = testBreaker()
		done := b.watch("clone", time.Second)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		done(errStoreGone)
		done2 := b.watch("clone", time.Second)
		done2(errStoreGone)
		if !b.allow() {
			t.Fatal("two calls counted as three failures")
		}
	})
}

// run is driven through its real ticker: the question is asked only while open, and
// each answer does what it says.
func TestAnOpenBreakerIsProbedAndOnlyAnAnswerMovesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		b, changes := testBreaker()
		var asked atomic.Int32
		var answer atomic.Pointer[error]
		answer.Store(&errStoreGone)
		go b.run(ctx, func(probeCtx context.Context) error {
			asked.Add(1)
			if deadline, ok := probeCtx.Deadline(); !ok || time.Until(deadline) != breakerProbeLimit {
				t.Errorf("the probe's bound = %v, %v", deadline, ok)
			}
			return *answer.Load()
		})

		// CLOSED, NOTHING IS ASKED.
		time.Sleep(3 * breakerProbeEvery)
		synctest.Wait()
		if asked.Load() != 0 {
			t.Fatalf("a closed breaker was probed %d times", asked.Load())
		}

		for range breakerFailures {
			b.outcome("clone", errStoreGone)
		}
		before := changes.Load()

		// COULD NOT TELL LEAVES IT OPEN.
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if asked.Load() != 1 || b.allow() {
			t.Fatalf("after a failed probe: asked %d, allowed %v", asked.Load(), b.allow())
		}
		if changes.Load() != before {
			t.Fatal("a failed probe reported a change")
		}

		// A MISS IS THE STORE ANSWERING, and it goes on probation, not straight back.
		miss := fmt.Errorf("wrapped: %w", storecontract.ErrMiss)
		answer.Store(&miss)
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if !b.allow() {
			t.Fatal("an answered probe left jobs skipping the store")
		}
		if got := b.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("state = %q, want probation", got)
		}

		// ONE FAILURE ON PROBATION REOPENS IT AT ONCE.
		b.outcome("clone", errStoreGone)
		if b.allow() {
			t.Fatal("a failure on probation left it allowed")
		}

		// AN ANSWER LATE FROM BEFORE IS NOT A PROBE.
		b.outcome("clone", nil)
		if b.allow() {
			t.Fatal("a call's answer closed an open breaker")
		}

		// AND ONE ANSWER ON PROBATION CLOSES IT.
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		b.outcome("clone", nil)
		if got := b.report(); got.State != lease.CacheStoreClosed || got.Failures != 0 || got.Reason != "" {
			t.Fatalf("report = %+v, want closed with nothing carried", got)
		}

		// CLOSED AGAIN, IT STARTS COUNTING FROM NOTHING.
		b.outcome("clone", errStoreGone)
		b.outcome("clone", errStoreGone)
		if !b.allow() {
			t.Fatal("failures before the close were still counted")
		}

		cancel()
		synctest.Wait()
	})
}

func TestANilBreakerAllowsEverythingAndCountsNothing(t *testing.T) {
	t.Parallel()

	var b *storeBreaker
	if !b.allow() {
		t.Error("a store nobody guards was refused")
	}
	b.outcome("clone", errStoreGone)
	b.watch("clone", time.Hour)(errStoreGone)
}
