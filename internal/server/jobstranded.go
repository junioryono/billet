package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// RunQueue reads whether a run is over and what its latest attempt's jobs of one name
// are doing. A target's Provisioner implements it over the App's REST client, with
// `actions: read`.
type RunQueue interface {
	JobsNamed(ctx context.Context, owner, repository string, runID int64, name string) (NamedJobs, error)
}

// NamedJobs is a run as GitHub records it now: whether it is over and, if it is not,
// how many of its jobs of one name no runner has taken and how many one has.
type NamedJobs struct {
	Completed bool
	Unstarted int
	Taken     int
}

const (
	// strandedAfter is how long after a launch that started nothing its job is first
	// asked about: two of GitHub's five-minute cancellations of an unrun assignment and
	// a margin, after which GitHub has either offered the job again or stopped
	// (docs/reference/records/assignment-protocol.md).
	strandedAfter = 15 * time.Minute
	// strandedWindow is how long a launch waits to be decided before it is decided as
	// expired: a strand nobody could ask about in a day is nobody's to report now.
	strandedWindow = 24 * time.Hour
	// strandedRows bounds one pass's read of the ledger, and strandedReadsPerInstallation
	// its reads of GitHub through one App installation.
	strandedRows                 = 200
	strandedReadsPerInstallation = 2
	// strandedAgain is how long a launch GitHub's record left undecided gives way to
	// the others before it is asked about again, so two that never decide cannot
	// take every pass's reads from a third.
	strandedAgain = 10 * time.Minute
	// strandedObservedWithin is how recent a tier's own report of its capacity must be
	// for what it says to count: a listener that stopped reporting says nothing.
	strandedObservedWithin = 10 * time.Minute
)

// findStranded decides, for each launch that started nothing a while ago, whether
// GitHub kept offering its job (#911).
//
// ONLY ON GITHUB'S WORD AND BILLET'S OWN RECORD, NEVER A TIMER ALONE. A job is
// stranded when GitHub's REST record of its run still has a job of its name queued
// with no runner, long after the failure, while its tier was unsealed, advertising
// capacity and waiting for room for nothing, and the tier has been assigned nothing
// for the run since that is, or may be, a job of that name. Each of those excludes a
// reason the same job would legitimately wait: an advertisement of 0 or a seal stops
// GitHub offering anything, a tier waiting for room is offered work it cannot buy
// for, and a later assignment may be the job itself. A read that fails is
// could-not-tell, and the launch is asked again on a later pass, until a day has
// passed.
func (s *Server) findStranded(ctx context.Context, targets map[string]Target, sweep *rerunSweep,
	budgets map[string][]RESTHeadroom, now time.Time,
) {
	misses, err := s.alloc.LaunchMisses(ctx, now.Add(-strandedWindow-time.Hour), sweep.strandedFrom, strandedRows)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not list the launches that started nothing; asking again later", "error", err)
		}

		return
	}

	// ONE PAGE A PASS, FROM WHERE THE LAST ONE ENDED, and from the start again once a
	// page reaches the launches too young to ask about or the end of the list, so
	// launches older than the page cannot keep a younger one from being listed at all.
	rounded := len(misses) < strandedRows
	due := make([]*alloc.LaunchMiss, 0, len(misses))
	for i := range misses {
		miss := &misses[i]
		age := now.Sub(miss.Ended)

		if miss.Ended.IsZero() || miss.Owner == "" || miss.Repository == "" || miss.RunID <= 0 ||
			miss.JobName == "" || age > strandedWindow {
			s.decideStranded(ctx, sweep, miss, alloc.StrandedExpired, now)

			continue
		}
		// OLDEST FIRST, so every launch after this one is younger still.
		if age < s.strandedWait() {
			rounded = true

			break
		}
		if asked, ok := sweep.strandedAsked[miss.LeaseID]; ok && now.Sub(asked) < strandedAgain {
			continue
		}
		due = append(due, miss)
	}
	sweep.strandedFrom = alloc.LaunchMissCursor{}
	if !rounded {
		sweep.strandedFrom = misses[len(misses)-1].After()
	}
	if len(due) == 0 {
		return
	}

	// LEAST RECENTLY ASKED FIRST, a launch never asked before any that was, so launches
	// GitHub's record never decides cannot keep another unread by giving way in turn.
	slices.SortStableFunc(due, func(a, b *alloc.LaunchMiss) int {
		return sweep.strandedAsked[a.LeaseID].Compare(sweep.strandedAsked[b.LeaseID])
	})

	admission, err := s.alloc.Admission(ctx)
	if err != nil {
		return
	}
	reports, err := s.alloc.CapacityReports(ctx)
	if err != nil {
		return
	}

	read := map[string]int{}
	for _, miss := range due {
		if ctx.Err() != nil {
			return
		}

		target := targets[miss.Tier]
		queue, reaches := target.Provisioner.(RunQueue)
		if !reaches {
			continue
		}
		installation := installationOf(target)
		if read[installation] >= strandedReadsPerInstallation || now.Before(sweep.paused[installation]) {
			continue
		}
		if _, _, _, low := inReserve(budgets[installation], now); low {
			continue
		}

		if !strandedTierWaitsForNothing(admission.Sealed(), reports[miss.Tier], now) {
			continue
		}

		later, err := s.alloc.LaterLeasesOfJob(ctx, miss.Tier, miss.RunID, miss.JobName, miss.Ended)
		switch {
		case err != nil:
			continue
		case later.Same > 0:
			s.decideStranded(ctx, sweep, miss, alloc.StrandedOffered, now)

			continue
		// AN ASSIGNMENT WHOSE JOB IS NOT NAMED YET may be this one offered again, and is
		// named when its job starts: until then, nothing is decided.
		case later.Unnamed > 0:
			continue
		}

		read[installation]++
		sweep.strandedAsked[miss.LeaseID] = now
		jobs, err := queue.JobsNamed(ctx, miss.Owner, miss.Repository, miss.RunID, miss.JobName)
		if err != nil {
			s.strandedReadFailed(ctx, miss, installation, err, sweep, now)

			continue
		}

		switch {
		case jobs.Completed, jobs.Unstarted == 0 && jobs.Taken > 0:
			s.decideStranded(ctx, sweep, miss, alloc.StrandedOffered, now)
		case jobs.Unstarted > 0 && jobs.Taken == 0:
			if s.decideStranded(ctx, sweep, miss, alloc.StrandedJob, now) {
				s.log.Error("GitHub has stopped offering a job: it is still queued with no runner, "+
					fmt.Sprintf("%s after billet's launch for it started nothing, ", now.Sub(miss.Ended).Round(time.Minute))+
					"while its tier advertised capacity and waited for nothing. GitHub does not offer a "+
					"job again after two cancelled assignments; cancel the run and re-run its failed jobs "+
					"to have it offered again",
					"tier", miss.Tier, "repository", miss.Owner+"/"+miss.Repository, "run", miss.RunID,
					"job", miss.JobName, "lease", miss.LeaseID,
					"url", fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", miss.Owner, miss.Repository, miss.RunID))
			}
		// NO JOB OF THAT NAME, or one taken beside one waiting: GitHub's record says
		// nothing this can decide on, and the launch is asked about again.
		default:
		}
	}
}

// strandedWait is how long after a launch that started nothing its job is first
// asked about: strandedAfter, unless WithStrandedAfter replaced it.
func (s *Server) strandedWait() time.Duration {
	if s.strandedAfterOverride > 0 {
		return s.strandedAfterOverride
	}

	return strandedAfter
}

// strandedTierWaitsForNothing says whether a tier was, by its own recent report,
// advertising capacity and waiting for room for nothing, under an unsealed admission:
// the only state in which GitHub not offering a queued job is GitHub's doing.
func strandedTierWaitsForNothing(sealed bool, report alloc.TierCapacity, now time.Time) bool {
	if sealed {
		return false
	}

	observed, err := time.Parse(time.RFC3339Nano, report.ObservedAt)
	if err != nil || now.Sub(observed) > strandedObservedWithin {
		return false
	}

	listener := report.Listener

	return listener.Sent != nil && *listener.Sent > 0 && listener.Waiting == 0
}

// decideStranded records a decision and reports whether this call made it. A launch
// decided, by this call or another, is not asked about again.
func (s *Server) decideStranded(ctx context.Context, sweep *rerunSweep, miss *alloc.LaunchMiss,
	decision alloc.StrandedDecision, now time.Time,
) bool {
	decided, err := s.alloc.DecideStranded(ctx, miss.LeaseID, decision, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not record whether GitHub kept offering a job; asking again later",
				"tier", miss.Tier, "lease", miss.LeaseID, "error", err)
		}

		return false
	}
	delete(sweep.strandedAsked, miss.LeaseID)

	return decided
}

// strandedReadFailed pauses an installation GitHub throttles or refuses, as a re-run's
// read does, and leaves the launch to be asked about again.
func (s *Server) strandedReadFailed(ctx context.Context, miss *alloc.LaunchMiss, installation string, err error,
	sweep *rerunSweep, now time.Time,
) {
	switch {
	case ctx.Err() != nil:
	case errors.Is(err, ErrRerunThrottled):
		sweep.paused[installation] = now.Add(rerunThrottled)
	case errors.Is(err, ErrRerunRefused):
		sweep.paused[installation] = now.Add(rerunRefused)
		s.log.Warn("GitHub refused this App installation a run's jobs; telling whether it stopped offering a "+
			"job needs the App to hold `actions: read`", "tier", miss.Tier, "installation", installation,
			"error", err)
	default:
		s.log.Warn("could not read a run's jobs to tell whether GitHub stopped offering one; asking again later",
			"tier", miss.Tier, "run", miss.RunID, "job", miss.JobName, "error", err)
	}
}
