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

	// discard is true when the instance must go as soon as it can be found, and
	// false when it is running work that should be allowed to finish.
	discard bool

	// since is when custody was taken, for the diagnostic that matters most: how
	// long capacity has been held for something nobody is watching.
	since time.Time
}

// Adopt takes custody of an instance that survived a restart.
func (r *Runner) adopt(leaseID string, inst *provider.Instance, epoch int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.custody[leaseID] = &custody{
		leaseID:  leaseID,
		epoch:    epoch,
		name:     inst.Name,
		instance: inst.ID,
		since:    r.now(),
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
		leaseID: lease.ID,
		epoch:   lease.Epoch,
		name:    name,
		discard: true,
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
	var failures []error

	for _, c := range r.custodySnapshot() {
		if err := r.tendOne(ctx, c); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

func (r *Runner) tendOne(ctx context.Context, c *custody) error {
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
		// Nothing there. For a discarded entry that is the answer it was waiting
		// for; for an adopted one it means the container went away without billet
		// seeing it stop. Either way the capacity can go back.
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
	err := r.alloc.Release(ctx, c.leaseID, c.epoch, alloc.PhaseDone)
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) && !errors.Is(err, alloc.ErrFenced) {
		// KEPT in custody. Failing to release means the capacity is still recorded
		// as held, and dropping the entry now would leave nothing to retry it —
		// the lease would sit until the reaper noticed the missing heartbeat.
		return fmt.Errorf("node: release lease %s after its compute was cleaned up: %w", c.leaseID, err)
	}

	r.mu.Lock()
	delete(r.custody, c.leaseID)
	r.mu.Unlock()

	return nil
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
