package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// custody is a lease whose capacity must keep being held because compute for it
// may exist, and which nothing else in this process is managing.
//
// Two situations produce one, and they are the same situation seen from
// different sides:
//
//   - ADOPTED. A container survived a restart. The runner inside it is talking
//     to GitHub directly and may well finish the job successfully, so destroying
//     it is a deliberate job failure rather than a recovery. Billet cannot manage
//     it — no request-id mapping, no completion handling — but it can keep the
//     lease alive so the capacity is not resold, and clean up when the container
//     stops.
//
//   - DISCARDED. A launch failed ambiguously and the cleanup could not be
//     confirmed. The container may or may not exist; until billet knows it does
//     not, its capacity must stay held. This is the case where releasing the
//     lease on a launch error was quietly over-committing the host.
//
// Both need the same two things: a heartbeat so the reaper does not terminalize
// the lease, and a repeated attempt to find out what happened. The only
// difference is what a still-running instance means — leave it, or kill it.
type custody struct {
	leaseID  string
	epoch    int64
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

	// discard is true when the instance must go as soon as it can be found, and
	// false when it is running work that should be allowed to finish.
	//
	// It changes over an entry's life — a completion or a lost lease turns an
	// adoption into a discard — so it says what to do NEXT, not where the entry
	// came from.
	discard bool

	// observed is true once billet has actually seen this instance exist.
	//
	// SEPARATE FROM discard, and conflating the two was a bug. The grace period
	// before an absence is believed exists for compute that may never have
	// started: a create whose response was lost can still be in flight inside the
	// daemon. It has nothing to say about an instance billet WATCHED running and
	// then found gone — that one has genuinely finished, and making it wait out
	// the grace held its capacity for a minute after its job was over.
	//
	// Because discard flips to true when a completion arrives, checking it alone
	// applied the stray grace to every adopted job at exactly the moment it
	// finished.
	observed bool

	// since is when custody was taken, for the diagnostic that matters most: how
	// long capacity has been held for something nobody is watching.
	since time.Time
}

// adopt takes custody of an instance that survived a restart.
func (r *Runner) adopt(lease *alloc.Lease, inst *provider.Instance) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.custody[lease.ID] = &custody{
		leaseID:   lease.ID,
		epoch:     lease.Epoch,
		name:      inst.Name,
		instance:  inst.ID,
		requestID: lease.RequestID,
		outcome:   alloc.PhaseDone,
		// Adoption starts from a container billet just watched running.
		observed: true,
		since:    r.now(),
	}
}

// hold takes custody of a lease whose compute could not be confirmed gone.
//
// The instance id is not required and usually not known: the whole reason this
// exists is that Find could not answer. The name is enough, because the name is
// what identifies the instance to the backend.
//
// THE REQUEST ID IS PASSED IN, NOT READ OFF THE LEASE, and that is not a style
// choice. Assign writes the request id to SQLite without touching the caller's
// in-memory Lease, so the pointer a listener holds still carries RequestID 0 —
// every discarded entry was being filed under request 0. A later assignment for
// the real request then walked straight past heldForRequest and started a second
// runner, and a completion for it could never find the entry at all.
//
// The unit tests could not see this because their helper writes the id back onto
// the struct by hand, which no production path does. The same stale-pointer trap
// bit a test in this package an hour earlier; I fixed it there and did not think
// to look for it here.
func (r *Runner) hold(lease *alloc.Lease, name string, requestID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.custody[lease.ID] = &custody{
		leaseID:   lease.ID,
		epoch:     lease.Epoch,
		name:      name,
		requestID: requestID,
		discard:   true,
		// FAILED, not done. This lease's job never started.
		outcome: alloc.PhaseFailed,
		since:   r.now(),
	}
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
// The window in which a daemon that accepted a create can still act on it. Sixty
// seconds is far longer than any container runtime needs and cheap to be wrong
// about — the cost of waiting is one lease's capacity for a minute, and the cost
// of not waiting is a container running on capacity billet has already resold.
const strayGrace = time.Minute

// custodyWarnAfter is how long a held lease may go unresolved before every tick
// says so.
//
// An hour is longer than most CI jobs and far shorter than the point at which
// the capacity loss stops mattering. The warning is the actual mechanism here:
// the bound below is a backstop that should never fire, and an operator finding
// out about held capacity only when it is destroyed a day later has been told
// too late to do anything about it.
const custodyWarnAfter = time.Hour

// DefaultMaxCustody is OFF, and that is a deliberate reversal.
//
// I gave this a 24-hour default and it was wrong. Elapsed time is not evidence
// that a job has stopped making progress: billet imposes no job limit of its
// own, self-hosted runners are routinely configured past GitHub's six-hour
// default, and a legitimately long job restarted shortly after it began would be
// killed a day later for no reason anyone could see in the logs.
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
// SEPARATE FROM Tend, AND THAT SEPARATION IS THE WHOLE POINT. Renewal used to
// happen inside Tend, which runs after Reap on a shared tick and which makes
// unbounded provider calls — a slow `docker ps` or a wedged daemon delays the
// next renewal without delaying the next reap. The interval from a successful
// heartbeat to the following Reap was therefore unbounded, and anything longer
// than the lease TTL means the reaper terminalizes a lease that is being held on
// purpose, hands its capacity back, and lets a listener advertise it while the
// container is still running.
//
// So this does exactly one thing, touches only the ledger, and ticks at a third
// of the TTL — the same cadence and the same reasoning as the listener's own
// heartbeats. Two renewals may be missed entirely before anything expires.
func (r *Runner) KeepAlive(ctx context.Context) {
	// THE CADENCE IS RE-READ EVERY CYCLE, not fixed at the first one.
	//
	// The TTL is negotiated at registration, and a node re-registers whenever the
	// control plane forgets it or restarts. A plane that comes back advertising a
	// SHORTER TTL leaves a janitor built on the old one renewing too slowly: the
	// lease expires between two heartbeats, the reaper resells its capacity, and
	// the container it was holding is still running. The janitor is started once
	// on purpose — custody outlives any registration — so re-reading here is the
	// only place that correction can happen.
	timer := time.NewTimer(r.renewEvery())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.renewHeld(ctx)
			timer.Reset(r.renewEvery())
		}
	}
}

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
	for _, c := range r.custodySnapshot() {
		err := r.alloc.Heartbeat(ctx, c.leaseID, c.epoch)
		if err == nil || ctx.Err() != nil {
			continue
		}

		if errors.Is(err, alloc.ErrLeaseNotFound) || errors.Is(err, alloc.ErrFenced) {
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
	} else if held > custodyWarnAfter {
		r.log.Warn("still holding capacity for compute billet is not managing",
			"name", c.name, "lease", c.leaseID, "held_for", held.Round(time.Minute),
			"adopted", !c.discard)
	}

	if err := r.alloc.Heartbeat(ctx, c.leaseID, c.epoch); err != nil {
		if !errors.Is(err, alloc.ErrLeaseNotFound) && !errors.Is(err, alloc.ErrFenced) {
			return fmt.Errorf("node: hold the capacity of lease %s: %w", c.leaseID, err)
		}

		// The lease is gone. Its capacity is already back in the budget, so the
		// instance is now an orphan whatever it was before.
		c.discard = true
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

	if err := r.provider.Destroy(ctx, inst.ID); err != nil {
		return fmt.Errorf("node: destroy %s held for lease %s: %w", c.name, c.leaseID, err)
	}

	r.log.Info("released compute that was being held",
		"name", c.name, "lease", c.leaseID, "adopted", !c.discard,
		"held_for", r.now().Sub(c.since).Round(time.Second))

	return r.finish(ctx, c)
}

// finish releases a custody entry's lease and forgets it.
func (r *Runner) finish(ctx context.Context, c *custody) error {
	err := r.alloc.Release(ctx, c.leaseID, c.epoch, c.outcome)
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

	out := make([]*custody, 0, len(r.custody)+len(r.launching))
	for _, c := range r.custody {
		out = append(out, c)
	}

	// A LAUNCH IN PROGRESS NEEDS RENEWING TOO, and nothing was doing it.
	//
	// Across the wire, the control plane stops waiting after the command timeout
	// and hands the listener custody — which stops heartbeating on the strength of
	// it. The node meanwhile is still INSIDE provider.Launch, pulling a large
	// image, and does not adopt anything until that call returns. Between those
	// two moments the lease has no owner at all: the reaper releases its capacity,
	// the allocator sells it to another job, and then the launch completes and
	// starts a second workload on the same hardware.
	//
	// So the node renews from the instant it commits to launching, which is the
	// only moment either side can be sure something is about to exist.
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

	r.launching[lease.ID] = &custody{
		leaseID: lease.ID,
		epoch:   lease.Epoch,
		name:    name,
	}

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
