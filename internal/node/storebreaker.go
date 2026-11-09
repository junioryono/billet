package node

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/lease"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	// breakerFailures different jobs failing within breakerWindow, with nothing
	// answered between them, open the breaker: enough that no one job can cost anyone
	// else anything, and few enough that a store that has stopped answering costs only
	// the jobs already waiting on it. THE WINDOW OUTLASTS THREE DEADLINES, or a site
	// launching one job at a time, each failing a deadline after the last, would age
	// its first failure out before its third and never open.
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

// storeCall is one thing a job asked the store.
type storeCall struct {
	op string
	// who is the session that asked. One session counts once, whatever it does: a
	// job can make the store refuse it, and must not be able to turn every other
	// job's caches off by doing so.
	who string
	// decides says the call needs the index lock and the pool: it hands over a volume or
	// moves a key's pointer. So its success can end a probation or clear a count. A
	// lookup and a read are lock-free or served from memory, and a clone's miss says only
	// that a lookup worked: the store can answer those with what a job needs out of reach.
	decides bool
}

// storeBreaker decides whether this node asks its site's store for a cache.
//
// IT DECIDES ONLY THAT. Never capacity, custody or teardown, and a timeout is never
// proof a volume is gone: a store that stopped answering could not tell, and every
// volume in custody stays there until the store says otherwise.
//
// ONLY WHAT A JOB WAITS ON COUNTS: clones, creates, lookups, publications' timeouts and
// content-addressed reads. A discard, a renewal, eviction and the trash purge do not,
// because on 2026-09-25 every `rbd rm` failed for an hour while clones worked, and
// counting those would have sent every job cold over a problem in the trash.
//
// THE ZERO VALUE IS CLOSED, and it holds nothing across a restart: a node that comes
// up beside a dead store costs three more jobs a deadline each, and a record of a
// verdict that stopped being true would cost every job its cache until a probe ran.
type storeBreaker struct {
	store   string
	log     *slog.Logger
	changed func()

	mu       sync.Mutex
	state    lease.CacheStoreState
	since    time.Time
	reason   lease.CacheStoreReason
	failures []storeFailure
	opened   int
	// trialFor is how long one job has to settle a probation, and trialUntil when the
	// one out now stops holding it: a job that went away, or never needed a volume,
	// says nothing, and must not hold the store's recovery.
	trialFor   time.Duration
	trialUntil time.Time
}

// storeFailure is one session's latest failure inside the window.
type storeFailure struct {
	at  time.Time
	who string
}

func newStoreBreaker(store string, trialFor time.Duration, log *slog.Logger, changed func()) *storeBreaker {
	return &storeBreaker{
		store: store, log: log, changed: changed, trialFor: trialFor,
		state: lease.CacheStoreClosed, since: time.Now(),
	}
}

// allow says whether a call that does not settle a probation may ask the store. A nil
// breaker, on a store nobody guards, always may.
func (b *storeBreaker) allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state == lease.CacheStoreClosed
}

// admit says whether this job may ask the store for what would settle a probation, a
// volume or a publication, and is asked right before it does, so that nothing that goes
// on to ask nothing can hold a trial. ONE AT A TIME
// ON PROBATION, because a probe's answer is not everything a job needs, and every job
// let through while a trial was out would wait a deadline if it fails.
func (b *storeBreaker) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case lease.CacheStoreClosed:
		return true
	case lease.CacheStoreProbation:
		if now := time.Now(); !now.Before(b.trialUntil) {
			b.trialUntil = now.Add(b.trialFor)

			return true
		}
	}

	return false
}

// unused gives back a probation's trial that admit gave a job which then asked
// the store nothing, as a publication the kill switch refused does, so the next
// job takes it rather than wait out trialFor. A job that did ask settled it
// already, and a breaker not on probation holds no trial.
func (b *storeBreaker) unused() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == lease.CacheStoreProbation {
		b.trialUntil = time.Time{}
	}
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

// storeAnswered says the store gave an answer, whatever it was. A miss and a conflict
// are the store working.
func storeAnswered(err error) bool {
	return err == nil || errors.Is(err, storecontract.ErrMiss) ||
		errors.Is(err, storecontract.ErrConflict)
}

// outcome counts what one call the store was asked came to. A caller that went away
// says nothing about the store.
func (b *storeBreaker) outcome(call storeCall, err error) {
	if b == nil || errors.Is(err, context.Canceled) {
		return
	}
	if storeAnswered(err) {
		if err == nil && call.decides {
			b.answered()
		}

		return
	}

	reason := lease.CacheStoreError
	if errors.Is(err, context.DeadlineExceeded) {
		reason = lease.CacheStoreTimeout
	}
	b.failed(call, reason)
}

// answered is the store doing what a job needs: what ends a probation, and what says the
// failures so far were not the store. It closes nothing an open breaker holds, which
// only a probe moves, because a call finishing now may have started long before the
// store stopped answering.
func (b *storeBreaker) answered() {
	b.mu.Lock()
	b.failures = nil
	if b.state != lease.CacheStoreProbation {
		b.mu.Unlock()

		return
	}
	b.state, b.since, b.trialUntil = lease.CacheStoreClosed, time.Now(), time.Time{}
	b.mu.Unlock()

	b.log.Info("the site's store is answering again, so jobs use their caches", "store", b.store)
	b.changed()
}

// failed counts one failure. Different sessions' reaching breakerFailures within
// breakerWindow, or any at all on probation, stops jobs asking.
func (b *storeBreaker) failed(call storeCall, reason lease.CacheStoreReason) {
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
		for _, failure := range b.failures {
			if now.Sub(failure.at) < breakerWindow && failure.who != call.who {
				kept = append(kept, failure)
			}
		}
		kept = append(kept, storeFailure{at: now, who: call.who})
		b.failures = kept
		if len(b.failures) < breakerFailures {
			b.mu.Unlock()

			return
		}
		b.opened = len(b.failures)
	}
	b.state, b.since, b.reason, b.trialUntil = lease.CacheStoreOpen, now, reason, time.Time{}
	b.failures = nil
	failures := b.opened
	b.mu.Unlock()

	b.log.Warn("the site's store is not answering, so new jobs start cold and caches answer a "+
		"miss until it does", "store", b.store, "failures", failures, "reason", reason, "last", call.op)
	b.changed()
}

// watch counts one call the store was asked, as a timeout from limit on if it has not
// returned by then, so a call stuck where nothing can cancel it still counts while it
// waits. What it returns reports the call's outcome, and counts it only if the timer
// has not; it may be called more than once, and the first counts.
func (b *storeBreaker) watch(call storeCall, limit time.Duration) func(error) {
	if b == nil {
		return func(error) {}
	}

	var once sync.Once
	timer := time.AfterFunc(limit, func() {
		once.Do(func() { b.failed(call, lease.CacheStoreTimeout) })
	})

	return func(err error) {
		timer.Stop()
		once.Do(func() { b.outcome(call, err) })
	}
}

// run asks the store every breakerProbeEvery while the breaker is open, until ctx
// ends. A store that answers goes on probation rather than straight back, because
// what the probe reads is not everything a job needs: the index can answer while its
// lock is out of reach, so the next job to ask for a volume, or to publish, decides.
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

		// Only this loop leaves open, so nothing can have moved it since the look above.
		b.mu.Lock()
		b.state, b.since, b.trialUntil = lease.CacheStoreProbation, time.Now(), time.Time{}
		b.mu.Unlock()

		b.log.Info("the site's store answered a probe; the next job to use it decides "+
			"whether it is back", "store", b.store)
		b.changed()
	}
}

func (b *storeBreaker) isOpen() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.state == lease.CacheStoreOpen
}
