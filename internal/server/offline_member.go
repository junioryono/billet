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
	// ID is the id GitHub holds for the name, set whenever Present is.
	ID int64
}

// RunnerInspector reads one pool registration's state at GitHub and withdraws
// it by id. A registry that implements it lets the listener retire a member
// GitHub can no longer route work to.
type RunnerInspector interface {
	// InspectRunner looks the registration up by its exact name. A non-zero
	// runnerID is what the pool recorded, and a registration holding another id
	// is an error.
	InspectRunner(ctx context.Context, runnerName string, runnerID int64) (RunnerState, error)
	// WithdrawRunner returns nil only when GitHub acknowledged deleting exactly
	// this id; an absent runner is an error, never a success.
	WithdrawRunner(ctx context.Context, runnerID int64) error
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
	// offlineID is the runner id the first offline answer named; the second must
	// name the same one.
	offlineID   map[string]int64
	inspectedAt map[string]time.Time
	lastInspect time.Time
	// withdrawn holds members GitHub has already deleted whose retirement the
	// ledger has not yet recorded. They are journaled on the next reconcile
	// without asking GitHub again, whose answer now is only "absent".
	withdrawn map[string]alloc.PoolRunner
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
//
// THE ID IS GITHUB'S, READ BY THE MEMBER'S EXACT NAME. A remote node's JIT mint
// journals the registration's id, but the listener's own launch path journals a
// member with a name and no id, and the pool then learns one only from a
// JobStarted, which the members this exists for never receive. The name is
// billet-<lease id>, unique to the lease; both offline answers must name the same
// id, which must equal one the pool journaled, and that id is the one withdrawn
// and the one the cleanup's removal expects. (On 2026-10-02 four members adopted
// into a node's custody across a controller restart held its drain for nine
// hours, because the registry this asserts was an adapter that did not forward
// the inspector at all, #317.)
func (l *Listener) retireOfflineMembers(ctx context.Context, runners []alloc.PoolRunner) {
	inspector, ok := l.registry.(RunnerInspector)
	if !ok || l.alloc == nil {
		return
	}

	w := &l.offline
	if w.idleSince == nil {
		w.idleSince = map[string]time.Time{}
		w.offlineSince = map[string]time.Time{}
		w.offlineID = map[string]int64{}
		w.inspectedAt = map[string]time.Time{}
		w.withdrawn = map[string]alloc.PoolRunner{}
	}

	now := l.clock()
	idle := make(map[string]bool, len(runners))

	var candidate *alloc.PoolRunner

	for i := range runners {
		member := &runners[i]
		if member.Status != alloc.PoolRunnerIdle || member.ActualRequestID != 0 || member.RunnerName == "" {
			continue
		}

		idle[member.LeaseID] = true

		// THE CACHED MEMBER, NOT THE ROW: only it carries the id GitHub deleted,
		// which the cleanup's removal must expect.
		if withdrawn, pending := w.withdrawn[member.LeaseID]; pending {
			l.retireWithdrawn(ctx, withdrawn)
			continue
		}

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
			delete(w.offlineID, id)
			delete(w.inspectedAt, id)
		}
	}

	for id := range w.withdrawn {
		if !idle[id] {
			delete(w.withdrawn, id)
		}
	}

	if candidate == nil || now.Sub(w.lastInspect) < offlineInspectEvery {
		return
	}

	w.lastInspect = now
	w.inspectedAt[candidate.LeaseID] = now

	restart := func() {
		delete(w.offlineSince, candidate.LeaseID)
		delete(w.offlineID, candidate.LeaseID)
	}

	state, err := inspector.InspectRunner(ctx, candidate.RunnerName, candidate.RunnerID)
	if err != nil {
		restart()
		l.log.Debug("could not read an idle pool member's runner at GitHub; keeping it",
			"tier", l.tier, "runner", candidate.RunnerName, "error", err)

		return
	}

	if !state.Present || state.Online || state.Busy {
		restart()
		return
	}

	// AN OFFLINE ANSWER THAT NAMES NO ID, OR ANOTHER ONE, IS NOT EVIDENCE about
	// the registration this member launched.
	if state.ID <= 0 || (candidate.RunnerID > 0 && state.ID != candidate.RunnerID) {
		restart()
		l.log.Warn("GitHub's answer for an idle pool member's runner named no id or another one; "+
			"keeping it", "tier", l.tier, "runner", candidate.RunnerName,
			"pool_id", candidate.RunnerID, "github_id", state.ID)

		return
	}

	// DATED WHEN THE ANSWER ARRIVED, not when the question was asked: the lookup
	// can wait on the client's mutex and two network calls, and a first answer
	// dated at its start would let the second follow it by seconds.
	answered := l.clock()

	first, seen := w.offlineSince[candidate.LeaseID]
	if !seen || w.offlineID[candidate.LeaseID] != state.ID {
		w.offlineSince[candidate.LeaseID] = answered
		w.offlineID[candidate.LeaseID] = state.ID

		return
	}

	if now.Sub(first) < offlineGrace {
		return
	}

	// THE DELETE IS THE PROOF, AND IT COMES FIRST. No REST answer, however many,
	// stops GitHub delivering a job the instant after it was read. The Actions
	// service refuses to delete a runner with a job still running
	// (JobStillRunningException, the refusal ARC's runner controller relies on),
	// so an acknowledged DELETE of this exact id is the fence: nothing can be
	// routed to the runner afterwards and nothing was running on it. A refusal,
	// an absent runner or any error leaves the member idle and unjournaled, so a
	// JobStarted for it still binds.
	//
	// A DELETE GITHUB CARRIED OUT WHOSE ANSWER WAS LOST is kept the same way, and
	// stays kept: later inspections find the runner absent, and absence is not the
	// acknowledgement this path requires. That member is left for an operator, the
	// safe direction, as it was before this path existed.
	if err := inspector.WithdrawRunner(ctx, state.ID); err != nil {
		restart()
		l.log.Warn("GitHub reported an idle pool member's runner offline but its deletion was "+
			"refused or not confirmed; keeping it", "tier", l.tier, "runner", candidate.RunnerName,
			"runner_id", state.ID, "error", err)

		return
	}

	l.log.Warn("GitHub deleted a pool member's runner after reporting it offline and not busy; "+
		"it was never given a job, retiring it", "tier", l.tier, "runner", candidate.RunnerName,
		"runner_id", state.ID, "idle_since", w.idleSince[candidate.LeaseID], "offline_since", first)

	delete(w.idleSince, candidate.LeaseID)
	delete(w.offlineSince, candidate.LeaseID)
	delete(w.offlineID, candidate.LeaseID)
	delete(w.inspectedAt, candidate.LeaseID)

	candidate.RunnerID = state.ID
	w.withdrawn[candidate.LeaseID] = *candidate

	l.retireWithdrawn(ctx, *candidate)
}

// retireWithdrawn journals and tears down a member whose registration GitHub
// has deleted. A failed claim keeps it in the watch, so the next reconcile
// retries the ledger without inspecting again; a controller restart in that
// window forgets it and leaves the member idle with no registration.
func (l *Listener) retireWithdrawn(ctx context.Context, member alloc.PoolRunner) {
	claim := l.alloc.RetirePoolRunner
	if l.claimRetirement != nil {
		claim = l.claimRetirement
	}

	if err := claim(ctx, member.LeaseID); err != nil {
		l.log.Error("could not claim a withdrawn pool member for retirement; retrying on the next "+
			"reconcile", "tier", l.tier, "runner", member.RunnerName, "error", err)

		return
	}

	delete(l.offline.withdrawn, member.LeaseID)

	member.Status = alloc.PoolRunnerRetiring
	l.retirePoolMember(ctx, member)
}

// clock is the listener's time source for the offline watch.
func (l *Listener) clock() time.Time {
	if l.now != nil {
		return l.now()
	}

	return time.Now()
}
