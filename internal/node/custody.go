package node

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// custody is a lease whose capacity must keep being held because compute for it may
// exist, and which nothing else in this process is managing.
//
// Two situations produce one:
//
//   - ADOPTED. A container survived a restart. The runner inside is talking to
//     GitHub directly and may finish successfully, so destroying it is a deliberate
//     job failure rather than a recovery. billet cannot manage it, but it can keep
//     the lease alive so the capacity is not resold.
//
//   - DISCARDED. A launch failed ambiguously and cleanup could not be confirmed.
//     Until billet knows the container does not exist, its capacity stays held.
//
// Both need a heartbeat so the reaper does not terminalize the lease, and a repeated
// attempt to find out what happened. The only difference is what a still-running
// instance means — leave it, or kill it.
type custody struct {
	leaseID string
	// epoch is the fencing token this entry renews with.
	//
	// ATOMIC BECAUSE IT MOVES NOW. It was immutable once an entry existed, so a
	// plain field was safe; re-adopting a lease the reaper quarantined under us
	// writes it from Tend while KeepAlive is reading it from its own goroutine —
	// and that separation is the whole point of the two loops, so the write
	// cannot simply borrow Tend's lock.
	epoch    atomic.Int64
	name     string
	instance string

	// requestID is the job the lease was assigned, so a completion arriving for
	// it can end the custody. Without it an adopted container that hangs is held
	// forever: the restarted listener has no record of the request, so its
	// completion handler returns without releasing anything, and nothing else
	// ever connects the message to the compute.
	requestID int64

	// outcome is how the lease should be recorded once its compute is gone. A
	// discarded entry is a launch that FAILED, and writing "done" into the
	// durable history for a runner that never started is a lie a later
	// investigation would have to unpick.
	outcome alloc.Phase

	// phase is the durable, operator-visible kind of hold this entry reports.
	// Custody preserves adopted work; teardown is actively trying to remove it.
	phase alloc.Phase

	// discard is true when the instance must go as soon as it can be found, and
	// false when it is running work that should be allowed to finish.
	//
	// It changes over an entry's life — a completion or a lost lease turns an
	// adoption into a discard — so it says what to do NEXT, not where the entry
	// came from.
	discard bool

	// observed is true once billet has actually seen this instance exist.
	//
	// SEPARATE FROM discard. The grace before an absence is believed exists for compute
	// that may never have started — a create whose response was lost can still be in
	// flight inside the daemon. It says nothing about an instance billet WATCHED
	// running and then found gone, which has genuinely finished.
	//
	// Since discard flips to true when a completion arrives, checking it alone applies
	// the stray grace to every adopted job at the moment it finishes.
	observed bool

	// asked is true once a teardown has been requested and not confirmed, so the
	// log line saying so is written once rather than on every tick.
	//
	// NOT A GUARD AGAINST RE-REQUESTING. The request is re-issued every tick on
	// purpose: it is idempotent, it costs one call, and it is the only thing that
	// recovers a teardown a backend accepted and then did not perform.
	asked bool

	// unconfirmed is true while this entry's COMPUTE has not been proved gone.
	//
	// THE DIFFERENCE BETWEEN AN ENTRY THAT MUST SILENCE THE LISTENER AND ONE THAT
	// MUST NOT, which is a distinction a bare "is anything held for this request"
	// cannot make — and getting it wrong breaks a case in either direction:
	//
	//	unconfirmed   a teardown was accepted and the guest may still be running.
	//	              A Destroy for this request must answer ErrCustody, or the
	//	              caller releases the capacity underneath a live guest (#46).
	//	confirmed     the compute is gone and only the RELEASE failed — the shape a
	//	              redelivered assignment after a crash produces, where custody
	//	              holds one lease and the listener holds another for the same
	//	              request. Answering ErrCustody there strands the listener's own
	//	              lease, whose container really was destroyed, because the
	//	              listener drops the reference without releasing it.
	//
	// Set when a teardown could not be confirmed, cleared when it is. An entry
	// that reaches a confirmed teardown is normally deleted by finish, so a
	// surviving entry with this false is one whose release did not land.
	//
	// ATOMIC FOR THE SAME REASON epoch IS. tendOne writes it while holding
	// `tending`; the Destroy path reads it while holding `mu`. Those are different
	// locks by design — Tend must not block on the map mutex — so the field cannot
	// borrow either one's protection.
	unconfirmed atomic.Bool

	// since is when custody was taken, for the diagnostic that matters most: how
	// long capacity has been held for something nobody is watching.
	since time.Time
}

// adopt takes custody of an instance that survived a restart.
func (r *Runner) adopt(lease *alloc.Lease, inst *provider.Instance) {
	outcome := alloc.PhaseDone
	discard := false
	phase := alloc.PhaseCustody
	if lease.FailureReason != "" {
		outcome = alloc.PhaseFailed
		discard = true
		phase = alloc.PhaseTeardown
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := &custody{
		leaseID: lease.ID,

		name:      inst.Name,
		instance:  inst.ID,
		requestID: lease.RequestID,
		outcome:   outcome,
		phase:     phase,
		discard:   discard,
		// Adoption starts from a container billet just watched running.
		observed: true,
		since:    r.now(),
	}
	entry.epoch.Store(lease.Epoch)

	// UNCONFIRMED, BECAUSE AN ADOPTED GUEST IS RUNNING RIGHT NOW. The zero value
	// says "the compute is gone and only the bookkeeping is outstanding", which is
	// the exact opposite of what an adoption is — and a Destroy for this request
	// would then report success on the strength of it, letting the caller release
	// capacity underneath a live job. It is cleared the moment a tend proves the
	// instance gone, which is the ordinary way an adoption ends.
	entry.unconfirmed.Store(true)

	r.custody[lease.ID] = entry
}

// hold takes custody of a lease whose compute could not be confirmed gone.
//
// The instance id is not required and usually not known: the reason this exists is
// that Find could not answer. The name identifies the instance to the backend.
//
// THE REQUEST ID IS PASSED IN, NOT READ OFF THE LEASE. Assign writes it to SQLite
// without touching the caller's in-memory Lease, so the pointer a listener holds
// still carries RequestID 0 — every discarded entry filed under request 0. A later
// assignment for the real request then walks past heldForRequest and starts a
// second runner, and a completion can never find the entry at all.
func (r *Runner) hold(lease *alloc.Lease, name string, requestID int64) {
	// FAILED, not done. This lease's job never started.
	r.holdWithOutcome(lease, name, requestID, alloc.PhaseFailed, false)
}

// holdWithOutcome is hold for a job whose outcome is already known.
//
// THE OUTCOME IS A PARAMETER BECAUSE THERE ARE NOW TWO WAYS TO REACH THIS, and
// they are recorded differently. A launch that could not be confirmed cleaned up
// is a FAILURE — nothing ran. A teardown the backend merely accepted (#46) is a
// job that FINISHED, and is only here because EC2 cannot say when its guest
// stopped. Writing "failed" for the second would put a lie in job_history for
// every job that completed normally on an EC2 host, and an investigation reading
// it would see a fleet where nothing ever succeeded.
func (r *Runner) holdWithOutcome(
	lease *alloc.Lease, name string, requestID int64, outcome alloc.Phase, observed bool,
) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := &custody{
		leaseID: lease.ID,

		name:      name,
		requestID: requestID,
		discard:   true,
		outcome:   outcome,
		phase:     alloc.PhaseTeardown,
		since:     r.now(),
		// An ambiguous launch has not been observed and needs the grace before an
		// absence is believed. A normal teardown starts from an instance billet
		// already tracked, so its first absence proves that known compute is gone.
		observed: observed,
	}
	entry.epoch.Store(lease.Epoch)

	// UNCONFIRMED BY CONSTRUCTION. Both routes here exist because billet could not
	// establish that a piece of compute is gone — a launch whose cleanup Find could
	// not confirm, or a teardown a backend merely accepted. Until something proves
	// otherwise, a Destroy for this request must not report success.
	entry.unconfirmed.Store(true)

	r.custody[lease.ID] = entry
}

// heldLeases reports the leases currently in custody, so a sweep does not treat
// their instances as orphans.
func (r *Runner) heldLeases() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	held := make(map[string]bool, len(r.custody))
	for id := range r.custody {
		held[id] = true
	}

	return held
}

// strayGrace is how long a possible stray is looked for before an absence is
// believed.
//
// The window in which a daemon that accepted a create can still act on it.
//
// SIZED FOR A CREATE THAT INCLUDES A PULL, which is what the docker provider
// actually does: it launches with `docker run --detach`, and an image that is
// not already local is fetched inline. A multi-gigabyte runner image on a slow
// link takes minutes, so "far longer than any container runtime needs" was true
// only of a warm cache.
//
// Cheap to be wrong in one direction and not the other. Waiting costs one
// lease's capacity for a few minutes, which comes back by itself; not waiting
// puts a container on capacity billet has already resold, which nothing
// recovers. The same asymmetry sets alloc.quarantineGrace, and the two are
// deliberately the same size.
const strayGrace = 5 * time.Minute

// custodyWarnAfter is how long a held lease may go unresolved before every tick
// says so.
//
// An hour is longer than most CI jobs and far shorter than the point at which
// the capacity loss stops mattering. The warning is the actual mechanism here:
// the bound below is a backstop that should never fire, and an operator finding
// out about held capacity only when it is destroyed a day later has been told
// too late to do anything about it.
const custodyWarnAfter = time.Hour

// DefaultMaxCustody is OFF, deliberately.
//
// Elapsed time is not evidence that a job has stopped making progress: billet
// imposes no job limit of its own, self-hosted runners are routinely configured
// past GitHub's six-hour default, and a legitimately long job would be killed
// for no reason anyone could see in the logs.
//
// Killing live work must be authorised by something that actually knows — a
// completion from GitHub, an observed process exit, or an operator. Time drives
// the WARNING instead, which is the honest signal: "billet is holding capacity
// it cannot account for, and it has been doing so for two hours."
//
// An operator who does know their longest job can set a bound. Zero means none.
const DefaultMaxCustody = 0

// KeepAlive renews held leases on their own clock until the context ends.
//
// SEPARATE FROM Tend, AND THAT SEPARATION IS THE WHOLE POINT. Tend runs after
// Reap on a shared tick and makes unbounded provider calls, so renewal inside it
// would leave the interval from a successful heartbeat to the following Reap
// unbounded — a slow `docker ps` delays the next renewal without delaying the
// next reap. Anything longer than the lease TTL and the reaper terminalizes a
// lease held on purpose, hands its capacity back, and lets a listener advertise
// it while the container is still running.
//
// So this does exactly one thing, touches only the ledger, and ticks at a third
// of the TTL — the same cadence and the same reasoning as the listener's own
// heartbeats. Two renewals may be missed entirely before anything expires.
func (r *Runner) KeepAlive(ctx context.Context) {
	// THE CADENCE IS RE-READ EVERY CYCLE, and the loop wakes on a short fixed tick
	// rather than a timer armed for the TTL it read last.
	//
	// The TTL is negotiated at registration, and a plane that restarts advertising a
	// SHORTER one leaves a janitor renewing too slowly: the lease expires between two
	// heartbeats, the reaper resells its capacity, and the container it was holding is
	// still running. A timer armed for thirty seconds does not shorten itself when the
	// answer becomes nine, so the tick asks whether a renewal is DUE instead. The cost
	// is a few no-op wakeups a minute. The janitor is started once on purpose —
	// custody outlives any registration — so this is the only place that can correct.
	next := time.Now().Add(r.renewEvery())

	timer := time.NewTimer(watchTick)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			due := r.renewEvery()

			// A SHORTENED CADENCE PULLS THE DEADLINE IN. This is the whole point of
			// waking early: a deadline further away than the current interval allows
			// was set under a TTL that no longer applies, and keeping it would let the
			// lease expire before the next renewal.
			if latest := now.Add(due); next.After(latest) {
				next = latest
			}

			if !now.Before(next) {
				r.renewHeld(ctx)

				next = time.Now().Add(due)
			}

			// Sleep until the deadline, but never longer than one tick, so the next
			// change of cadence is noticed just as quickly.
			wait := watchTick
			if until := time.Until(next); until > 0 && until < wait {
				wait = until
			}

			timer.Reset(wait)
		}
	}
}

// watchTick bounds how long the janitor can be unaware of a shortened TTL.
//
// Short enough that a renegotiated cadence takes effect within one tick, long
// enough that a fleet of idle nodes is not spinning. It is a ceiling on the
// sleep, never the renewal interval itself.
const watchTick = 500 * time.Millisecond

// renewEvery is how long to wait between renewals.
//
// A THIRD OF THE TTL, so two consecutive failures still leave a renewal before
// the deadline the reaper on the other side enforces.
func (r *Runner) renewEvery() time.Duration {
	every := r.ttl() / 3
	if every <= 0 {
		// Only before the first registration answers, and the janitor no longer
		// starts before then. Kept because a cadence of zero is a busy loop.
		return time.Second
	}

	return every
}

// renewHeld heartbeats every held lease once.
//
// Failures are logged rather than returned: a lease that will not renew is
// handled by Tend, which is the only thing allowed to decide that compute should
// go. Doing that here would put teardown on a path that must stay cheap and
// never block.
func (r *Runner) renewHeld(ctx context.Context) {
	for _, c := range r.renewSnapshot() {
		err := r.alloc.Heartbeat(ctx, c.leaseID, c.epoch.Load())
		if err == nil || ctx.Err() != nil {
			continue
		}

		if errors.Is(err, alloc.ErrLeaseNotFound) || errors.Is(err, alloc.ErrFenced) ||
			errors.Is(err, alloc.ErrForceRelease) {
			// Expected once a job finishes: the listener released the lease and Tend
			// is about to clean up the compute. Not worth a line every few seconds.
			continue
		}

		r.log.Warn("could not renew a held lease; the reaper may reclaim its capacity "+
			"while the compute is still running",
			"lease", c.leaseID, "name", c.name, "error", err)
	}
}

// Tend advances everything in custody by one step, and is called on the same
// tick as Sweep.
//
// The heartbeat comes FIRST and its failure is the most informative outcome
// here. A lease that will not heartbeat is one the reaper already terminalized,
// or one whose epoch moved because somebody else took it — either way this
// process no longer holds the capacity, so there is nothing left to protect and
// the instance becomes something to destroy rather than something to preserve.
// That is also the ordinary way an adopted container ends: GitHub reports the
// job complete, the listener releases the lease, and the next Tend finds the
// heartbeat refused and cleans up.
func (r *Runner) Tend(ctx context.Context) error {
	// SERIALIZED. Today one goroutine calls this, but nothing in the Sweeper
	// contract says so, and two concurrent ticks would race on an entry's discard
	// flag and issue duplicate destroys — worse, one could delete an entry the
	// other had just replaced, dropping custody of compute that still exists.
	r.tending.Lock()
	defer r.tending.Unlock()

	var failures []error

	for _, c := range r.custodySnapshot() {
		if err := r.tendOne(ctx, c); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// tendOne advances one custody entry. THE CALLER MUST HOLD r.tending: this
// mutates the entry in place and issues backend calls between reads, so two
// concurrent callers would destroy the same instance twice and could delete an
// entry that had been replaced.
func (r *Runner) tendOne(ctx context.Context, c *custody) error {
	held := r.now().Sub(c.since)

	// Past an operator-set bound, it goes — and it is recorded as a FAILURE,
	// because that is what a forcibly killed job is. Keeping the adopted entry's
	// "done" would archive a teardown billet chose as a job that finished.
	if r.maxCustody > 0 && held > r.maxCustody && !c.discard {
		r.log.Error("a job has been held past the configured limit; destroying it "+
			"and reclaiming its capacity",
			"name", c.name, "lease", c.leaseID, "held_for", held.Round(time.Minute),
			"limit", r.maxCustody)

		c.discard = true
		c.outcome = alloc.PhaseFailed
		if c.phase != "" {
			c.phase = alloc.PhaseTeardown
		}
	} else if held > custodyWarnAfter {
		r.log.Warn("still holding capacity for compute billet is not managing",
			"name", c.name, "lease", c.leaseID, "held_for", held.Round(time.Minute),
			"adopted", !c.discard)
	}

	heartbeatOK := false
	if err := r.alloc.Heartbeat(ctx, c.leaseID, c.epoch.Load()); err != nil {
		if errors.Is(err, alloc.ErrForceRelease) {
			r.log.Warn("an operator forced the release of compute held here; dropping custody",
				"name", c.name, "lease", c.leaseID)
			c.discard = true
			c.outcome = alloc.PhaseFailed
			c.unconfirmed.Store(false)
			r.cleanupCache(ctx, c.name)

			return r.finish(ctx, c)
		}

		if !errors.Is(err, alloc.ErrLeaseNotFound) && !errors.Is(err, alloc.ErrFenced) {
			return fmt.Errorf("node: hold the capacity of lease %s: %w", c.leaseID, err)
		}

		// FENCED IS NOT THE SAME AS GONE, and reading it that way destroyed live
		// jobs. The reaper bumps the epoch when it QUARANTINES a lease — capacity
		// still charged, container still running, nobody else holding it — so a
		// custody entry that predates the quarantine gets ErrFenced from a lease
		// that very much still wants its compute. Discarding on that destroyed the
		// job the quarantine exists to protect, and freed nothing, because the
		// lease was not terminal.
		//
		// So the ledger is asked which it is. A lease still quarantined on this
		// node is re-adopted at its new epoch and stays adopted; only a lease that
		// is genuinely terminal or missing makes its instance an orphan.
		quarantined, err := r.requarantined(ctx, c)
		if err != nil {
			return err
		}

		if !quarantined {
			// The lease is gone. Its capacity is already back in the budget, so the
			// instance is now an orphan whatever it was before.
			c.discard = true
		}
	} else {
		heartbeatOK = true
	}

	// REPORTED ONLY WHILE THIS EPOCH IS CURRENT. A fenced entry may have become
	// quarantine, and writing an older in-memory classification over that would
	// erase the control plane's more conservative verdict.
	if heartbeatOK && c.phase != "" {
		if err := r.alloc.Advance(ctx, c.leaseID, c.epoch.Load(), c.phase); err != nil {
			return fmt.Errorf("node: report %s for held lease %s: %w", c.phase, c.leaseID, err)
		}
	}

	inst, found, err := r.provider.Find(ctx, c.name)
	if err != nil {
		return fmt.Errorf("node: look for %s: %w", c.name, err)
	}

	if !found {
		// AN ABSENCE IS A SNAPSHOT, NOT A CAUSAL RESULT. When a launch loses its
		// response, the daemon may still be processing the create — a `docker ps`
		// issued straight afterwards can overtake it and see nothing, and releasing
		// on that would hand the capacity back moments before the container
		// appears. So a discarded entry has to keep looking for a while before an
		// absence is believed.
		//
		// An entry billet has SEEN is different: its instance existed, so an
		// absence now means it genuinely went away and the capacity can go back
		// immediately.
		if !c.observed && r.now().Sub(c.since) < strayGrace {
			return nil
		}

		// PROVED GONE, by a sustained absence rather than by a backend saying so —
		// which is the only proof an asynchronous teardown ever offers.
		c.unconfirmed.Store(false)
		r.cleanupCache(ctx, c.name)

		return r.finish(ctx, c)
	}

	// Seen it now, whatever it was before. A stray that has finally materialised
	// is no longer a maybe, so a later absence needs no grace period.
	c.observed = true

	// An adopted instance that is still running is left alone. This is the case
	// destructive recovery got wrong: the runner inside is talking to GitHub on
	// its own and the job may well succeed.
	if !c.discard && inst.Running {
		return nil
	}

	state, err := r.provider.Destroy(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("node: destroy %s held for lease %s: %w", c.name, c.leaseID, err)
	}

	// THE SAME TRAP AS THE LISTENER'S, ONE LAYER DOWN (#46). finish releases the
	// lease, and calling it on the strength of an ACCEPTED teardown would free the
	// capacity while the guest was still shutting down — inside the very machinery
	// that exists to stop that happening.
	//
	// Nothing else is needed to recover: the entry stays in custody, so the next
	// tick looks again, and the release happens when Find reports the instance
	// gone. That path is already written above and already applies the grace an
	// unobserved absence needs.
	if state != provider.TeardownStopped {
		c.unconfirmed.Store(true)

		// SAID ONCE, THOUGH THE REQUEST IS RE-ISSUED EVERY TICK. Re-asking is the
		// safety net for a teardown that was accepted and did not take, and it is
		// idempotent and cheap; saying so on every tick for the minute or two an
		// EC2 instance spends shutting down is just noise in the one log an
		// operator reads to find out what a host is holding.
		if !c.asked {
			c.asked = true

			r.log.Info("asked the backend to remove compute being held; it has not confirmed "+
				"the guest stopped, so the capacity stays held",
				"name", c.name, "lease", c.leaseID)
		}

		return nil
	}

	// PROVED GONE. What is left for this entry is bookkeeping — releasing the
	// lease — so a Destroy arriving for its request is answerable with the truth
	// rather than with custody.
	c.unconfirmed.Store(false)
	r.cleanupCache(ctx, c.name)

	r.log.Info("released compute that was being held",
		"name", c.name, "lease", c.leaseID, "adopted", !c.discard,
		"held_for", r.now().Sub(c.since).Round(time.Second))

	return r.finish(ctx, c)
}

// requarantined re-adopts a custody entry whose lease was fenced by a
// quarantine, reporting whether it did.
//
// THE EPOCH MOVES AND NOTHING ELSE DOES. Quarantine is the reaper saying it can
// no longer account for this compute, which is precisely what custody is for —
// so the entry keeps its adopted status and picks up the new epoch, and the next
// tick heartbeats successfully.
func (r *Runner) requarantined(ctx context.Context, c *custody) (bool, error) {
	lease, err := r.alloc.Lease(ctx, c.leaseID)
	if err != nil {
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			// Genuinely terminal: its capacity is already back.
			return false, nil
		}

		// AMBIGUOUS IS NOT GONE. A read that failed says nothing about the lease,
		// and discarding on it would destroy a live job over a database blip.
		return false, fmt.Errorf("node: read the fenced lease %s: %w", c.leaseID, err)
	}

	if lease.Phase != alloc.PhaseQuarantine {
		// Somebody else holds it now — a replacement incarnation that re-adopted
		// this compute. Not ours to renew, and not ours to destroy either.
		return false, nil
	}

	r.log.Warn("the lease of compute held here was quarantined; adopting it at its new epoch "+
		"and keeping the job running",
		"name", c.name, "lease", c.leaseID, "epoch", lease.Epoch)

	c.epoch.Store(lease.Epoch)
	// Quarantine is already the durable, more conservative visibility state. Do
	// not transition it back to custody or teardown merely because its node came
	// back and resumed tending the same proof obligation.
	c.phase = ""

	return true, nil
}

// finish releases a custody entry's lease and forgets it.
func (r *Runner) finish(ctx context.Context, c *custody) error {
	err := r.alloc.Release(ctx, c.leaseID, c.epoch.Load(), c.outcome)
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) && !errors.Is(err, alloc.ErrFenced) {
		// KEPT in custody. Failing to release means the capacity is still recorded
		// as held, and dropping the entry now would leave nothing to retry it —
		// the lease would sit until the reaper noticed the missing heartbeat.
		return fmt.Errorf("node: release lease %s after its compute was cleaned up: %w", c.leaseID, err)
	}

	r.mu.Lock()
	// DELETED ONLY IF IT IS STILL THE SAME ENTRY. A launch can replace a custody
	// entry for the same lease while this one is finishing, and deleting by id
	// alone would drop the new entry — losing track of compute that exists.
	if r.custody[c.leaseID] == c {
		delete(r.custody, c.leaseID)
	}
	r.mu.Unlock()

	return nil
}

// heldForRequest reports whether custody already covers a job.
//
// The guard against launching a SECOND runner for a request billet is already
// running. After a crash, an unacknowledged assignment is redelivered; the new
// listener has empty maps and would happily escrow another lease and start
// another container, while the adopted one carries on. Two runners for one job
// is worse than it sounds — the extra one is a live registration that can pick
// up unrelated work, and the completion for the original may then kill it.
func (r *Runner) heldForRequest(requestID int64) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, c := range r.custody {
		if c.requestID == requestID {
			return c.leaseID, true
		}
	}

	return "", false
}

// unconfirmedForRequest reports a lease held for a request whose COMPUTE has not
// been proved gone.
//
// NARROWER THAN heldForRequest ON PURPOSE, and the narrowness is the point. A
// Destroy has to answer ErrCustody while this request's compute might still be
// running, and must NOT answer it merely because some entry exists — an entry
// whose container was destroyed and whose release failed belongs to the caller's
// ordinary path, and silencing that one strands a lease nothing else will
// release. See custody.unconfirmed.
func (r *Runner) unconfirmedForRequest(requestID int64) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, c := range r.custody {
		if c.requestID == requestID && c.unconfirmed.Load() {
			return c.leaseID, true
		}
	}

	return "", false
}

// releaseRequest ends the custody of a job GitHub has reported finished, if this
// runner is holding any for it.
//
// Reports only whether the transitions succeeded, not whether there was anything
// to do: "custody handled this" had exactly one caller, and it stopped needing
// the answer when a custody failure stopped being the listener's problem.
//
// The other half of the fix for adopted compute that hangs. The restarted
// listener has no record of the request, so its own completion handling returns
// without releasing anything; this is what connects the message to the container
// a previous incarnation started.
func (r *Runner) releaseRequest(ctx context.Context, requestID int64) error {
	// HELD ACROSS THE WHOLE TRANSITION, not just the flag write. This runs on the
	// listener's goroutine while the periodic tick runs on the reaper's, so
	// releasing the lock before tendOne would let both act on the same entry —
	// duplicate destroys, and a delete racing a replacement.
	//
	// tendOne does not take this lock itself, so holding it here is not
	// re-entrant. The lock order is tending before mu, everywhere.
	r.tending.Lock()
	defer r.tending.Unlock()

	r.mu.Lock()

	var held []*custody

	// EVERY entry for the request, not the first one found. One request should
	// only ever have one, but "should" is doing a lot of work in a function whose
	// job is cleaning up after states that should not exist — and stopping at the
	// first match would destroy one container, acknowledge the completion, and
	// leave the rest.
	for _, c := range r.custody {
		if c.requestID == requestID {
			held = append(held, c)
		}
	}

	r.mu.Unlock()

	if len(held) == 0 {
		return nil
	}

	var failures []error

	for _, c := range held {
		c.discard = true
		if c.phase != "" {
			c.phase = alloc.PhaseTeardown
		}

		r.log.Info("a job billet adopted has been reported finished; releasing its compute",
			"lease", c.leaseID, "request", requestID)

		if err := r.tendOne(ctx, c); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

func (r *Runner) custodySnapshot() []*custody {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*custody, 0, len(r.custody))
	for _, c := range r.custody {
		out = append(out, c)
	}

	return out
}

// Superseded moves everything this node is running into custody.
//
// AFTER SUPERSESSION, EVERYTHING HERE IS UNACCOUNTED FOR — which is the
// definition of custody. The control plane routes a job's completion to whichever
// process currently owns the name, so the destroy for a container running HERE
// now goes to the replacement, which cannot see it, reports success, and lets the
// lease be released.
//
// Nothing else would ever finish this work. Tend is what confirms compute gone
// and releases its lease, and Tend only looks at custody — so a running entry
// left where it was made Holding() true forever and the drain unable to end.
//
// The lease's outcome is recorded as done rather than failed: the job was
// launched cleanly and is running: what changed is who is allowed to talk about
// it, not whether it worked.
func (r *Runner) Superseded() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for requestID, inst := range r.running {
		lease, ok := r.runningLease[requestID]
		if !ok {
			continue
		}

		// MOVED, NOT COPIED, and copying was a way to make the drain eternal. An
		// entry left in `running` keeps Holding() true after Tend has confirmed the
		// compute gone and removed its custody entry — so the process has nothing
		// left to do and can never stop.
		delete(r.running, requestID)
		delete(r.runningLease, requestID)

		if _, held := r.custody[lease.ID]; held {
			continue
		}

		entry := &custody{
			leaseID: lease.ID,

			name:      inst.Name,
			instance:  inst.ID,
			requestID: requestID,
			outcome:   alloc.PhaseDone,
			phase:     alloc.PhaseCustody,
			since:     r.now(),
			// ALREADY SEEN, because this came from the running set: its launch was
			// observed, so a later absence means it genuinely went away rather than
			// that a create might still be in flight. Leaving it false made a job
			// that exits right after supersession hold its host's capacity for the
			// whole stray grace — five minutes of a machine nobody can use, waiting
			// for a container that has already finished.
			observed: true,
		}
		entry.epoch.Store(lease.Epoch)

		// UNCONFIRMED, FOR THE SAME REASON AS AN ADOPTION: this entry was built
		// from an instance in the RUNNING set, so its guest is executing a job
		// right now. Left at the zero value it would read as "the compute is gone
		// and only the release is outstanding", and a Destroy for this request
		// would report success on it — releasing the capacity of a superseded
		// process's live job, which is the one thing supersession exists to hold.
		entry.unconfirmed.Store(true)

		r.custody[lease.ID] = entry
	}
}

// Holding reports whether this node is still responsible for compute.
//
// ASKED WHEN THE NODE HAS BEEN SUPERSEDED and is deciding whether it may stop.
// Exiting while custody remains is how a container ends up with a lease nobody
// renews: the replacement cannot see it — it is on a different machine — so
// nothing else will ever hold it.
func (r *Runner) Holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// RUNNING COUNTS, and leaving it out was the whole bug. Custody is compute
	// billet cannot account for and launching is compute it is creating — but an
	// ordinary job that launched cleanly and is running right now is neither, and
	// it is just as much this process's responsibility.
	//
	// Without it a node that had successfully started a job saw "holding nothing"
	// the moment it was superseded, exited, and left a container whose completion
	// is now routed to a replacement that cannot see it. That Destroy finds
	// nothing, reports success, and the lease is released under a running job.
	return len(r.custody) > 0 || len(r.launching) > 0 || len(r.running) > 0
}

// renewSnapshot is everything whose lease this node must keep alive.
//
// WIDER THAN CUSTODY, AND ONLY FOR RENEWAL. A launch in progress needs its lease
// renewed for exactly the same reason a custody entry does — for its duration
// nobody else is doing it — but it is not custody and must not be TENDED.
//
// Putting the launching entries into custodySnapshot instead was a real bug and
// an instructive one: Tend iterates that same snapshot to decide which compute
// is finished and release its lease, so a launch still in progress was handed to
// teardown. It carries no outcome, so the release failed with `invalid phase
// transition: "" is not terminal`, and the container it had just started was
// left with a lease the node had already stopped tracking.
//
// The gap it closes is real, so the fix is a second view rather than a revert:
//
// Across the wire, the control plane stops waiting after the command timeout and
// hands the listener custody, which stops the listener heartbeating. The node is
// meanwhile still inside provider.Launch, pulling a large image, and adopts
// nothing until that call returns. Between those two moments the lease has no
// owner at all: the reaper releases its capacity, the allocator sells it to
// another job, and then the launch completes onto hardware already spoken for.
func (r *Runner) renewSnapshot() []*custody {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*custody, 0, len(r.custody)+len(r.launching))
	for _, c := range r.custody {
		out = append(out, c)
	}

	for _, c := range r.launching {
		out = append(out, c)
	}

	return out
}

// holdWhileLaunching keeps a lease renewed for as long as the launch runs.
//
// Returns the function that stops it. Renewing the same lease twice is harmless
// — Heartbeat is idempotent, and a lease already gone answers ErrLeaseNotFound,
// which renewHeld ignores — so the overlap with the listener's own heartbeat
// costs nothing and the gap it closes is unbounded.
func (r *Runner) holdWhileLaunching(lease *alloc.Lease, name string) func() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.launching == nil {
		r.launching = make(map[string]*custody)
	}

	entry := &custody{leaseID: lease.ID, name: name}
	entry.epoch.Store(lease.Epoch)

	r.launching[lease.ID] = entry

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		delete(r.launching, lease.ID)
	}
}

// errCustody is returned by Launch when the runner has taken responsibility for
// a lease's capacity, so the caller must not release it.
//
// It wraps server.ErrCustody rather than being it, so the listener can recognise
// the situation without importing this package while the message still says
// which instance is involved.
func errCustody(name string, cause error) error {
	return fmt.Errorf("%w: compute named %s may still exist; billet is holding its "+
		"capacity until it is confirmed gone (%v)", server.ErrCustody, name, cause)
}
