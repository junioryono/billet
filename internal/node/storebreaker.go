package node

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/lease"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	// breakerFailures within breakerWindow open the breaker: enough that one job's
	// bad clone costs nobody else anything, and few enough that a store that has
	// stopped answering costs only the jobs already waiting on it. THE WINDOW OUTLASTS
	// THREE DEADLINES, or a site launching one job at a time, each failing a deadline
	// after the last, would age its first failure out before its third and never open.
	breakerFailures = 3
	breakerWindow   = 5 * time.Minute
	// breakerProbeEvery is how often an open breaker asks the store whether it
	// answers, each question bounded by breakerProbeLimit.
	breakerProbeEvery = 30 * time.Second
	breakerProbeLimit = 10 * time.Second
	// casReadLimit bounds the first byte of a content-addressed read. It unblocks
	// nothing, because a read of a volume whose OSDs are gone waits in the kernel;
	// it is what lets that wait count, so no other job's read joins it.
	casReadLimit = 15 * time.Second
)

// storeBreaker decides whether this node asks its site's store for a cache.
//
// IT DECIDES ONLY THAT. Never capacity, custody or teardown, and a timeout is never
// proof a volume is gone: a store that stopped answering could not tell, and every
// volume in custody stays there until the store says otherwise.
//
// ONLY WHAT A JOB WAITS ON COUNTS: clones, creates, lookups, publications and
// content-addressed reads. A discard, a renewal, eviction and the trash purge do not,
// because on 2026-09-25 every `rbd rm` failed for an hour while clones worked, and
// counting those would have sent every job cold over a problem in the trash.
//
// THE ZERO VALUE IS CLOSED, and it holds nothing across a restart: a node that comes
// up beside a dead store costs three more jobs a deadline each, and a record of a
// verdict that stopped being true would cost every job its cache until a probe ran.
type storeBreaker struct {
	store    string
	log      *slog.Logger
	changed  func()
	mu       sync.Mutex
	state    lease.CacheStoreState
	since    time.Time
	reason   lease.CacheStoreReason
	failures []time.Time
	opened   int
}

func newStoreBreaker(store string, log *slog.Logger, changed func()) *storeBreaker {
	return &storeBreaker{
		store: store, log: log, changed: changed,
		state: lease.CacheStoreClosed, since: time.Now(),
	}
}

// allow says whether a cache may ask the store. A nil breaker, on a store nobody
// guards, always may.
func (b *storeBreaker) allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state != lease.CacheStoreOpen
}

// report is the breaker's state as the control plane is told it.
func (b *storeBreaker) report() lease.CacheStoreReport {
	b.mu.Lock()
	defer b.mu.Unlock()

	report := lease.CacheStoreReport{Store: b.store, State: b.state, Since: b.since.UTC()}
	if b.state != lease.CacheStoreClosed {
		report.Failures, report.Reason = b.opened, b.reason
	}

	return report
}

// isOpen says whether jobs are skipping the store right now, what the gauge reports.
func (b *storeBreaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state == lease.CacheStoreOpen
}

// storeAnswered says the store gave an answer, whatever it was. A miss and a conflict
// are the store working; so is a read of an object that is not there.
func storeAnswered(err error) bool {
	return err == nil || errors.Is(err, storecontract.ErrMiss) ||
		errors.Is(err, storecontract.ErrConflict) || errors.Is(err, fs.ErrNotExist)
}

// outcome counts what one call the store was asked came to. A caller that went away
// says nothing about the store.
func (b *storeBreaker) outcome(op string, err error) {
	if b == nil || errors.Is(err, context.Canceled) {
		return
	}
	if storeAnswered(err) {
		b.answered()

		return
	}

	reason := lease.CacheStoreError
	if errors.Is(err, context.DeadlineExceeded) {
		reason = lease.CacheStoreTimeout
	}
	b.failed(op, reason)
}

// answered is a call the store answered: what ends a probation. It closes nothing an
// open breaker holds, which only a probe does, because a call finishing now may have
// started long before the store stopped answering.
func (b *storeBreaker) answered() {
	b.mu.Lock()
	if b.state != lease.CacheStoreProbation {
		b.mu.Unlock()

		return
	}
	b.state, b.since = lease.CacheStoreClosed, time.Now()
	b.failures = nil
	b.mu.Unlock()

	b.log.Info("the site's store is answering again, so jobs use their caches", "store", b.store)
	b.changed()
}

// failed counts one failure. Within breakerWindow of breakerFailures, or at once on
// probation, jobs stop asking.
func (b *storeBreaker) failed(op string, reason lease.CacheStoreReason) {
	now := time.Now()

	b.mu.Lock()
	switch b.state {
	case lease.CacheStoreOpen:
		b.mu.Unlock()

		return
	case lease.CacheStoreProbation:
		b.opened = 1
	default:
		kept := b.failures[:0]
		for _, at := range b.failures {
			if now.Sub(at) < breakerWindow {
				kept = append(kept, at)
			}
		}
		kept = append(kept, now)
		b.failures = kept
		if len(b.failures) < breakerFailures {
			b.mu.Unlock()

			return
		}
		b.opened = len(b.failures)
	}
	b.state, b.since, b.reason = lease.CacheStoreOpen, now, reason
	b.failures = nil
	failures := b.opened
	b.mu.Unlock()

	b.log.Warn("the site's store is not answering, so new jobs start cold and caches answer a "+
		"miss until it does", "store", b.store, "failures", failures, "reason", reason, "last", op)
	b.changed()
}

// watch counts one call the store was asked, as a timeout from limit on if it has not
// returned by then, so a call stuck where nothing can cancel it still counts while it
// waits. What it returns reports the call's outcome, and counts it only if the timer
// has not.
func (b *storeBreaker) watch(op string, limit time.Duration) func(error) {
	if b == nil {
		return func(error) {}
	}

	var once sync.Once
	timer := time.AfterFunc(limit, func() {
		once.Do(func() { b.failed(op, lease.CacheStoreTimeout) })
	})

	return func(err error) {
		timer.Stop()
		once.Do(func() { b.outcome(op, err) })
	}
}

// run asks the store every breakerProbeEvery while the breaker is open, until ctx
// ends. A store that answers goes on probation rather than straight back, because
// what the probe reads is not everything a job needs: the index can answer while its
// lock is out of reach, so the next real call decides.
func (b *storeBreaker) run(ctx context.Context, probe func(context.Context) error) {
	ticker := time.NewTicker(breakerProbeEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !b.isOpen() {
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, breakerProbeLimit)
		err := probe(probeCtx)
		cancel()
		// COULD NOT TELL IS NOT HEALTHY: anything but an answer leaves it open.
		if !storeAnswered(err) || ctx.Err() != nil {
			continue
		}

		b.mu.Lock()
		// A CALL THE STORE WAS ASKED BEFORE IT STOPPED ANSWERING cannot have moved an
		// open breaker, so nothing else can be here but this loop.
		b.state, b.since = lease.CacheStoreProbation, time.Now()
		b.mu.Unlock()

		b.log.Info("the site's store answered a probe; the next job's cache decides whether it "+
			"is back", "store", b.store)
		b.changed()
	}
}
