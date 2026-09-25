package nodeplane

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/server"
)

// GITHUB IS ASKED ONCE ABOUT A JOB WHOSE ANSWER IT DECIDED, however many of the
// job's calls ask at once, and every one of them gets that job's answer.
func TestAJobsAuthorityIsAskedOnceAndRemembered(t *testing.T) {
	t.Parallel()

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
	var started sync.WaitGroup
	for range callers {
		started.Add(1)
		go func() {
			started.Done()
			answers <- memory.resolve("l1", binding, now, decided)
		}()
	}
	started.Wait()
	for asked.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range callers {
		if got := <-answers; got != want {
			t.Fatalf("a caller was answered %+v, want %+v", got, want)
		}
	}
	if got := memory.resolve("l1", binding, now.Add(authorityDecidedFor-time.Second), decided); got != want {
		t.Fatalf("the remembered answer was %+v, want %+v", got, want)
	}
	if asked.Load() != 1 {
		t.Fatalf("GitHub was asked %d times about one decided job, want once", asked.Load())
	}
}

// ANOTHER JOB IS NEVER ANSWERED FROM A JOB'S QUESTION, nor waits behind it: a
// different key is asked and answered while the first is still in flight.
func TestAnotherJobIsAskedOnItsOwnWhileTheFirstIsInFlight(t *testing.T) {
	t.Parallel()

	var memory authorityMemory
	now := time.Now()
	first := server.CacheAuthority{LeaseID: "l1", JobID: "job-1", RunID: 31, Proven: true}
	second := server.CacheAuthority{LeaseID: "l1", JobID: "job-2", RunID: 32}
	inFlight, release := make(chan struct{}), make(chan struct{})
	done := make(chan server.CacheAuthority, 1)
	go func() {
		done <- memory.resolve("l1", alloc.PoolRunner{JobID: "job-1", RunID: 31}, now,
			func() (server.CacheAuthority, bool) {
				close(inFlight)
				<-release

				return first, true
			})
	}()
	<-inFlight

	got := memory.resolve("l1", alloc.PoolRunner{JobID: "job-2", RunID: 32}, now,
		func() (server.CacheAuthority, bool) { return second, true })
	if got != second {
		t.Fatalf("the second job was answered %+v, want its own %+v", got, second)
	}
	close(release)
	if got := <-done; got != first {
		t.Fatalf("the first job was answered %+v, want %+v", got, first)
	}
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
