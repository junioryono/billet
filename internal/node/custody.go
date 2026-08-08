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
	discard bool

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
		since:     r.now(),
	}
}

// hold takes custody of a lease whose compute could not be confirmed gone.
//
// The instance id is not required and usually not known: the whole reason this
// exists is that Find could not answer. The name is enough, because the name is
// what identifies the instance to the backend.
func (r *Runner) hold(lease *alloc.Lease, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.custody[lease.ID] = &custody{
		leaseID:   lease.ID,
		epoch:     lease.Epoch,
		name:      name,
		requestID: lease.RequestID,
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

// MaxCustody is how long compute may be held before billet destroys it anyway.
//
// A HARD BOUND ON AN OTHERWISE UNBOUNDED HOLD. Adoption keeps a lease alive for
// as long as its container runs, which is right for a job that is making
// progress and wrong for one that has hung — a runner wedged on a network call
// holds its vCPU forever, and the only evidence is a log line from the restart
// that adopted it.
//
// Twenty-four hours, because the thing being bounded is a JOB and the longest
// legitimate one is far shorter. GitHub's own default job timeout is six hours;
// self-hosted runners can be configured beyond that, which is why this is not
// six. Nothing that has been running for a day is still doing useful CI work,
// and holding capacity for it is a worse failure than killing it.
const MaxCustody = 24 * time.Hour

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

	// PAST THE BOUND, IT GOES, whatever it was. An adopted container this old is
	// not finishing, and continuing to hold its capacity means the host stays
	// short by that much until somebody restarts billet.
	if held > MaxCustody && !c.discard {
		r.log.Error("a job has been held far too long to still be running; destroying it "+
			"and reclaiming its capacity",
			"name", c.name, "lease", c.leaseID, "held_for", held.Round(time.Minute),
			"limit", MaxCustody)

		c.discard = true
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
		// An adopted entry is different: its container was observed running, so an
		// absence now means it genuinely went away.
		if c.discard && r.now().Sub(c.since) < strayGrace {
			return nil
		}

		return r.finish(ctx, c)
	}

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

// releaseRequest ends the custody of a job GitHub has reported finished, if
// this runner is holding one for it.
//
// The other half of the fix for adopted compute that hangs. The restarted
// listener has no record of the request, so its own completion handling returns
// without releasing anything; this is what connects the message to the container
// a previous incarnation started.
func (r *Runner) releaseRequest(ctx context.Context, requestID int64) (bool, error) {
	r.mu.Lock()

	var held *custody

	for _, c := range r.custody {
		if c.requestID == requestID {
			held = c

			break
		}
	}

	r.mu.Unlock()

	if held == nil {
		return false, nil
	}

	// HELD ACROSS THE WHOLE TRANSITION, not just the flag write. This runs on the
	// listener's goroutine while the periodic tick runs on the reaper's, so
	// releasing the lock before tendOne would let both act on the same entry —
	// duplicate destroys, and a delete racing a replacement. Serializing only the
	// mutation was the same bug the serialization was added to fix, moved one
	// line down.
	//
	// tendOne does not take this lock itself, so holding it here is not
	// re-entrant. The lock order is tending before mu, everywhere.
	r.tending.Lock()
	defer r.tending.Unlock()

	held.discard = true

	r.log.Info("a job billet adopted has been reported finished; releasing its compute",
		"lease", held.leaseID, "request", requestID)

	return true, r.tendOne(ctx, held)
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
