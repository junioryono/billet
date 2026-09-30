package server

import (
	"context"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// RunnerState is what GitHub says about one registration right now.
type RunnerState struct {
	// Present is false only when GitHub positively answered that no such
	// registration exists; a lookup that failed is an error, never absence.
	Present bool
	// Online is whether the runner holds the connection a job is delivered over.
	Online bool
	// Busy is whether GitHub says the runner is executing a job.
	Busy bool
}

// RunnerInspector reads one pool registration's state at GitHub. A registry
// that implements it lets the listener retire a member GitHub can no longer
// route work to.
type RunnerInspector interface {
	InspectRunner(ctx context.Context, runnerName string, runnerID int64) (RunnerState, error)
}

const (
	// offlineIdleAfter is how long a member must have been idle here before
	// GitHub is asked about it. A runner connects within a minute of its guest
	// booting, so a member offline past this is not one still starting.
	offlineIdleAfter = 15 * time.Minute
	// offlineGrace separates the two observations a retirement needs. One
	// offline answer can be a runner reconnecting; two spanning this cannot.
	offlineGrace = 5 * time.Minute
	// offlineInspectEvery bounds the lookups one tier makes: one member per
	// interval, oldest-inspected first.
	offlineInspectEvery = time.Minute
)

// offlineWatch is one listener's memory of its idle members. It lives only in
// the process: a restart forgets it and every clock starts again, which delays
// a retirement and never hastens one.
type offlineWatch struct {
	idleSince    map[string]time.Time
	offlineSince map[string]time.Time
	inspectedAt  map[string]time.Time
	lastInspect  time.Time
}

// retireOfflineMembers retires at most one idle pool member whose runner GitHub
// has twice, offlineGrace apart, reported present, offline and not busy.
//
// This is the per-registration evidence reconcilePool's surplus cannot supply.
// The surplus is judged against GitHub's assigned-job count, which only a
// message carrying statistics refreshes, so a tier that goes quiet freezes it
// and a member whose runner never takes a job is never surplus. On 2026-09-30
// such a member held its lease, and a draining node's restart, for ten hours
// (#288). The offline answers only choose the candidate; what licenses the
// teardown is GitHub accepting the registration's removal, which it refuses
// while a job runs.
//
// An online idle member is left alone even here: GitHub may hand it a job at any
// moment, and its only cost is capacity the aggregate will reclaim once it moves.
func (l *Listener) retireOfflineMembers(ctx context.Context, runners []alloc.PoolRunner) {
	inspector, ok := l.registry.(RunnerInspector)
	if !ok || l.alloc == nil {
		return
	}

	w := &l.offline
	if w.idleSince == nil {
		w.idleSince = map[string]time.Time{}
		w.offlineSince = map[string]time.Time{}
		w.inspectedAt = map[string]time.Time{}
	}

	now := l.clock()
	idle := make(map[string]bool, len(runners))

	var candidate *alloc.PoolRunner

	for i := range runners {
		member := &runners[i]
		if member.Status != alloc.PoolRunnerIdle || member.ActualRequestID != 0 ||
			member.RunnerID <= 0 || member.RunnerName == "" {
			continue
		}

		idle[member.LeaseID] = true

		since, seen := w.idleSince[member.LeaseID]
		if !seen {
			w.idleSince[member.LeaseID] = now
			continue
		}

		if now.Sub(since) < offlineIdleAfter {
			continue
		}

		if candidate == nil || w.inspectedAt[member.LeaseID].Before(w.inspectedAt[candidate.LeaseID]) {
			candidate = member
		}
	}

	for id := range w.idleSince {
		if !idle[id] {
			delete(w.idleSince, id)
			delete(w.offlineSince, id)
			delete(w.inspectedAt, id)
		}
	}

	if candidate == nil || now.Sub(w.lastInspect) < offlineInspectEvery {
		return
	}

	w.lastInspect = now
	w.inspectedAt[candidate.LeaseID] = now

	state, err := inspector.InspectRunner(ctx, candidate.RunnerName, candidate.RunnerID)
	if err != nil {
		delete(w.offlineSince, candidate.LeaseID)
		l.log.Debug("could not read an idle pool member's runner at GitHub; keeping it",
			"tier", l.tier, "runner", candidate.RunnerName, "error", err)

		return
	}

	if !state.Present || state.Online || state.Busy {
		delete(w.offlineSince, candidate.LeaseID)
		return
	}

	// DATED WHEN THE ANSWER ARRIVED, not when the question was asked: the lookup
	// can wait on the client's mutex and two network calls, and a first answer
	// dated at its start would let the second follow it by seconds.
	answered := l.clock()

	first, seen := w.offlineSince[candidate.LeaseID]
	if !seen {
		w.offlineSince[candidate.LeaseID] = answered
		return
	}

	if now.Sub(first) < offlineGrace {
		return
	}

	// THE REMOVAL IS THE PROOF, AND IT COMES FIRST. No REST answer, however many,
	// stops GitHub delivering a job the instant after it was read. The Actions
	// service refuses to delete a runner with a job still running
	// (JobStillRunningException, the refusal ARC's runner controller relies on),
	// so a removal that succeeds is the fence: nothing can be routed to this
	// runner afterwards and nothing was running on it. A refusal or any error
	// leaves the member idle and unjournaled, so a JobStarted for it still binds.
	if err := l.registry.RemoveRunner(ctx, candidate.RunnerID, candidate.RunnerName); err != nil {
		delete(w.offlineSince, candidate.LeaseID)
		l.log.Warn("GitHub reported an idle pool member's runner offline but would not remove it; "+
			"keeping it", "tier", l.tier, "runner", candidate.RunnerName, "error", err)

		return
	}

	if err := l.alloc.RetirePoolRunner(ctx, candidate.LeaseID); err != nil {
		l.log.Error("could not claim an offline pool member for retirement", "tier", l.tier,
			"runner", candidate.RunnerName, "error", err)

		return
	}

	l.log.Warn("retiring a pool member whose runner GitHub reported offline and not busy and "+
		"then removed; it was never given a job", "tier", l.tier, "runner", candidate.RunnerName,
		"idle_since", w.idleSince[candidate.LeaseID], "offline_since", first)

	delete(w.idleSince, candidate.LeaseID)
	delete(w.offlineSince, candidate.LeaseID)
	delete(w.inspectedAt, candidate.LeaseID)

	candidate.Status = alloc.PoolRunnerRetiring
	l.retirePoolMember(ctx, *candidate)
}

// clock is the listener's time source for the offline watch.
func (l *Listener) clock() time.Time {
	if l.now != nil {
		return l.now()
	}

	return time.Now()
}
