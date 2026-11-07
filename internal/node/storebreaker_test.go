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

const testTrial = time.Minute

func testBreaker() (*storeBreaker, *atomic.Int32) {
	changes := new(atomic.Int32)

	return newStoreBreaker("ceph", testTrial, slog.New(slog.DiscardHandler),
		func() { changes.Add(1) }), changes
}

// aClone is one job asking for a volume, and aLookup one asking what is there.
func aClone(who string) storeCall  { return storeCall{op: "clone", who: who, decides: true} }
func aLookup(who string) storeCall { return storeCall{op: "lookup", who: who} }

// failThree is three different jobs failing, what it takes to open a breaker.
func failThree(b *storeBreaker) {
	for i := range breakerFailures {
		b.outcome(aClone(fmt.Sprintf("job-%d", i)), errStoreGone)
	}
}

func TestThreeJobsFailingWithinTheWindowOpenTheBreakerAndTwoDoNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, changes := testBreaker()

		b.outcome(aClone("one"), errStoreGone)
		b.outcome(aClone("two"), errStoreGone)
		if !b.allow() {
			t.Fatal("two failures opened the breaker")
		}
		if changes.Load() != 0 {
			t.Fatal("two failures reported a change")
		}

		b.outcome(aClone("three"), errStoreGone)
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

// ONE JOB CAN MAKE THE STORE REFUSE IT AS OFTEN AS IT LIKES, and must not be able to turn
// every other job's caches off by doing so.
func TestOneJobFailingOverAndOverIsOneFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := testBreaker()

		for range 10 * breakerFailures {
			b.outcome(aClone("one"), errStoreGone)
		}
		b.outcome(aClone("two"), errStoreGone)
		if !b.allow() {
			t.Fatal("one job's failures counted as more than one")
		}

		// ITS LATEST FAILURE IS THE ONE THAT DATES IT.
		time.Sleep(breakerWindow - time.Second)
		b.outcome(aClone("one"), errStoreGone)
		time.Sleep(time.Second)
		b.outcome(aClone("three"), errStoreGone)
		if !b.allow() {
			t.Fatal("a failure past the window still counted")
		}
		b.outcome(aClone("four"), errStoreGone)
		if b.allow() {
			t.Fatal("three jobs failing within the window left it closed")
		}
	})
}

func TestAFailureOutsideTheWindowDoesNotCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := testBreaker()

		b.outcome(aClone("one"), errStoreGone)
		b.outcome(aClone("two"), errStoreGone)
		time.Sleep(breakerWindow)
		b.outcome(aClone("three"), errStoreGone)
		if !b.allow() {
			t.Fatal("failures a window apart opened the breaker")
		}
		time.Sleep(time.Second)
		b.outcome(aClone("four"), errStoreGone)
		b.outcome(aClone("five"), errStoreGone)
		if b.allow() {
			t.Fatal("three failures within the window left it closed")
		}
	})
}

// A VOLUME HANDED OVER SAYS THE FAILURES SO FAR WERE NOT THE STORE; an answer to a lookup
// does not, because the store can give one with what a job needs out of reach.
func TestOnlyAVolumeHandedOverClearsTheCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _ := testBreaker()

		b.outcome(aClone("one"), errStoreGone)
		b.outcome(aClone("two"), errStoreGone)
		b.outcome(aLookup("ok"), nil)
		b.outcome(aClone("miss"), storecontract.ErrMiss)
		b.outcome(aClone("three"), errStoreGone)
		if b.allow() {
			t.Fatal("an answer that hands over no volume cleared the count")
		}

		b, _ = testBreaker()
		b.outcome(aClone("one"), errStoreGone)
		b.outcome(aClone("two"), errStoreGone)
		b.outcome(aClone("ok"), nil)
		b.outcome(aClone("three"), errStoreGone)
		if !b.allow() {
			t.Fatal("a volume handed over left the count standing")
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
			for i := range breakerFailures {
				b.outcome(aClone(fmt.Sprintf("job-%d", i)), err)
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
			for i := range breakerFailures {
				b.outcome(aClone(fmt.Sprintf("job-%d", i)), err)
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

		for i := range breakerFailures {
			b.watch(aClone(fmt.Sprintf("job-%d", i)), time.Minute)
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
		for i := range breakerFailures {
			b.watch(aClone(fmt.Sprintf("job-%d", i)), time.Minute)(nil)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if !b.allow() {
			t.Fatal("calls that returned an answer counted when their timers would have fired")
		}

		// Past its limit and then handed a volume over: that late, it clears nothing.
		b, _ = testBreaker()
		b.outcome(aClone("one"), errStoreGone)
		late := b.watch(aClone("two"), time.Second)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		late(nil)
		b.outcome(aClone("three"), errStoreGone)
		if b.allow() {
			t.Fatal("a call counted as a timeout then cleared the count")
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

		failThree(b)
		before := changes.Load()

		// COULD NOT TELL LEAVES IT OPEN.
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if asked.Load() != 1 || b.admit() {
			t.Fatalf("after a failed probe: asked %d, admitted %v", asked.Load(), b.admit())
		}
		if changes.Load() != before {
			t.Fatal("a failed probe reported a change")
		}

		// A MISS IS THE STORE ANSWERING, and it goes on probation, not straight back.
		miss := fmt.Errorf("wrapped: %w", storecontract.ErrMiss)
		answer.Store(&miss)
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if got := b.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("state = %q, want probation", got)
		}
		if changes.Load() != before+1 {
			t.Fatalf("going on probation reported %d changes", changes.Load()-before)
		}
		if b.isOpen() {
			t.Fatal("the gauge would say open on probation")
		}

		// A READ OF A MOUNTED VOLUME WAITS FOR THE VERDICT; ONE JOB ASKS.
		if b.allow() {
			t.Fatal("probation let what decides nothing ask")
		}
		if !b.admit() {
			t.Fatal("probation let no job try the store")
		}
		if b.admit() {
			t.Fatal("probation let a second job try while the first was out")
		}

		// ONE FAILURE ON PROBATION REOPENS IT AT ONCE.
		b.outcome(aClone("trial"), errStoreGone)
		if got := b.report(); got.State != lease.CacheStoreOpen || got.Failures != 1 {
			t.Fatalf("after a failure on probation: %+v", got)
		}

		// A VOLUME LATE FROM BEFORE IS NOT A PROBE.
		b.outcome(aClone("late"), nil)
		if got := b.report().State; got != lease.CacheStoreOpen {
			t.Fatalf("a call's answer moved an open breaker to %q", got)
		}

		// THE NEXT PROBATION HAS A TRIAL OF ITS OWN.
		time.Sleep(breakerProbeEvery)
		synctest.Wait()
		if !b.admit() {
			t.Fatal("a new probation held the last one's trial")
		}

		// NOTHING BUT A VOLUME ENDS IT: A LOOKUP AND A CLONE'S MISS BOTH CAN BE ANSWERED
		// WITH WHAT A JOB NEEDS OUT OF REACH.
		b.outcome(aLookup("trial"), nil)
		b.outcome(aClone("trial"), storecontract.ErrMiss)
		if got := b.report().State; got != lease.CacheStoreProbation {
			t.Fatalf("an answer that handed over no volume moved probation to %q", got)
		}

		// AND ONE HANDED OVER CLOSES IT.
		b.outcome(aClone("trial"), nil)
		if got := b.report(); got.State != lease.CacheStoreClosed || got.Failures != 0 || got.Reason != "" {
			t.Fatalf("report = %+v, want closed with nothing carried", got)
		}
		if !b.allow() || !b.admit() || !b.admit() {
			t.Fatal("a closed breaker refused someone")
		}

		// CLOSED AGAIN, IT STARTS COUNTING FROM NOTHING.
		b.outcome(aClone("one"), errStoreGone)
		b.outcome(aClone("two"), errStoreGone)
		if !b.allow() {
			t.Fatal("failures before the close were still counted")
		}

		cancel()
		synctest.Wait()
	})
}

// A JOB THAT WENT AWAY, OR NEVER NEEDED A VOLUME, SAYS NOTHING, and must not hold the
// store's recovery: the next job may try once its time is up.
func TestATrialThatNeverAnswersLetsTheNextJobTry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		b, _ := testBreaker()
		go b.run(ctx, func(context.Context) error { return nil })
		failThree(b)
		time.Sleep(breakerProbeEvery)
		synctest.Wait()

		if !b.admit() {
			t.Fatal("probation let no job try the store")
		}
		b.outcome(aClone("trial"), context.Canceled)
		time.Sleep(testTrial - time.Second)
		if b.admit() {
			t.Fatal("a second job tried inside the first's time")
		}
		time.Sleep(time.Second)
		if !b.admit() {
			t.Fatal("a trial that never answered held probation past its time")
		}

		cancel()
		synctest.Wait()
	})
}

func TestANilBreakerAllowsEverythingAndCountsNothing(t *testing.T) {
	t.Parallel()

	var b *storeBreaker
	if !b.allow() || !b.admit() {
		t.Error("a store nobody guards was refused")
	}
	b.outcome(aClone("one"), errStoreGone)
	b.watch(aClone("one"), time.Hour)(errStoreGone)
}
