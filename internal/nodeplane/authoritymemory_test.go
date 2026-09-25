package nodeplane

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/server"
)

// GITHUB IS ASKED ONCE ABOUT A JOB WHOSE ANSWER IT DECIDED, however many of the
// job's calls ask at once, and every one of them gets that job's answer. The
// callers run in a bubble, so synctest.Wait proves every one of them is parked
// on the question in flight before it is answered.
func TestAJobsAuthorityIsAskedOnceAndRemembered(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var memory authorityMemory
		var asked atomic.Int64
		now := time.Now()
		binding := alloc.PoolRunner{JobID: "job-1", RunID: 31}
		want := server.CacheAuthority{LeaseID: "l1", JobID: "job-1", RunID: 31, Proven: true, WriteOwnRef: true}
		release := make(chan struct{})
		decided := func() (server.CacheAuthority, bool) {
			asked.Add(1)
			<-release

			return want, true
		}

		const callers = 8
		answers := make(chan server.CacheAuthority, callers)
		for range callers {
			go func() { answers <- memory.resolve("l1", binding, now, decided) }()
		}
		synctest.Wait()
		if got := asked.Load(); got != 1 || len(answers) != 0 {
			t.Errorf("with every caller waiting, GitHub was asked %d times and %d answered; want one question",
				got, len(answers))
		}
		close(release)
		for range callers {
			if got := <-answers; got != want {
				t.Errorf("a caller was answered %+v, want %+v", got, want)
			}
		}
		if got := memory.resolve("l1", binding, now.Add(authorityDecidedFor-time.Second), decided); got != want {
			t.Errorf("the remembered answer was %+v, want %+v", got, want)
		}
		if asked.Load() != 1 {
			t.Errorf("GitHub was asked %d times about one decided job, want once", asked.Load())
		}
	})
}

// ANOTHER JOB IS NEVER ANSWERED FROM A JOB'S QUESTION, nor waits behind it: a
// different key is asked and answered while the first is still in flight.
func TestAnotherJobIsAskedOnItsOwnWhileTheFirstIsInFlight(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var memory authorityMemory
		now := time.Now()
		first := server.CacheAuthority{LeaseID: "l1", JobID: "job-1", RunID: 31, Proven: true}
		second := server.CacheAuthority{LeaseID: "l1", JobID: "job-2", RunID: 32}
		release := make(chan struct{})
		firstDone, secondDone := make(chan server.CacheAuthority, 1), make(chan server.CacheAuthority, 1)
		defer func() {
			close(release)
			if got := <-firstDone; got != first {
				t.Errorf("the first job was answered %+v, want its own %+v", got, first)
			}
		}()
		go func() {
			firstDone <- memory.resolve("l1", alloc.PoolRunner{JobID: "job-1", RunID: 31}, now,
				func() (server.CacheAuthority, bool) {
					<-release

					return first, true
				})
		}()
		synctest.Wait()
		go func() {
			secondDone <- memory.resolve("l1", alloc.PoolRunner{JobID: "job-2", RunID: 32}, now,
				func() (server.CacheAuthority, bool) { return second, true })
		}()
		synctest.Wait()
		select {
		case got := <-secondDone:
			if got != second {
				t.Errorf("the second job was answered %+v, want its own %+v", got, second)
			}
		default:
			t.Error("the second job waited behind the first job's question")
			close(release)
			release = make(chan struct{})
			<-secondDone
		}
	})
}

// A COULD-NOT-TELL ANSWER STANDS ONLY ITS BRIEF WHILE, and is never kept as
// though GitHub had decided it.
func TestACouldNotTellAnswerIsAskedAgainSoon(t *testing.T) {
	t.Parallel()

	var memory authorityMemory
	now := time.Now()
	binding := alloc.PoolRunner{JobID: "job-1", RunID: 31}
	var asked atomic.Int64
	unproven := server.CacheAuthority{LeaseID: "l2", JobID: "job-1", RunID: 31}
	couldNotTell := func() (server.CacheAuthority, bool) {
		asked.Add(1)

		return unproven, false
	}
	for _, at := range []time.Duration{0, authorityUndecidedFor - time.Second, authorityUndecidedFor} {
		if got := memory.resolve("l2", binding, now.Add(at), couldNotTell); got != unproven {
			t.Fatalf("at %s the answer was %+v, want %+v", at, got, unproven)
		}
	}
	if asked.Load() != 2 {
		t.Fatalf("a could-not-tell answer was asked %d times, want twice across its interval", asked.Load())
	}
}

// A QUESTION THAT PANICS STILL ANSWERS ITS WAITERS, with nothing proven.
func TestAPanickingQuestionReleasesItsWaiters(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var memory authorityMemory
		now := time.Now()
		binding := alloc.PoolRunner{JobID: "job-1", RunID: 31}
		release := make(chan struct{})
		go func() {
			defer func() { _ = recover() }()
			memory.resolve("l1", binding, now, func() (server.CacheAuthority, bool) {
				<-release
				panic("github client")
			})
		}()
		synctest.Wait()
		waiter := make(chan server.CacheAuthority, 1)
		go func() {
			waiter <- memory.resolve("l1", binding, now, func() (server.CacheAuthority, bool) {
				return server.CacheAuthority{Proven: true}, true
			})
		}()
		synctest.Wait()
		close(release)
		if got := <-waiter; got.Proven {
			t.Errorf("a waiter behind a panicked question was answered %+v, want nothing proven", got)
		}
	})
}
