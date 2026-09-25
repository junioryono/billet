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
// job's calls ask at once, and again only for another job, or once a
// could-not-tell answer has stood its brief while.
func TestAJobsAuthorityIsAskedOnceAndRemembered(t *testing.T) {
	t.Parallel()

	var memory authorityMemory
	var asked atomic.Int64
	now := time.Now()
	binding := alloc.PoolRunner{JobID: "job-1", RunID: 31}
	decided := func() (server.CacheAuthority, bool) {
		asked.Add(1)
		time.Sleep(20 * time.Millisecond)

		return server.CacheAuthority{LeaseID: "l1", RunID: 31}, true
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			memory.resolve("l1", binding, now, decided)
		}()
	}
	wg.Wait()
	memory.resolve("l1", binding, now.Add(authorityDecidedFor-time.Second), decided)
	if asked.Load() != 1 {
		t.Fatalf("GitHub was asked %d times about one decided job, want once", asked.Load())
	}
	memory.resolve("l1", alloc.PoolRunner{JobID: "job-2", RunID: 32}, now, decided)
	if asked.Load() != 2 {
		t.Fatalf("another job was answered from the first one's memory")
	}

	var undecided atomic.Int64
	couldNotTell := func() (server.CacheAuthority, bool) {
		undecided.Add(1)

		return server.CacheAuthority{LeaseID: "l2"}, false
	}
	memory.resolve("l2", binding, now, couldNotTell)
	memory.resolve("l2", binding, now.Add(authorityUndecidedFor-time.Second), couldNotTell)
	memory.resolve("l2", binding, now.Add(authorityUndecidedFor), couldNotTell)
	if undecided.Load() != 2 {
		t.Fatalf("a could-not-tell answer was asked %d times, want twice across its interval", undecided.Load())
	}
}
