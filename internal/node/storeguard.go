package node

import (
	"context"
	"errors"
	"time"

	"github.com/junioryono/billet/internal/lease"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	// storeReportLimit bounds telling the control plane what the breaker did, and
	// storeReportRetry is how soon a report that failed is sent again.
	storeReportLimit = 5 * time.Second
	storeReportRetry = 30 * time.Second
	// storeProbeKey is what a probe asks the store about. Nothing publishes it, so
	// the answer is a miss, and a miss is the store answering.
	storeProbeKey = "billet-store-probe"
)

// errStoreOpen says nothing asked the store, because it had stopped answering.
var errStoreOpen = errors.New("node: the site's store is not answering, so it was not asked")

// errStoreLate says the store did not answer in time. The call goes on without the
// caller; see acquireVolume.
var errStoreLate = errors.New("node: the site's store did not answer in time")

// CacheStoreReporter is told what a node's breaker did, so `billet status` can say so.
// The node client is one.
type CacheStoreReporter interface {
	ReportCacheStore(ctx context.Context, report lease.CacheStoreReport) error
}

// GuardStore puts the service's store behind a breaker, before the listener starts.
// deadline is how long a job waits for it to hand over a volume. A service left
// unguarded asks its store as it always did.
func (s *CacheService) GuardStore(store string, deadline time.Duration) {
	s.storeChanged = make(chan struct{}, 1)
	s.storeDeadline = deadline
	s.breaker = newStoreBreaker(store, deadline, s.log, s.signalStoreChange)
}

func (s *CacheService) signalStoreChange() {
	select {
	case s.storeChanged <- struct{}{}:
	default:
	}
}

// ResendStoreState says what the store's breaker holds again, whether or not it has
// moved: a registration is a new process or a new control plane, and the ledger keeps
// a report only under the registration in force.
func (s *CacheService) ResendStoreState() {
	if s.breaker == nil {
		return
	}
	s.storeResend.Store(true)
	s.signalStoreChange()
}

// StoreBreakerOpen says which store this node guards and whether jobs are skipping
// it, and last whether there is a breaker: a store nobody guards has nothing to report.
func (s *CacheService) StoreBreakerOpen() (string, bool, bool) {
	if s.breaker == nil {
		return "", false, false
	}

	return s.breaker.store, s.breaker.isOpen(), true
}

// RunStoreGuard probes an open breaker's store and tells reporter whenever the breaker
// moves, until ctx ends. A store nobody guards returns at once.
//
// THE PROBE RUNS BESIDE THE REPORT, because a plane that is slow to hear about a store
// must not keep the store's own recovery waiting.
func (s *CacheService) RunStoreGuard(ctx context.Context, reporter CacheStoreReporter) {
	if s.breaker == nil {
		return
	}

	probed := make(chan struct{})
	go func() {
		defer close(probed)
		s.breaker.run(ctx, func(probeCtx context.Context) error {
			_, err := s.store.Current(probeCtx, s.qualifiedKey(storeProbeKey))

			return err
		})
	}()
	defer func() { <-probed }()

	s.reportStoreState(ctx, reporter)
}

// reportStoreState sends what the breaker holds whenever it moves, and again until a
// report lands: the latest state, not each one, since only the state now is any use.
func (s *CacheService) reportStoreState(ctx context.Context, reporter CacheStoreReporter) {
	retry := time.NewTicker(storeReportRetry)
	defer retry.Stop()

	var sent lease.CacheStoreReport
	settled := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.storeChanged:
		case <-retry.C:
		}

		report := s.breaker.report()
		resend := s.storeResend.Swap(false)
		if settled && report == sent && !resend {
			continue
		}
		reportCtx, cancel := context.WithTimeout(ctx, storeReportLimit)
		err := reporter.ReportCacheStore(reportCtx, report)
		cancel()
		if err != nil {
			// A STORE IN USE IS NOT NEWS, and a node not yet registered says so every time.
			if ctx.Err() == nil && report.State != lease.CacheStoreClosed {
				s.log.Warn("could not tell the control plane what the site's store is doing; "+
					"will retry", "store", report.Store, "state", report.State, "error", err)
			}
			settled = false

			continue
		}
		sent, settled = report, true
	}
}

// asked runs one question that may be cancelled, under the store's deadline when it
// has one, and counts what it came to. A REQUEST'S OWN TIME RUNNING OUT SAYS NOTHING OF
// THE STORE: a guest names its own deadline, and three of them would open the breaker.
func (s *CacheService) asked(
	ctx context.Context, session *cacheSession, op string, question func(context.Context) error,
) error {
	if s.breaker == nil {
		return question(ctx)
	}
	askCtx, cancel := context.WithTimeout(ctx, s.storeDeadline)
	defer cancel()

	done := s.breaker.watch(storeCall{op: op, who: session.instance}, s.storeDeadline)
	err := question(askCtx)
	if ctx.Err() != nil {
		done(context.Canceled)

		return err
	}
	done(err)

	return err
}

// storeAcquired is what asking the store for a volume came to.
type storeAcquired struct {
	volume storecontract.Volume
	cold   bool
	err    error
}

// acquireVolume asks the store for a volume a job is waiting on. Called with
// session.mu held; acquire must not take it.
//
// PAST THE DEADLINE THE JOB GOES ON WITHOUT IT, and the call goes on without the job.
// A store that is slow or hung must cost the job a cold start and no more, so the
// answer comes at the deadline, but a call cut short can leave a clone or a mapping
// behind, so nothing cancels it before limit. What it comes to is settled off the
// request's path by finishLateAcquisition, and the session cannot finish until it has.
//
// limit alone bounds the whole call: a guest names its own request's deadline, and the
// late path already owns what the call comes to. What the breaker hears is the store's
// own calls inside acquire (handOver), so nothing else the call does counts against it.
//
// ONE CALL OUT PER SESSION, AND NONE WHILE IT HOLDS WHAT ONE LEFT: a guest that hangs up
// on each request would otherwise start as many detached calls as it can send requests,
// each holding a place in the store's queue that no job is waiting on, and leave as many
// volumes as it likes outside its slots when their discards fail. On probation, or with
// the breaker open, it answers errStoreOpen with the store unasked.
func (s *CacheService) acquireVolume(
	parent context.Context, session *cacheSession, limit time.Duration,
	acquire func(context.Context) (storecontract.Volume, bool, error),
) (storecontract.Volume, bool, error) {
	if s.breaker == nil {
		ctx, cancel := context.WithTimeout(parent, limit)
		defer cancel()

		return acquire(ctx)
	}
	if session.acquiring > 0 || len(session.late) > 0 {
		return storecontract.Volume{}, false, errStoreLate
	}
	if !s.breaker.admit() {
		return storecontract.Volume{}, false, errStoreOpen
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), limit)

	result := make(chan storeAcquired, 1)
	go func() {
		defer cancel()

		volume, cold, err := acquire(ctx)
		result <- storeAcquired{volume: volume, cold: cold, err: err}
	}()

	timer := time.NewTimer(s.storeDeadline)
	defer timer.Stop()
	select {
	case got := <-result:
		return got.volume, got.cold, got.err
	case <-timer.C:
	case <-parent.Done():
	}

	session.acquiring++
	go s.finishLateAcquisition(context.WithoutCancel(parent), session, result)

	return storecontract.Volume{}, false, errStoreLate
}

// finishLateAcquisition settles a call no job is waiting on any more: a volume it
// comes back with goes into the session's durable custody and is discarded, so a crash
// or a failed discard leaves it for the session's cleanup.
func (s *CacheService) finishLateAcquisition(
	parent context.Context, session *cacheSession, result <-chan storeAcquired,
) {
	late := <-result

	session.mu.Lock()
	session.acquiring--
	keep := late.err == nil && late.volume.Handle != ""
	if keep {
		session.late = append(session.late, late.volume)
		if err := s.persistSession(session); err != nil {
			s.log.Warn("could not record a cache volume that came back too late; it is kept in memory "+
				"and discarded now", "instance", session.instance, "error", err)
		}
	}
	closed := session.closed
	session.mu.Unlock()

	if keep {
		ctx, cancel := context.WithTimeout(parent, cacheWorkLimit)
		defer cancel()
		s.discardLateVolume(ctx, session, late.volume)
	}
	if closed {
		s.signalClosed()
	}
}

// discardLateVolume discards one volume no job took and forgets it. One that cannot
// be discarded stays in custody for the session's cleanup, on a later pass.
func (s *CacheService) discardLateVolume(
	ctx context.Context, session *cacheSession, volume storecontract.Volume,
) {
	if err := s.store.Discard(ctx, volume); err != nil {
		s.log.Warn("could not discard a cache volume that came back too late; the session's cleanup "+
			"will retry", "instance", session.instance, "error", err)

		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if !session.forgetLateVolume(volume) || session.finished {
		return
	}
	if err := s.persistSession(session); err != nil {
		s.log.Warn("could not record that a late cache volume was discarded; the cleanup finds it gone",
			"instance", session.instance, "error", err)
	}
}

// forgetLateVolume removes one volume from what the session holds. Called with
// session.mu held.
func (session *cacheSession) forgetLateVolume(volume storecontract.Volume) bool {
	for i := range session.late {
		if session.late[i].Handle == volume.Handle {
			session.late = append(session.late[:i], session.late[i+1:]...)

			return true
		}
	}

	return false
}

// handOver asks the store for one volume a job is waiting on, counting what it comes
// to from the deadline on while it still waits. A success is what ends a probation.
func (s *CacheService) handOver(
	session string, op string, question func() (storecontract.Volume, error),
) (storecontract.Volume, error) {
	done := s.breaker.watch(storeCall{op: op, who: session, decides: true}, s.storeDeadline)
	volume, err := question()
	done(err)

	return volume, err
}

// countedPublication counts one publication step. ONLY A TIMEOUT AGAINST THE STORE: a
// refusal here is as likely about the candidate as the store (a filesystem that is not
// clean, a key another writer holds), and sending every job cold over one job's bad
// write would make a bad build everyone's outage.
//
// NOT THROUGH outcome, WHICH READS A CONFLICT ANYWHERE IN THE CHAIN AS AN ANSWER: a retried
// publication joins its first call's conflict to its second's timeout, and the timeout is
// the last thing the store did.
func (s *CacheService) countedPublication(session, op string, err error) {
	if s.breaker != nil && errors.Is(err, context.DeadlineExceeded) {
		s.breaker.failed(storeCall{op: op, who: session}, lease.CacheStoreTimeout)
	}
}

// countedPublish counts the step that moves a key's pointer, whose success, having
// needed the index lock, settles a probation as a volume handed over does: on a node
// with no job to ask for a volume, a publication is all that could.
func (s *CacheService) countedPublish(session string, err error) {
	if err == nil {
		s.breaker.outcome(storeCall{op: "a cache publication", who: session, decides: true}, nil)

		return
	}
	s.countedPublication(session, "a cache publication", err)
}
