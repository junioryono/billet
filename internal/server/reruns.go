package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// RunReruns reads the attempt a workflow run is on and asks GitHub to run it again.
// A target's Provisioner implements it over the App's REST client.
type RunReruns interface {
	LatestAttempt(ctx context.Context, owner, repository string, runID int64) (RunAttempt, error)
	RerunFailedJobs(ctx context.Context, owner, repository string, runID int64) error
	RerunJob(ctx context.Context, owner, repository string, jobID int64) error
}

// RunAttempt is a run as GitHub records it now. Jobs are the latest attempt's,
// and only a completed run lists them.
type RunAttempt struct {
	Completed bool
	Attempt   int64
	Jobs      []AttemptJob
}

// AttemptJob is one job of a run's latest attempt.
type AttemptJob struct {
	ID         int64
	RunnerName string
	// Attempt is the attempt GitHub says the job belongs to.
	Attempt int64
	// RunnerLost is GitHub's record showing the runner vanished under the job,
	// which a job failing on its own never leaves.
	RunnerLost bool
	// Failed is a job a request for the run's failed jobs could run again.
	Failed bool
}

var (
	// ErrRerunThrottled is a re-run's read or request GitHub's rate limit refused.
	ErrRerunThrottled = errors.New("server: GitHub is rate-limiting this App installation")
	// ErrRerunRefused is one GitHub refused the App with a 401 or 403: an App
	// without `actions: write`, a revoked key or a suspended App.
	ErrRerunRefused = errors.New("server: GitHub refused this App installation the re-run")
	// ErrRerunRunning is a request GitHub refused because the run is going.
	ErrRerunRunning = errors.New("server: the run is already running")
)

// errRunUnfinished says a run has jobs left, which is not a failure of any kind.
var errRunUnfinished = errors.New("server: the run has not finished")

const (
	// rerunEvery is how often the loop looks for jobs to decide about.
	rerunEvery = time.Minute
	// rerunSettle is how long after a job's result its run waits. A run's results
	// arrive one message at a time, so deciding on the first could read a sibling
	// whose completion billet has not heard yet as a job that failed on its own.
	rerunSettle = 2 * time.Minute
	// rerunWindow is how long a job can wait for its run to finish, or for GitHub to
	// answer about it, before it is decided as expired. A re-run after that is an
	// event nobody is waiting for.
	rerunWindow = 24 * time.Hour
	// rerunPageRows and rerunPages bound one pass's reading of the ledger. A page of
	// a tier that does not ask is paged past, never decided.
	rerunPageRows = 1000
	rerunPages    = 10
	// rerunOpenPageRows and rerunOpenPages bound the requests in doubt one pass reads,
	// all of them on every pass: there is at most one per run, and each is settled
	// within a few passes, so these are bounds on a ledger something else is wrong with.
	rerunOpenPageRows = 100
	rerunOpenPages    = 100
	// rerunRunsPerInstallation is how many runs one pass reads through one App
	// installation, the same share of its budget the queue-time sweep takes.
	rerunRunsPerInstallation = 3
	// rerunMaxTries is how many times one request is sent. A failure to ask GitHub
	// is reported, and never retried forever.
	rerunMaxTries = 5
	// rerunRetryFirst is the wait after a read or a request that failed, doubled
	// after each until rerunRetryMost.
	rerunRetryFirst = time.Minute
	rerunRetryMost  = 30 * time.Minute
	// rerunUnfinishedEvery is how often a run with jobs left is read again.
	rerunUnfinishedEvery = 2 * time.Minute
	// An installation GitHub throttles waits rerunThrottled before its next read,
	// and one it refuses waits rerunRefused.
	rerunThrottled = 15 * time.Minute
	rerunRefused   = time.Hour
	// rerunDetailBytes bounds what a failed request records of GitHub's answer.
	rerunDetailBytes = 500
)

// rerunRun is one workflow run of one repository. It is a run's identity across
// targets: a repository two targets serve has one run, and one request for it.
type rerunRun struct {
	repository string
	run        int64
}

// rerunSweep is what the loop remembers between passes: each run's backoff, each
// installation paused by a throttle or a refusal, the last failure of a request
// still in doubt, for the row it settles, since when each tier has asked, when each
// run was last read, and where in the candidates the next pass reads on from.
type rerunSweep struct {
	retries  map[rerunRun]queueTimeRetry
	paused   map[string]time.Time
	reserved map[string]bool
	failures map[rerunRun]string
	asked    map[string]time.Time
	tried    map[rerunRun]time.Time
	listed   alloc.RerunCursor
	// began is when the pass began on the wall clock, which clock measures from.
	began time.Time
}

// clock is the pass's now moved on by how long the pass has run, for a deadline
// checked after a read that may have been slow.
func (s *rerunSweep) clock(now time.Time) time.Time {
	if s.began.IsZero() {
		return now
	}

	return now.Add(time.Since(s.began))
}

func newRerunSweep() *rerunSweep {
	return &rerunSweep{retries: map[rerunRun]queueTimeRetry{}, paused: map[string]time.Time{},
		reserved: map[string]bool{}, failures: map[rerunRun]string{}, tried: map[rerunRun]time.Time{}}
}

// rerunWork is one run a pass will read, through the first target that serves it.
// What it decides from is read again when it is decided: the pages that brought it up
// may not have held all of it.
type rerunWork struct {
	run          rerunRun
	target       string
	installation string
	client       RunReruns
	// doubt is the run's request in doubt, which a pass that cannot read the run
	// settles once it is two days old.
	doubt *alloc.RerunRequest
}

// rerunOverdue says whether a request in doubt is past twice the window, after which
// one whose run is not read is settled without a read: a paused, exhausted or
// refusing installation must not hold its jobs for ever.
func rerunOverdue(request *alloc.RerunRequest, now time.Time) bool {
	return request != nil && !request.Created.IsZero() && now.Sub(request.Created) > 2*rerunWindow
}

// rerunLimits is each tier's retry.disrupted, for the tiers that set one.
func (s *Server) rerunLimits() map[string]int64 {
	limits := map[string]int64{}
	for i := range s.tiers {
		if n := s.tiers[i].DisruptedReruns(); n > 0 {
			limits[s.tiers[i].Label] = int64(n)
		}
	}

	return limits
}

// rerunDisrupted asks GitHub to re-run the jobs billet proved it ended, until ctx
// ends. It can stop nothing: a pass that fails is logged and the next tries again.
//
// IT RUNS WHERE NO TIER ASKS TOO: its first pass records that none does, which is
// what makes a tier asking again start again, and each pass after reads the requests
// in doubt, which still have to be settled when their tier stops asking, and asks
// GitHub only about them.
func (s *Server) rerunDisrupted(ctx context.Context, targets map[string]Target, limits map[string]int64) {
	every := rerunEvery
	if s.rerunEvery > 0 {
		every = s.rerunEvery
	}

	sweep := newRerunSweep()
	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		s.rerunOnce(ctx, targets, limits, sweep, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// rerunOnce is one pass at now.
func (s *Server) rerunOnce(ctx context.Context, targets map[string]Target, limits map[string]int64,
	sweep *rerunSweep, now time.Time,
) {
	sweep.began = time.Now()
	if sweep.asked == nil {
		tiers := make([]string, 0, len(limits))
		for tier := range limits {
			tiers = append(tiers, tier)
		}

		asked, err := s.alloc.AskReruns(ctx, tiers)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("could not record which tiers ask for re-runs; trying again", "error", err)
			}

			return
		}
		sweep.asked = asked
	}

	for run, retry := range sweep.retries {
		if now.Sub(retry.after) > rerunWindow {
			delete(sweep.retries, run)
			delete(sweep.failures, run)
		}
	}
	for run, at := range sweep.tried {
		if now.Sub(at) > rerunWindow {
			delete(sweep.tried, run)
		}
	}

	read := map[string]int{}
	budgets := installationBudgets(targets)

	// THE RUN READ LONGEST AGO FIRST, never-read before any, so the few reads an
	// installation has a pass go round every run in turn, and a run that keeps
	// failing cannot hold the share for ever.
	work := s.gatherRerunWork(ctx, targets, limits, sweep, now)
	slices.SortStableFunc(work, func(a, b rerunWork) int {
		return sweep.tried[a.run].Compare(sweep.tried[b.run])
	})

	for i := range work {
		w := &work[i]
		if ctx.Err() != nil {
			return
		}
		// A SHARE SPENT IS NOT A RUN THAT CANNOT BE READ: the runs go round in turn, and
		// this one is read on a pass soon after, which settles it from what GitHub says.
		if read[w.installation] == rerunRunsPerInstallation {
			continue
		}
		if now.Before(sweep.paused[w.installation]) {
			s.settleOverdue(ctx, w, now, "its installation could not read its run within two days")

			continue
		}
		if remaining, limit, reset, low := inReserve(budgets[w.installation], now); low {
			if !sweep.reserved[w.installation] {
				sweep.reserved[w.installation] = true
				s.log.Warn("this App installation's REST budget is down to its reserve; deciding re-runs "+
					"again when GitHub's next hour begins", "target", w.target, "installation", w.installation,
					"remaining", remaining, "limit", limit, "reset", reset)
			}
			s.settleOverdue(ctx, w, now, "its installation's REST budget kept it from being read within two days")

			continue
		}
		delete(sweep.reserved, w.installation)
		read[w.installation]++
		sweep.tried[w.run] = now

		s.afterRerun(ctx, w, s.decideRerun(ctx, w, limits, sweep, now), sweep, now)
	}
}

// settleOverdue settles, without a read, the request in doubt of a run this pass does
// not read, once it is two days old. One whose run is read is settled from the read.
func (s *Server) settleOverdue(ctx context.Context, w *rerunWork, now time.Time, why string) {
	if rerunOverdue(w.doubt, now) {
		s.settleUnread(ctx, *w.doubt, why)
	}
}

// errRerunSettled marks an installation's refusal or throttle that settled the run's
// request in doubt: the installation is paused, and the run is not held back.
var errRerunSettled = errors.New("server: the run's request in doubt was settled")

// afterRerun records what a run's turn asks of the next pass.
func (s *Server) afterRerun(ctx context.Context, w *rerunWork, err error, sweep *rerunSweep, now time.Time) {
	switch {
	case err == nil:
		delete(sweep.retries, w.run)
		return
	case ctx.Err() != nil:
		return
	case errors.Is(err, errRunUnfinished):
		sweep.retries[w.run] = queueTimeRetry{after: now.Add(rerunUnfinishedEvery)}
		return
	case errors.Is(err, ErrRerunThrottled):
		sweep.paused[w.installation] = now.Add(rerunThrottled)
		s.log.Warn("GitHub is rate-limiting this App installation; deciding re-runs again later",
			"target", w.target, "installation", w.installation, "after", rerunThrottled, "error", err)
	case errors.Is(err, ErrRerunRefused):
		sweep.paused[w.installation] = now.Add(rerunRefused)
		s.log.Warn("GitHub refused this App installation a re-run; a tier with retry.disrupted needs the App "+
			"to hold `actions: write`, and a revoked key or a suspended App is refused the same way",
			"target", w.target, "installation", w.installation, "after", rerunRefused, "error", err)
	default:
		s.log.Warn("could not decide a re-run; trying again later",
			"target", w.target, "repository", w.run.repository, "run", w.run.run, "error", err)
	}

	if errors.Is(err, errRerunSettled) {
		delete(sweep.retries, w.run)

		return
	}

	retry := sweep.retries[w.run]
	retry.wait = min(max(2*retry.wait, rerunRetryFirst), rerunRetryMost)
	retry.after = now.Add(retry.wait)
	sweep.retries[w.run] = retry
}

// rerunWorkSet is the runs a pass has found, once each.
type rerunWorkSet struct {
	targets map[string]Target
	index   map[rerunRun]int
	work    []rerunWork
}

// add brings a run up through the first of tiers a target this control plane serves
// can read it through, and says whether one could.
func (set *rerunWorkSet) add(run rerunRun, tiers ...string) bool {
	if _, seen := set.index[run]; seen {
		return true
	}

	for _, tier := range tiers {
		target := set.targets[tier]
		client, reaches := target.Provisioner.(RunReruns)
		if !reaches {
			continue
		}

		set.index[run] = len(set.work)
		set.work = append(set.work, rerunWork{run: run, target: target.Config.Name,
			installation: installationOf(target), client: client})

		return true
	}

	return false
}

// doubt records the request in doubt of a run the set holds.
func (set *rerunWorkSet) doubt(run rerunRun, request alloc.RerunRequest) {
	if i, ok := set.index[run]; ok {
		set.work[i].doubt = &request
	}
}

// gatherRerunWork finds the runs worth reading this pass: those with a request in
// doubt, every one, and those with a job awaiting a decision, and decides as expired
// what has waited too long to be worth a request.
//
// THE CANDIDATES ARE READ ON FROM WHERE THE LAST PASS STOPPED, and back to their start
// once they reach their end, so what one pass cannot reach is not passed over for ever
// by what comes before it. The newest page is read on every pass, so a job that just
// went red does not wait for the backlog.
func (s *Server) gatherRerunWork(ctx context.Context, targets map[string]Target, limits map[string]int64,
	sweep *rerunSweep, now time.Time,
) []rerunWork {
	set := &rerunWorkSet{targets: targets, index: map[rerunRun]int{}}

	waiting := func(run rerunRun) bool {
		retry, ok := sweep.retries[run]
		return ok && now.Before(retry.after)
	}

	var cursor alloc.OpenRerunCursor

	for range rerunOpenPages {
		open, err := s.alloc.OpenReruns(ctx, cursor, rerunOpenPageRows)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("could not list the re-runs in doubt; trying again", "error", err)
			}

			return nil
		}

		for i := range open {
			s.bringUpOpenRerun(ctx, set, &open[i], now, waiting)
		}

		if len(open) < rerunOpenPageRows {
			break
		}
		cursor = open[len(open)-1].Cursor
	}

	since, asking := now, false
	for _, at := range sweep.asked {
		if !asking || at.Before(since) {
			since, asking = at, true
		}
	}
	if !asking {
		return set.work
	}

	scan := func(from alloc.RerunCursor, pages int) (alloc.RerunCursor, bool) {
		cursor := from
		for range pages {
			jobs, err := s.alloc.RerunCandidates(ctx, since, cursor, rerunPageRows)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("could not list the jobs awaiting a re-run decision; trying again", "error", err)
				}

				return from, false
			}

			for i := range jobs {
				job := &jobs[i]

				// A JOB A TIER DID NOT ASK ABOUT WHEN IT ENDED is not re-run because the
				// config changed since.
				asked, opted := sweep.asked[job.Tier]
				if _, limited := limits[job.Tier]; !opted || !limited || job.ResultAt.Before(asked) {
					continue
				}

				// A JOB A REQUEST IN DOUBT COVERS waits for that request, whose run the
				// list of requests brings up: its outcome is written on the job then.
				if job.Bound > 0 {
					continue
				}

				if now.Sub(job.ResultAt) > rerunWindow {
					s.decideJob(ctx, job, alloc.RerunExpired, 0)

					continue
				}

				run := rerunRun{repository: job.Repository, run: job.RunID}
				if !waiting(run) {
					set.add(run, job.Tier)
				}
			}

			if len(jobs) < rerunPageRows {
				return alloc.RerunCursor{}, true
			}
			cursor = jobs[len(jobs)-1].Cursor
		}

		return cursor, true
	}

	pages := rerunPages
	if sweep.listed != (alloc.RerunCursor{}) {
		if _, ok := scan(alloc.RerunCursor{}, 1); !ok {
			return set.work
		}
		pages--
	}
	if cursor, ok := scan(sweep.listed, pages); ok {
		sweep.listed = cursor
	}

	return set.work
}

// bringUpOpenRerun adds the run of a request in doubt, through the tier the list
// names or, where that one has no reader, any other it still covers.
func (s *Server) bringUpOpenRerun(ctx context.Context, set *rerunWorkSet, request *alloc.OpenRerun, now time.Time,
	waiting func(rerunRun) bool,
) {
	run := rerunRun{repository: request.Repository, run: request.RunID}

	if waiting(run) {
		// A RUN WHOSE READS KEEP FAILING waits between them, and is not read past two
		// days either.
		if rerunOverdue(&request.RerunRequest, now) {
			s.settleUnread(ctx, request.RerunRequest, "its run could not be read within two days")
		}

		return
	}
	if set.add(run, request.Tier) {
		set.doubt(run, request.RerunRequest)

		return
	}

	tiers, err := s.alloc.BoundRerunTiers(ctx, request.RerunRequest)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not read the tiers a re-run in doubt covers; trying again", "error", err)
		}

		return
	}
	if set.add(run, tiers...) {
		set.doubt(run, request.RerunRequest)

		return
	}

	// PAST THE WINDOW IT IS SETTLED, never sent: it may have landed, and counting it
	// only makes billet ask less.
	if s.rerunWindowPassed(request.RerunRequest, now) {
		s.settleUnread(ctx, request.RerunRequest, "no target this control plane serves can read its run")

		return
	}

	s.log.Warn("no target this control plane serves can read the run of a re-run in doubt; it waits",
		"repository", request.Repository, "run", request.RunID, "tiers", tiers)
}

// rerunWindowPassed says whether a request in doubt has waited past the window.
func (s *Server) rerunWindowPassed(request alloc.RerunRequest, now time.Time) bool {
	return !request.Created.IsZero() && now.Sub(request.Created) > rerunWindow
}

// settleUnread settles a request in doubt whose run could not be read in time as
// unconfirmed: it may have landed, and counting it only makes billet ask less. Its
// jobs are decided at last, which a job covered by a request in doubt otherwise never
// is.
func (s *Server) settleUnread(ctx context.Context, request alloc.RerunRequest, why string) bool {
	if err := s.alloc.SettleRerun(ctx, request, alloc.RerunUnconfirmed, why); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not settle a re-run in doubt past its window; trying again",
				"repository", request.Repository, "run", request.RunID, "error", err)
		}

		return false
	}

	s.log.Warn("settled a re-run in doubt as unconfirmed, counted as landed since it may have; nothing was sent",
		"repository", request.Repository, "run", request.RunID, "attempt", request.Attempt, "why", why)

	return true
}

// decideJob records a decision on one job, and logs a write that failed: the job
// stays undecided and the next pass decides it again.
func (s *Server) decideJob(ctx context.Context, job *alloc.RerunCandidate, decision alloc.RerunDecision, attempt int64) {
	if err := s.alloc.RecordRerunDecision(ctx, job.LeaseID, decision, attempt); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("could not record a re-run decision; trying again",
				"tier", job.Tier, "lease", job.LeaseID, "decision", decision, "error", err)
		}

		return
	}

	s.log.Info("decided a disrupted job's re-run", "tier", job.Tier, "lease", job.LeaseID,
		"repository", job.Repository, "run", job.RunID, "disruption", job.Disruption, "decision", decision)
}

// decideRerun takes one run as far as it can go on this pass. ADR-023 holds the
// rule; the order below is what keeps it, and each step's refusal is the decision.
func (s *Server) decideRerun(ctx context.Context, w *rerunWork, limits map[string]int64, sweep *rerunSweep,
	now time.Time,
) error {
	owner, repository, ok := config.SplitRepository(w.run.repository)
	if !ok {
		return fmt.Errorf("server: %q is not a repository", w.run.repository)
	}

	// A REQUEST IN DOUBT FIRST, read from the ledger rather than from the page that
	// brought the run up, so no job of the run is decided while one may have landed.
	open, doubted, err := s.alloc.OpenRerun(ctx, w.run.repository, w.run.run)
	if err != nil {
		return err
	}

	// A RUN STILL SETTLING COSTS GITHUB NOTHING: the ledger alone says to wait. It is
	// asked again after the read below, which is the answer that counts.
	if !doubted {
		if _, err := s.runCandidates(ctx, w.run, limits, sweep, now); errors.Is(err, errRunUnfinished) {
			return err
		}
	}

	attempt, err := w.client.LatestAttempt(ctx, owner, repository, w.run.run)
	if err == nil {
		// TWO READS THAT DISAGREE ABOUT THE LATEST ATTEMPT decide nothing, and send
		// nothing: a request for the run's failed jobs reaches whatever attempt GitHub
		// is on.
		for i := range attempt.Jobs {
			if attempt.Jobs[i].Attempt > attempt.Attempt {
				err = fmt.Errorf("server: run %d is on attempt %d and lists a job of attempt %d",
					w.run.run, attempt.Attempt, attempt.Jobs[i].Attempt)
			}
		}
	}
	if err == nil && doubted && attempt.Attempt < open.Attempt {
		err = fmt.Errorf("server: run %d is on attempt %d, before the attempt %d billet asked about",
			w.run.run, attempt.Attempt, open.Attempt)
	}
	if err != nil {
		// A RUN THAT CANNOT BE READ, OR READ SENSIBLY, holds a request in doubt only
		// until the window: a run deleted at GitHub must not hold its jobs for ever. A
		// throttle or a refusal is the installation's and may pass, so it holds until
		// twice the window.
		transient := errors.Is(err, ErrRerunThrottled) || errors.Is(err, ErrRerunRefused)
		if doubted && s.rerunWindowPassed(open, now) && ctx.Err() == nil &&
			(!transient || rerunOverdue(&open, now)) {
			settled := s.settleUnread(ctx, open, rerunDetail(err))

			// THE INSTALLATION'S ANSWER STILL PAUSES IT, for the other runs it reads; the
			// run, settled, waits for nothing of its own.
			if transient && settled {
				return fmt.Errorf("%w: %w", errRerunSettled, err)
			}
			if transient {
				return err
			}

			return nil
		}

		return err
	}

	if doubted {
		settled, err := s.settleOpenRerun(ctx, w, open, attempt, limits, owner, repository, sweep, now)
		if err != nil || !settled {
			return err
		}
	}

	if !attempt.Completed {
		return errRunUnfinished
	}

	// THE SIBLINGS BEFORE THE CANDIDATES, and the order is the guarantee: a sibling
	// heard before this read is a candidate in the next, with a result too new to
	// decide on, and one not heard yet holds the run here.
	if err := s.siblingsHeardFrom(ctx, attempt); err != nil {
		return err
	}

	jobs, err := s.runCandidates(ctx, w.run, limits, sweep, now)
	if err != nil || len(jobs) == 0 {
		return err
	}

	proved, err := s.decideEachJob(ctx, w, jobs, attempt)
	if err != nil || len(proved) == 0 {
		return err
	}

	// A JOB WHOSE RESULT CAME LATE, after its attempt was asked about, BEFORE THE LIMIT:
	// the key allows the attempt no second request, so no new one is being weighed. A
	// request for the run's failed jobs covered it, and its answer is the job's; a
	// request for another job alone did not.
	settled, kind, asked, err := s.alloc.RerunOfAttempt(ctx, w.run.repository, w.run.run, attempt.Attempt)
	if err != nil {
		return err
	}
	if asked {
		if kind != alloc.RerunFailedJobs {
			settled = alloc.RerunMixed
		}
		s.decideAll(ctx, proved, settled, attempt.Attempt)

		return nil
	}

	request, proved, ok, err := s.rerunInWindow(ctx, w, attempt, proved, limits, sweep, now)
	if err != nil || !ok {
		return err
	}

	leases := make([]string, 0, len(proved))
	for i := range proved {
		leases = append(leases, proved[i].candidate.LeaseID)
	}
	if err := s.alloc.BeginRerun(ctx, request, leases); err != nil {
		return err
	}

	return s.sendRerun(ctx, w, request, owner, repository, sweep)
}

// rerunInWindow is the request for the proved jobs still inside the window, false
// when the jobs were decided instead.
//
// THE WINDOW ON THE CLOCK AS IT IS NOW, after the reads, so no job is asked about past
// it however slow GitHub was, and looked at again right before what the jobs come to
// is written: a job past it is decided alone, and the rest is worked out again from
// the others, as it would be had the job expired before the pass. It is then a failed
// job the request must not run again. Each turn round decides one job at least, so it
// ends.
func (s *Server) rerunInWindow(ctx context.Context, w *rerunWork, attempt RunAttempt, proved []provedJob,
	limits map[string]int64, sweep *rerunSweep, now time.Time,
) (alloc.RerunRequest, []provedJob, bool, error) {
	for {
		proved = s.expireCrossed(ctx, proved, attempt.Attempt, sweep.clock(now))
		if len(proved) == 0 {
			return alloc.RerunRequest{}, nil, false, nil
		}

		used, err := s.alloc.RunReruns(ctx, w.run.repository, w.run.run)
		if err != nil {
			return alloc.RerunRequest{}, nil, false, err
		}

		request, exact := exactRequest(w.run, attempt, proved)
		if anyPastWindow(proved, sweep.clock(now)) {
			continue
		}

		switch {
		case used >= allowedFor(proved, limits):
			s.decideAll(ctx, proved, alloc.RerunLimit, attempt.Attempt)
		case !exact:
			s.decideAll(ctx, proved, alloc.RerunMixed, attempt.Attempt)
		default:
			return request, proved, true, nil
		}

		return alloc.RerunRequest{}, nil, false, nil
	}
}

// expireCrossed decides expired every proved job past the window at clock, and
// returns the others.
func (s *Server) expireCrossed(ctx context.Context, proved []provedJob, attempt int64, clock time.Time) []provedJob {
	inWindow := make([]provedJob, 0, len(proved))
	for i := range proved {
		if clock.Sub(proved[i].candidate.ResultAt) > rerunWindow {
			s.decideJob(ctx, proved[i].candidate, alloc.RerunExpired, attempt)

			continue
		}
		inWindow = append(inWindow, proved[i])
	}

	return inWindow
}

// anyPastWindow says whether any proved job is past the window at clock.
func anyPastWindow(proved []provedJob, clock time.Time) bool {
	for i := range proved {
		if clock.Sub(proved[i].candidate.ResultAt) > rerunWindow {
			return true
		}
	}

	return false
}

// runCandidates reads every job of a run awaiting a decision that its tier asks
// about, and says the run is not ready while the newest result is too new: a run's
// results arrive one message at a time.
func (s *Server) runCandidates(ctx context.Context, run rerunRun, limits map[string]int64, sweep *rerunSweep,
	now time.Time,
) ([]alloc.RerunCandidate, error) {
	settle := rerunSettle
	if s.rerunSettle > 0 {
		settle = s.rerunSettle
	}

	var (
		jobs   []alloc.RerunCandidate
		cursor alloc.RerunCursor
	)

	// TO ITS END, never a prefix: a run decided from part of its jobs would read the
	// rest as jobs that failed on their own.
	for range rerunPages {
		listed, err := s.alloc.RunRerunCandidates(ctx, run.repository, run.run, cursor, rerunPageRows)
		if err != nil {
			return nil, err
		}

		for i := range listed {
			job := listed[i]

			asked, opted := sweep.asked[job.Tier]
			if _, limited := limits[job.Tier]; !opted || !limited || job.ResultAt.Before(asked) || job.Bound > 0 ||
				now.Sub(job.ResultAt) > rerunWindow {
				continue
			}
			if now.Sub(job.ResultAt) < settle {
				return nil, errRunUnfinished
			}

			jobs = append(jobs, job)
		}

		if len(listed) < rerunPageRows {
			return jobs, nil
		}
		cursor = listed[len(listed)-1].Cursor
	}

	return nil, fmt.Errorf("server: run %d has more candidates than %d pages hold", run.run, rerunPages)
}

// settleOpenRerun answers a request in doubt from GitHub's record of the run, never by
// asking again while it may have landed, and says whether it is settled.
//
// MEASURED 2026-11-04: straight after a 201 the run is `queued` and still on its
// attempt, so the status says it landed before the attempt does. A completed run on
// the attempt asked about never heard it: that one is sent again, unless its tries
// are used or a limit has since been lowered under it.
func (s *Server) settleOpenRerun(ctx context.Context, w *rerunWork, open alloc.RerunRequest, attempt RunAttempt,
	limits map[string]int64, owner, repository string, sweep *rerunSweep, now time.Time,
) (bool, error) {
	switch {
	case !attempt.Completed || attempt.Attempt > open.Attempt:
		if err := s.alloc.SettleRerun(ctx, open, alloc.RerunRequested, ""); err != nil {
			return false, err
		}
		delete(sweep.failures, w.run)
		s.log.Info("a re-run billet asked for landed", "repository", w.run.repository, "run", w.run.run,
			"attempt", open.Attempt)

		if !attempt.Completed {
			return false, errRunUnfinished
		}

		return true, nil
	case open.Tries >= rerunMaxTries:
		return false, s.giveUpRerun(ctx, w, open, alloc.RerunFailed, sweep)
	}

	// NOT SENT PAST THE WINDOW: a job red that long is nobody's news, whatever kept
	// the request from GitHub meanwhile. On the clock as it is now, after the read,
	// rather than as it was when the pass began.
	if s.rerunWindowPassed(open, sweep.clock(now)) {
		sweep.failures[w.run] = "it was not sent within a day of being decided"

		return false, s.giveUpRerun(ctx, w, open, alloc.RerunExpired, sweep)
	}

	// EVERY TIER IT STILL COVERS, AS IT IS NOW, since a limit may have been lowered
	// while the request was in doubt. A tier no longer asking allows none, and one
	// that has started asking again since the request was made asks afresh.
	tiers, err := s.alloc.BoundRerunTiers(ctx, open)
	if err != nil {
		return false, err
	}
	for _, tier := range tiers {
		if asked, ok := sweep.asked[tier]; ok && asked.After(open.Created) {
			sweep.failures[w.run] = "a tier it covers stopped asking and asked again since it was made"

			return false, s.giveUpRerun(ctx, w, open, alloc.RerunExpired, sweep)
		}
	}
	allowed := int64(0)
	for i, tier := range tiers {
		if i == 0 || limits[tier] < allowed {
			allowed = limits[tier]
		}
	}

	used, err := s.alloc.RunReruns(ctx, w.run.repository, w.run.run)
	if err != nil {
		return false, err
	}
	if used >= allowed {
		sweep.failures[w.run] = "a tier it covers no longer allows it under retry.disrupted"

		return false, s.giveUpRerun(ctx, w, open, alloc.RerunLimit, sweep)
	}

	if err := s.alloc.RetryRerun(ctx, open); err != nil {
		return false, err
	}
	open.Tries++

	return false, s.sendRerun(ctx, w, open, owner, repository, sweep)
}

// provedJob is a candidate whose lost runner GitHub's record shows.
type provedJob struct {
	candidate *alloc.RerunCandidate
	id        int64
}

// allowedFor is the smallest retry.disrupted among the tiers of the jobs a request
// would run again: each is asked for only while the run has had fewer re-runs from
// billet than its own tier allows.
func allowedFor(proved []provedJob, limits map[string]int64) int64 {
	allowed := int64(-1)
	for i := range proved {
		if n := limits[proved[i].candidate.Tier]; allowed < 0 || n < allowed {
			allowed = n
		}
	}

	return max(allowed, 0)
}

// decideEachJob decides the jobs that can be decided on their own, and returns the
// ones GitHub's record proves. An error is a record nothing can be decided from.
func (s *Server) decideEachJob(ctx context.Context, w *rerunWork, jobs []alloc.RerunCandidate, attempt RunAttempt,
) ([]provedJob, error) {
	byRunner := make(map[string]*AttemptJob, len(attempt.Jobs))
	shared := map[string]bool{}
	for i := range attempt.Jobs {
		name := attempt.Jobs[i].RunnerName
		if _, seen := byRunner[name]; seen {
			shared[name] = true
		}
		byRunner[name] = &attempt.Jobs[i]
	}

	// EVERY REFUSAL TO DECIDE BEFORE ANY DECISION, so a run nothing can be decided from
	// is left whole rather than decided in part.
	for i := range jobs {
		name := provider.InstanceName(jobs[i].LeaseID)

		listed, found := byRunner[name]
		switch {
		case shared[name]:
			// A NAME TWO JOBS SHARE makes either a guess, which RunnerJob refuses too.
			return nil, fmt.Errorf("server: run %d lists two jobs on runner %q", w.run.run, name)
		case !found && attempt.Attempt == 1:
			// NOTHING CAN HAVE SUPERSEDED THE FIRST ATTEMPT, so a job missing from it is
			// could-not-tell, and the window is what ends it.
			return nil, fmt.Errorf("server: run %d's first attempt does not list runner %q", w.run.run, name)
		case found && listed.Attempt > attempt.Attempt:
			// THE TWO READS DISAGREE about which attempt is the latest.
			return nil, fmt.Errorf("server: run %d is on attempt %d and lists runner %q on attempt %d",
				w.run.run, attempt.Attempt, name, listed.Attempt)
		}
	}

	var proved []provedJob

	for i := range jobs {
		job := &jobs[i]

		listed, found := byRunner[provider.InstanceName(job.LeaseID)]
		switch {
		case !found, listed.Attempt < attempt.Attempt:
			// THE RUN MOVED PAST THE JOB'S ATTEMPT: a person re-ran it, or a request of
			// billet's own the job was not yet a candidate for when it was made.
			s.decideJob(ctx, job, alloc.RerunSuperseded, attempt.Attempt)
		case !listed.RunnerLost:
			s.decideJob(ctx, job, alloc.RerunUnproven, attempt.Attempt)
		default:
			proved = append(proved, provedJob{candidate: job, id: listed.ID})
		}
	}

	return proved, nil
}

// siblingsHeardFrom refuses to decide while a failed job of the run ran on a runner of
// this deployment's whose result billet has not heard: the completion may still be
// on its way, and the job may be one more that billet proves.
func (s *Server) siblingsHeardFrom(ctx context.Context, attempt RunAttempt) error {
	for i := range attempt.Jobs {
		job := &attempt.Jobs[i]
		if !job.Failed {
			continue
		}

		lease, billets := provider.LeaseOf(job.RunnerName)
		if !billets {
			continue
		}

		result, err := s.alloc.RecordedJobResult(ctx, lease)
		switch {
		case errors.Is(err, alloc.ErrLeaseNotFound):
			// ANOTHER DEPLOYMENT'S RUNNER, or one this ledger never assigned.
			continue
		case err != nil:
			return err
		case result == "":
			return errRunUnfinished
		}
	}

	return nil
}

// decideAll records one decision on every job.
func (s *Server) decideAll(ctx context.Context, jobs []provedJob, decision alloc.RerunDecision, attempt int64) {
	for i := range jobs {
		s.decideJob(ctx, jobs[i].candidate, decision, attempt)
	}
}

// exactRequest is the request that runs the proved jobs again and no job that
// failed on its own, false when neither of GitHub's two does.
//
// `rerun-failed-jobs` RE-RUNS EVERY FAILED JOB OF THE RUN, so it is exact only when
// every one of them is proved. Otherwise one proved job can be asked for alone, and
// several cannot: each request starts an attempt, and the next job's id is then
// another attempt's.
func exactRequest(run rerunRun, attempt RunAttempt, proved []provedJob) (alloc.RerunRequest, bool) {
	ours := make(map[int64]bool, len(proved))
	for i := range proved {
		ours[proved[i].id] = true
	}

	exact := true
	for i := range attempt.Jobs {
		if attempt.Jobs[i].Failed && !ours[attempt.Jobs[i].ID] {
			exact = false
		}
	}

	request := alloc.RerunRequest{Repository: run.repository, RunID: run.run, Attempt: attempt.Attempt, Tries: 1}

	switch {
	case exact:
		request.Kind = alloc.RerunFailedJobs
	case len(proved) == 1:
		request.Kind, request.JobID = alloc.RerunOneJob, proved[0].id
	default:
		return alloc.RerunRequest{}, false
	}

	return request, true
}

// sendRerun sends one request whose row says it may land, and settles it when
// GitHub accepts. Any other answer leaves it in doubt: the next pass reads the run
// before anything is sent or given up, because an answer lost on the way back is
// indistinguishable from a request never received.
func (s *Server) sendRerun(ctx context.Context, w *rerunWork, request alloc.RerunRequest, owner, repository string,
	sweep *rerunSweep,
) error {
	var err error

	switch request.Kind {
	case alloc.RerunOneJob:
		err = w.client.RerunJob(ctx, owner, repository, request.JobID)
	case alloc.RerunFailedJobs:
		err = w.client.RerunFailedJobs(ctx, owner, repository, w.run.run)
	default:
		// A KIND A NEWER BINARY WROTE. Nothing is sent, and the tries run out.
		err = fmt.Errorf("server: a re-run of kind %q is not one this billet sends", request.Kind)
	}

	switch {
	case err == nil:
		delete(sweep.failures, w.run)
		s.log.Info("asked GitHub to re-run a job billet disrupted", "repository", w.run.repository,
			"run", w.run.run, "attempt", request.Attempt, "kind", request.Kind, "try", request.Tries)

		return s.alloc.SettleRerun(ctx, request, alloc.RerunRequested, "")
	case errors.Is(err, ErrRerunRunning):
		// THE RUN WAS COMPLETED WHEN IT WAS READ, so something started it since. The
		// row stays in doubt and the next pass finds it going, and settles it as
		// accepted: billet cannot tell its own earlier request from a person's, and
		// counting it only makes billet ask less.
		return errRunUnfinished
	case ctx.Err() != nil:
		return ctx.Err()
	}

	sweep.failures[w.run] = rerunDetail(err)

	return err
}

// giveUpRerun settles a request that will not be sent again, and reports it.
func (s *Server) giveUpRerun(ctx context.Context, w *rerunWork, request alloc.RerunRequest,
	decision alloc.RerunDecision, sweep *rerunSweep,
) error {
	detail := sweep.failures[w.run]
	if detail == "" {
		detail = "a request in doubt across a restart used its tries"
	}

	s.log.Warn("billet will not ask GitHub again for this re-run; the job stays red, and a person can re-run it",
		"repository", w.run.repository, "run", w.run.run, "attempt", request.Attempt, "tries", request.Tries,
		"decision", decision, "detail", detail)

	if err := s.alloc.SettleRerun(ctx, request, decision, detail); err != nil {
		return err
	}
	delete(sweep.failures, w.run)

	return nil
}

// rerunDetail is what a failed request records: its error, bounded and valid
// UTF-8, which PostgreSQL's text requires. It is billet's own text around GitHub's
// decoded message, with the App's token already removed.
func rerunDetail(err error) string {
	text := err.Error()
	if len(text) > rerunDetailBytes {
		text = text[:rerunDetailBytes]
	}

	return strings.ToValidUTF8(text, "")
}
