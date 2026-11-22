package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// RunRecovery reads a run's whole latest attempt, cancels a run and asks GitHub to
// re-run its failed jobs: what recovering a job GitHub stopped offering is made of
// (#911, retry.stranded). A target's Provisioner implements it over the App's REST
// client, which needs `actions: write` for the two requests.
type RunRecovery interface {
	QueuedAttempt(ctx context.Context, owner, repository string, runID int64) (QueuedAttempt, error)
	CancelRun(ctx context.Context, owner, repository string, runID int64) error
	RerunFailedJobs(ctx context.Context, owner, repository string, runID int64) error
}

// QueuedAttempt is a run as GitHub records it now: whether it is over, the attempt it
// is on and, unless it is over, every job of that attempt.
type QueuedAttempt struct {
	Completed bool
	Attempt   int64
	Jobs      []QueuedJob
}

// QueuedJob is one job of a run's latest attempt.
type QueuedJob struct {
	Name    string
	Attempt int64
	// Unstarted is a job queued with no runner, and Done one GitHub records
	// completed. A job neither is running, or taken by a runner.
	Unstarted bool
	Done      bool
}

// ErrRunCompleted is a cancel GitHub refused because the run is over.
var ErrRunCompleted = errors.New("server: the run is already completed")

const (
	// strandedRecoveryReadsPerInstallation is how many recoveries under way one pass
	// reads through one App installation, beside the detector's share, and
	// strandedBeginsPerInstallation how many stranded jobs it reads to begin one: a
	// share each, so recoveries GitHub cannot be read about never keep one from
	// beginning.
	strandedRecoveryReadsPerInstallation = 2
	strandedBeginsPerInstallation        = 1
	// strandedRecoveryTries is how many times one of a recovery's two requests is
	// sent before the recovery is given up.
	strandedRecoveryTries = 3
	// strandedRecoveryWindow is how long a recovery may take, and how long after its
	// decision a stranded job may still begin one.
	strandedRecoveryWindow = 24 * time.Hour
	// strandedRecoveryRows bounds what one pass reads of the ledger.
	strandedRecoveryRows = 100
)

// recoverStranded takes each recovery of a job GitHub stopped offering one step
// further, and begins one for each stranded job of a tier that asks (#911).
//
// IT CANCELS A RUN THAT MAY BE SOMEONE'S, so every step is taken only on GitHub's
// record read in the same pass, and the ledger says where a recovery is before each
// request leaves. A cancel is sent only while every job of the run's latest attempt
// is queued with no runner or over, so it ends nothing that is running, and only
// for a launch that ended after its tier started asking, once per run. A re-run is
// sent only once GitHub records the very attempt billet cancelled as completed, and
// one that moved past it by any other hand is left alone. A recovery that cancelled
// a run and cannot re-run it says so, with the run's URL.
func (s *Server) recoverStranded(ctx context.Context, targets map[string]Target, sweep *rerunSweep,
	budgets map[string][]RESTHeadroom, now time.Time,
) {
	if sweep.strandedAsking == nil {
		var tiers []string
		for i := range s.tiers {
			if s.tiers[i].RecoversStranded() {
				tiers = append(tiers, s.tiers[i].Label)
			}
		}

		asking, err := s.alloc.AskStranded(ctx, tiers)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("could not record which tiers recover stranded jobs; trying again", "error", err)
			}

			return
		}
		sweep.strandedAsking = asking
	}

	// WHAT THE PASS REMEMBERS OUTLIVES NOTHING IT IS ABOUT: a recovery ends within its
	// window, and a launch leaves the list after it.
	for run, at := range sweep.recoveryRead {
		if now.Sub(at) > strandedRecoveryWindow {
			delete(sweep.recoveryRead, run)
		}
	}
	for lease, at := range sweep.recoveryAsked {
		if now.Sub(at) > strandedRecoveryWindow {
			delete(sweep.recoveryAsked, lease)
		}
	}

	// WHAT IS UNDER WAY FIRST, whether or not its tier still asks, within a share of its
	// own: a run billet may have cancelled must not be left cancelled because the config
	// changed. One page a pass from where the last ended, and the one read longest ago
	// first, so recoveries GitHub cannot be read about keep none waiting.
	open, err := s.alloc.OpenStrandedRecoveries(ctx, sweep.recoveryOpenFrom, strandedRecoveryRows)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not list the stranded-job recoveries under way; trying again", "error", err)
		}

		return
	}
	sweep.recoveryOpenFrom = alloc.RecoveryCursor{}
	if len(open) == strandedRecoveryRows {
		sweep.recoveryOpenFrom = open[len(open)-1].After()
	}

	slices.SortStableFunc(open, func(a, b alloc.StrandedRecovery) int {
		return sweep.recoveryRead[recoveryRun(&a)].Compare(sweep.recoveryRead[recoveryRun(&b)])
	})
	advanced := map[string]int{}
	for i := range open {
		if ctx.Err() != nil {
			return
		}
		s.advanceRecovery(ctx, targets, sweep, budgets, advanced, &open[i], now)
	}

	if len(sweep.strandedAsking) == 0 {
		return
	}

	// ONE PAGE A PASS, FROM WHERE THE LAST ENDED, and from the start once a page comes
	// back short, so launches that stay unsafe to cancel cannot keep another from
	// being listed. The list holds only what a tier asking may take.
	jobs, err := s.alloc.StrandedToRecover(ctx, now.Add(-strandedRecoveryWindow), sweep.recoveryFrom,
		strandedRecoveryRows)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not list the stranded jobs to recover; trying again", "error", err)
		}

		return
	}
	sweep.recoveryFrom = alloc.StrandedCursor{}
	if len(jobs) == strandedRecoveryRows {
		sweep.recoveryFrom = jobs[len(jobs)-1].After()
	}

	// LEAST RECENTLY ASKED FIRST, a launch never asked before any that was.
	due := make([]*alloc.StrandedLaunch, 0, len(jobs))
	for i := range jobs {
		if at, ok := sweep.recoveryAsked[jobs[i].LeaseID]; ok && now.Sub(at) < strandedAgain {
			continue
		}
		due = append(due, &jobs[i])
	}
	slices.SortStableFunc(due, func(a, b *alloc.StrandedLaunch) int {
		return sweep.recoveryAsked[a.LeaseID].Compare(sweep.recoveryAsked[b.LeaseID])
	})
	begun := map[string]int{}
	for _, job := range due {
		if ctx.Err() != nil {
			return
		}
		s.beginRecovery(ctx, targets, sweep, budgets, begun, job, now)
	}
}

// recoveryRun is the run a recovery is of.
func recoveryRun(r *alloc.StrandedRecovery) rerunRun {
	return rerunRun{repository: r.Repository, run: r.RunID}
}

// recoveryReader is the client, installation and repository a recovery reads its
// run through, false where it cannot be read on this pass.
func (s *Server) recoveryReader(targets map[string]Target, sweep *rerunSweep, budgets map[string][]RESTHeadroom,
	read map[string]int, share int, tier, repository string, now time.Time,
) (RunRecovery, string, string, string, bool) {
	owner, name, ok := config.SplitRepository(repository)
	if !ok {
		return nil, "", "", "", false
	}

	target := targets[tier]
	client, reaches := target.Provisioner.(RunRecovery)
	if !reaches {
		return nil, "", "", "", false
	}
	installation := installationOf(target)
	if read[installation] >= share || now.Before(sweep.paused[installation]) {
		return nil, "", "", "", false
	}
	if _, _, _, low := inReserve(budgets[installation], now); low {
		return nil, "", "", "", false
	}
	read[installation]++

	return client, installation, owner, name, true
}

// beginRecovery cancels the run of one stranded job, when everything says it may.
func (s *Server) beginRecovery(ctx context.Context, targets map[string]Target, sweep *rerunSweep,
	budgets map[string][]RESTHeadroom, read map[string]int, job *alloc.StrandedLaunch, now time.Time,
) {
	// THE LEDGER AGAIN, as the decision read it: an assignment since that is, or may
	// be, the job means GitHub may be offering it after all. Asked here to spare a read
	// of GitHub, and again in the write that authorises the cancel.
	later, err := s.alloc.LaterLeasesOfJob(ctx, job.Tier, job.RunID, job.JobName, job.Ended)
	if err != nil || later.Same > 0 || later.Unnamed > 0 {
		return
	}

	client, installation, owner, name, ok := s.recoveryReader(targets, sweep, budgets, read, strandedBeginsPerInstallation,
		job.Tier,
		job.Repository, now)
	if !ok {
		return
	}
	sweep.recoveryAsked[job.LeaseID] = now

	attempt, err := client.QueuedAttempt(ctx, owner, name, job.RunID)
	if err != nil {
		s.recoveryFailed(ctx, sweep, installation, job.Tier, job.Repository, job.RunID, err, now)

		return
	}
	if safe, _ := recoverable(attempt, job.JobName); !safe {
		return
	}

	recovery := alloc.StrandedRecovery{Repository: job.Repository, RunID: job.RunID, Attempt: attempt.Attempt,
		Tier: job.Tier, LeaseID: job.LeaseID, JobName: job.JobName, Ended: job.Ended,
		State: alloc.RecoveryCancelling, Tries: 1}
	begun, err := s.alloc.BeginStrandedRecovery(ctx, recovery)
	if err != nil || !begun {
		if err != nil && !errors.Is(err, alloc.ErrMayBeOffered) && ctx.Err() == nil {
			s.log.Warn("could not record a stranded-job recovery; trying again", "tier", job.Tier,
				"run", job.RunID, "error", err)
		}

		return
	}
	delete(sweep.recoveryAsked, job.LeaseID)

	s.log.Warn("cancelling a run to recover a job GitHub stopped offering; its failed jobs are re-run once it "+
		"is cancelled", "tier", job.Tier, "repository", job.Repository, "run", job.RunID, "attempt", attempt.Attempt,
		"job", job.JobName, "url", runURL(job.Repository, job.RunID))
	s.sendCancel(ctx, client, sweep, installation, &recovery, owner, name, now)
}

// recoverable says whether a run's latest attempt may be cancelled to recover its
// job named name: GitHub has a job of that name queued with no runner, and every
// job of the attempt is queued with no runner or over, so a cancel ends nothing
// running, and says it is known. A completed attempt lists no jobs, so none waits. It
// is not known for a read that disagrees with itself, a job listed against another
// attempt, which decides nothing either way.
func recoverable(attempt QueuedAttempt, name string) (bool, bool) {
	for _, job := range attempt.Jobs {
		if job.Attempt != attempt.Attempt {
			return false, false
		}
	}

	waiting := false
	for _, job := range attempt.Jobs {
		switch {
		case !job.Unstarted && !job.Done:
			return false, true
		case job.Unstarted && job.Name == name:
			waiting = true
		}
	}

	return waiting, true
}

// advanceRecovery takes one recovery under way one step further.
func (s *Server) advanceRecovery(ctx context.Context, targets map[string]Target, sweep *rerunSweep,
	budgets map[string][]RESTHeadroom, read map[string]int, recovery *alloc.StrandedRecovery, now time.Time,
) {
	if now.Sub(recovery.Created) > strandedRecoveryWindow {
		s.endRecovery(ctx, recovery, alloc.RecoveryExpired, "it was not finished within a day")

		return
	}

	client, installation, owner, name, ok := s.recoveryReader(targets, sweep, budgets, read,
		strandedRecoveryReadsPerInstallation, recovery.Tier,
		recovery.Repository, now)
	if !ok {
		return
	}

	sweep.recoveryRead[recoveryRun(recovery)] = now
	attempt, err := client.QueuedAttempt(ctx, owner, name, recovery.RunID)
	if err != nil {
		s.recoveryFailed(ctx, sweep, installation, recovery.Tier, recovery.Repository, recovery.RunID, err, now)

		return
	}

	switch {
	case attempt.Attempt < recovery.Attempt:
		// A READ THAT DISAGREES WITH THE ONE THE RECOVERY BEGAN FROM decides nothing.
		return
	case attempt.Attempt > recovery.Attempt && recovery.State == alloc.RecoveryRerunning:
		s.endRecovery(ctx, recovery, alloc.RecoveryRecovered, "")
	case attempt.Attempt > recovery.Attempt:
		// THE RUN MOVED ON WITHOUT BILLET'S RE-RUN: a person re-ran it, and it is theirs.
		s.endRecovery(ctx, recovery, alloc.RecoverySuperseded, "the run moved to a later attempt before billet "+
			"asked for one")
	case recovery.State == alloc.RecoveryCancelling && attempt.Completed:
		// THE ATTEMPT BILLET CANCELLED IS OVER, so its failed jobs, the cancelled one
		// among them, can be run again.
		moved, err := s.alloc.MoveStrandedRecovery(ctx, *recovery, alloc.RecoveryRerunning, 1)
		if err != nil || !moved {
			return
		}
		recovery.State, recovery.Tries = alloc.RecoveryRerunning, 1
		s.sendRerunOfRecovery(ctx, client, sweep, installation, recovery, owner, name, now)
	case recovery.State == alloc.RecoveryCancelling:
		// THE CANCEL HAS NOT LANDED: a landed one completes the run within seconds. Sent
		// again only while the run is still safe to cancel, and a read that disagrees
		// with itself decides nothing.
		safe, known := recoverable(attempt, recovery.JobName)
		switch {
		case !known:
		case !safe:
			s.endRecovery(ctx, recovery, alloc.RecoverySuperseded, "the run went on before billet's cancel landed")
		case recovery.Tries >= strandedRecoveryTries:
			s.endRecovery(ctx, recovery, alloc.RecoveryFailed, "GitHub did not take the cancel")
		default:
			moved, err := s.alloc.MoveStrandedRecovery(ctx, *recovery, alloc.RecoveryCancelling, recovery.Tries+1)
			if errors.Is(err, alloc.ErrMayBeOffered) {
				s.endRecovery(ctx, recovery, alloc.RecoverySuperseded, "the tier has since been assigned what may "+
					"be the job")

				return
			}
			if err != nil || !moved {
				return
			}
			recovery.Tries++
			s.sendCancel(ctx, client, sweep, installation, recovery, owner, name, now)
		}
	case !attempt.Completed:
		// A RUN GOING ON THE ATTEMPT A RE-RUN WAS ASKED OF took it: straight after the
		// request GitHub answers `queued` on the old attempt (measured 2026-11-04).
		s.endRecovery(ctx, recovery, alloc.RecoveryRecovered, "")
	case recovery.Tries >= strandedRecoveryTries:
		s.endRecovery(ctx, recovery, alloc.RecoveryFailed, "GitHub did not take the re-run")
	default:
		moved, err := s.alloc.MoveStrandedRecovery(ctx, *recovery, alloc.RecoveryRerunning, recovery.Tries+1)
		if err != nil || !moved {
			return
		}
		recovery.Tries++
		s.sendRerunOfRecovery(ctx, client, sweep, installation, recovery, owner, name, now)
	}
}

// sendCancel sends a recovery's cancel. Whatever GitHub answers, the next pass reads
// the run before anything else is sent.
func (s *Server) sendCancel(ctx context.Context, client RunRecovery, sweep *rerunSweep, installation string,
	recovery *alloc.StrandedRecovery, owner, name string, now time.Time,
) {
	err := client.CancelRun(ctx, owner, name, recovery.RunID)
	switch {
	case err == nil, errors.Is(err, ErrRunCompleted):
	default:
		s.recoveryFailed(ctx, sweep, installation, recovery.Tier, recovery.Repository, recovery.RunID, err, now)
	}
}

// sendRerunOfRecovery sends a recovery's re-run, and ends the recovery when GitHub
// takes it.
func (s *Server) sendRerunOfRecovery(ctx context.Context, client RunRecovery, sweep *rerunSweep,
	installation string, recovery *alloc.StrandedRecovery, owner, name string, now time.Time,
) {
	err := client.RerunFailedJobs(ctx, owner, name, recovery.RunID)
	switch {
	case err == nil:
		s.endRecovery(ctx, recovery, alloc.RecoveryRecovered, "")
	case errors.Is(err, ErrRerunRunning):
		// SOMETHING STARTED THE RUN SINCE IT WAS READ; the next read says what.
	default:
		s.recoveryFailed(ctx, sweep, installation, recovery.Tier, recovery.Repository, recovery.RunID, err, now)
	}
}

// endRecovery settles a recovery, and says what it came to: an ERROR for a run billet
// may have cancelled and did not re-run, with what a person does about it.
func (s *Server) endRecovery(ctx context.Context, recovery *alloc.StrandedRecovery, to alloc.StrandedRecoveryState,
	detail string,
) {
	settled, err := s.alloc.SettleStrandedRecovery(ctx, *recovery, to, detail)
	if err != nil || !settled {
		if err != nil && ctx.Err() == nil {
			s.log.Warn("could not record what a stranded-job recovery came to; trying again",
				"run", recovery.RunID, "error", err)
		}

		return
	}

	attrs := []any{"tier", recovery.Tier, "repository", recovery.Repository, "run", recovery.RunID,
		"attempt", recovery.Attempt, "url", runURL(recovery.Repository, recovery.RunID)}
	switch {
	case to == alloc.RecoveryRecovered:
		s.log.Info("recovered a job GitHub stopped offering: its run was cancelled and its failed jobs re-run",
			attrs...)
	case recovery.State == alloc.RecoveryRerunning:
		s.log.Error("billet cancelled a run to recover a job GitHub stopped offering, and could not re-run it; "+
			"re-run its failed jobs", append(attrs, "came_to", to, "detail", detail)...)
	default:
		s.log.Warn("a stranded-job recovery ended without a re-run", append(attrs, "came_to", to,
			"detail", detail)...)
	}
}

// recoveryFailed pauses an installation GitHub throttles or refuses, as a re-run's
// read does; anything else waits for the next pass.
func (s *Server) recoveryFailed(ctx context.Context, sweep *rerunSweep, installation, tier, repository string,
	runID int64, err error, now time.Time,
) {
	switch {
	case ctx.Err() != nil:
	case errors.Is(err, ErrRerunThrottled):
		sweep.paused[installation] = now.Add(rerunThrottled)
	case errors.Is(err, ErrRerunRefused):
		sweep.paused[installation] = now.Add(rerunRefused)
		s.log.Warn("GitHub refused this App installation a stranded-job recovery; a tier with retry.stranded "+
			"needs the App to hold `actions: write`", "tier", tier, "installation", installation, "error", err)
	default:
		s.log.Warn("could not take a stranded-job recovery further; trying again later", "tier", tier,
			"repository", repository, "run", runID, "error", err)
	}
}

// runURL is a run's page on GitHub.
func runURL(repository string, runID int64) string {
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d", repository, runID)
}
