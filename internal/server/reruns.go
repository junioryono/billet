package server

import (
	"context"
	"errors"
	"fmt"
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
	// rerunSettle is how long after a job's result its run waits. GitHub concludes
	// a run's lost jobs together and their completions arrive one by one, so
	// deciding on the first would see the second as a job that failed on its own.
	rerunSettle = 2 * time.Minute
	// rerunWindow is how long a job can wait for its run to finish, or for GitHub to
	// answer about it, before it is decided as expired. A re-run after that is an
	// event nobody is waiting for.
	rerunWindow = 24 * time.Hour
	// rerunListed bounds the jobs one pass reads. Disruption is rare, so a backlog
	// this long is a fleet with a larger problem, and it loses its oldest.
	rerunListed = 1000
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

// rerunRun is one run of one target's repository.
type rerunRun struct {
	target     string
	repository string
	run        int64
}

// rerunSweep is what the loop remembers between passes: each run's backoff, each
// installation paused by a throttle or a refusal, and the last failure of a request
// still in doubt, for the row it settles.
type rerunSweep struct {
	retries  map[rerunRun]queueTimeRetry
	paused   map[string]time.Time
	reserved map[string]bool
	failures map[rerunRun]string
}

func newRerunSweep() *rerunSweep {
	return &rerunSweep{retries: map[rerunRun]queueTimeRetry{}, paused: map[string]time.Time{},
		reserved: map[string]bool{}, failures: map[rerunRun]string{}}
}

// rerunWork is one run a pass will read, and through what. allowed is the smallest
// retry.disrupted among its jobs' tiers: one request covers them all.
type rerunWork struct {
	run          rerunRun
	installation string
	client       RunReruns
	jobs         []alloc.RerunCandidate
	allowed      int64
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
	for run, retry := range sweep.retries {
		if now.Sub(retry.after) > rerunWindow {
			delete(sweep.retries, run)
			delete(sweep.failures, run)
		}
	}

	read := map[string]int{}
	budgets := installationBudgets(targets)

	work := s.gatherRerunWork(ctx, targets, limits, sweep, now)
	for i := range work {
		w := &work[i]
		if ctx.Err() != nil {
			return
		}
		if read[w.installation] == rerunRunsPerInstallation || now.Before(sweep.paused[w.installation]) {
			continue
		}
		if remaining, limit, reset, low := inReserve(budgets[w.installation], now); low {
			if !sweep.reserved[w.installation] {
				sweep.reserved[w.installation] = true
				s.log.Warn("this App installation's REST budget is down to its reserve; deciding re-runs "+
					"again when GitHub's next hour begins", "target", w.run.target, "installation", w.installation,
					"remaining", remaining, "limit", limit, "reset", reset)
			}
			continue
		}
		delete(sweep.reserved, w.installation)
		read[w.installation]++

		s.afterRerun(w, s.decideRerun(ctx, w, sweep), sweep, now)
	}
}

// afterRerun records what a run's turn asks of the next pass.
func (s *Server) afterRerun(w *rerunWork, err error, sweep *rerunSweep, now time.Time) {
	switch {
	case err == nil:
		delete(sweep.retries, w.run)
		return
	case errors.Is(err, errRunUnfinished):
		sweep.retries[w.run] = queueTimeRetry{after: now.Add(rerunUnfinishedEvery)}
		return
	case errors.Is(err, ErrRerunThrottled):
		sweep.paused[w.installation] = now.Add(rerunThrottled)
		s.log.Warn("GitHub is rate-limiting this App installation; deciding re-runs again later",
			"target", w.run.target, "installation", w.installation, "after", rerunThrottled, "error", err)
	case errors.Is(err, ErrRerunRefused):
		sweep.paused[w.installation] = now.Add(rerunRefused)
		s.log.Warn("GitHub refused this App installation a re-run; a tier with retry.disrupted needs the App "+
			"to hold `actions: write`, and a revoked key or a suspended App is refused the same way",
			"target", w.run.target, "installation", w.installation, "after", rerunRefused, "error", err)
	default:
		s.log.Warn("could not decide a re-run; trying again later",
			"target", w.run.target, "repository", w.run.repository, "run", w.run.run, "error", err)
	}

	retry := sweep.retries[w.run]
	retry.wait = min(max(2*retry.wait, rerunRetryFirst), rerunRetryMost)
	retry.after = now.Add(retry.wait)
	sweep.retries[w.run] = retry
}

// gatherRerunWork groups the jobs awaiting a decision by run, newest first, and
// decides as expired what has waited too long to be worth a request.
func (s *Server) gatherRerunWork(ctx context.Context, targets map[string]Target, limits map[string]int64,
	sweep *rerunSweep, now time.Time,
) []rerunWork {
	// TWICE THE WINDOW, so a job past it is still listed and is decided as expired
	// rather than left undecided for ever.
	jobs, err := s.alloc.RerunCandidates(ctx, now.Add(-2*rerunWindow), rerunListed)
	if err != nil {
		s.log.Warn("could not list the jobs awaiting a re-run decision; trying again", "error", err)

		return nil
	}

	settle := rerunSettle
	if s.rerunSettle > 0 {
		settle = s.rerunSettle
	}

	index := map[rerunRun]int{}
	settling := map[rerunRun]bool{}

	var work []rerunWork

	for i := range jobs {
		job := &jobs[i]

		allowed, opted := limits[job.Tier]
		target := targets[job.Tier]
		client, reaches := target.Provisioner.(RunReruns)
		if !opted || !reaches {
			continue
		}

		if now.Sub(job.ResultAt) > rerunWindow {
			s.decideJob(ctx, job, alloc.RerunExpired, 0)

			continue
		}

		run := rerunRun{target: target.Config.Name, repository: job.Repository, run: job.RunID}
		if now.Sub(job.ResultAt) < settle {
			settling[run] = true
		}
		if retry, ok := sweep.retries[run]; ok && now.Before(retry.after) {
			continue
		}

		at, seen := index[run]
		if !seen {
			at = len(work)
			index[run] = at
			work = append(work, rerunWork{run: run, installation: installationOf(target), client: client, allowed: allowed})
		}
		work[at].jobs = append(work[at].jobs, *job)
		work[at].allowed = min(work[at].allowed, allowed)
	}

	// WHOLE RUNS, not jobs: a run with a result still settling is decided whole,
	// on a later pass.
	ready := work[:0]
	for i := range work {
		if !settling[work[i].run] {
			ready = append(ready, work[i])
		}
	}

	return ready
}

// decideJob records a decision on one job, and logs a write that failed: the job
// stays undecided and the next pass decides it again.
func (s *Server) decideJob(ctx context.Context, job *alloc.RerunCandidate, decision alloc.RerunDecision, attempt int64) {
	if err := s.alloc.RecordRerunDecision(ctx, job.LeaseID, decision, attempt); err != nil {
		s.log.Warn("could not record a re-run decision; trying again",
			"tier", job.Tier, "lease", job.LeaseID, "decision", decision, "error", err)

		return
	}

	s.log.Info("decided a disrupted job's re-run", "tier", job.Tier, "lease", job.LeaseID,
		"repository", job.Repository, "run", job.RunID, "disruption", job.Disruption, "decision", decision)
}

// decideRerun takes one run as far as it can go on this pass. ADR-023 holds the
// rule; the order below is what keeps it, and each step's refusal is the decision.
func (s *Server) decideRerun(ctx context.Context, w *rerunWork, sweep *rerunSweep) error {
	owner, repository, ok := config.SplitRepository(w.run.repository)
	if !ok {
		return fmt.Errorf("server: %q is not a repository", w.run.repository)
	}

	attempt, err := w.client.LatestAttempt(ctx, owner, repository, w.run.run)
	if err != nil {
		return err
	}

	// A REQUEST IN DOUBT FIRST, answered by GitHub's record of the run rather than
	// by asking again. Measured 2026-11-04: straight after a 201 the run is
	// `queued` and still on its attempt, so the status says it landed before the
	// attempt does. A completed run still on the attempt asked about never heard it.
	open, doubted, err := s.alloc.OpenRerun(ctx, w.run.repository, w.run.run)
	if err != nil {
		return err
	}
	if doubted {
		switch {
		case !attempt.Completed || attempt.Attempt > open.Attempt:
			return s.alloc.SettleRerun(ctx, open, true, "")
		case attempt.Attempt < open.Attempt:
			return fmt.Errorf("server: run %d is on attempt %d, before the attempt %d billet asked about",
				w.run.run, attempt.Attempt, open.Attempt)
		case open.Tries >= rerunMaxTries:
			return s.giveUpRerun(ctx, w, open, sweep)
		}
		if err := s.alloc.RetryRerun(ctx, open); err != nil {
			return err
		}
		open.Tries++

		return s.sendRerun(ctx, w, open, owner, repository, sweep)
	}

	if !attempt.Completed {
		return errRunUnfinished
	}

	proved, err := s.decideEachJob(ctx, w, attempt)
	if err != nil || len(proved) == 0 {
		return err
	}

	used, err := s.alloc.RunReruns(ctx, w.run.repository, w.run.run)
	if err != nil {
		return err
	}
	if used >= w.allowed {
		s.decideAll(ctx, proved, alloc.RerunLimit, attempt.Attempt)

		return nil
	}

	// A JOB WHOSE RESULT CAME LATE, after its attempt was asked about. The key allows
	// the attempt no second request, and the one it had is this job's answer too.
	settled, asked, err := s.alloc.RerunOfAttempt(ctx, w.run.repository, w.run.run, attempt.Attempt)
	if err != nil {
		return err
	}
	if asked {
		s.decideAll(ctx, proved, settled, attempt.Attempt)

		return nil
	}

	request, ok := exactRequest(w.run, attempt, proved)
	if !ok {
		s.decideAll(ctx, proved, alloc.RerunMixed, attempt.Attempt)

		return nil
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

// provedJob is a candidate whose lost runner GitHub's record shows.
type provedJob struct {
	candidate *alloc.RerunCandidate
	id        int64
}

// decideEachJob decides the jobs that can be decided on their own, and returns
// the ones GitHub's record proves.
func (s *Server) decideEachJob(ctx context.Context, w *rerunWork, attempt RunAttempt) ([]provedJob, error) {
	byRunner := make(map[string]*AttemptJob, len(attempt.Jobs))
	for i := range attempt.Jobs {
		byRunner[attempt.Jobs[i].RunnerName] = &attempt.Jobs[i]
	}

	var proved []provedJob

	for i := range w.jobs {
		job := &w.jobs[i]

		listed, found := byRunner[provider.InstanceName(job.LeaseID)]
		switch {
		case !found:
			s.decideJob(ctx, job, alloc.RerunSuperseded, attempt.Attempt)
		case !listed.RunnerLost:
			s.decideJob(ctx, job, alloc.RerunUnproven, attempt.Attempt)
		default:
			proved = append(proved, provedJob{candidate: job, id: listed.ID})
		}
	}

	return proved, nil
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

// sendRerun sends one request whose row says it may land, and settles what its
// answer settles.
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

		return s.alloc.SettleRerun(ctx, request, true, "")
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
	if request.Tries >= rerunMaxTries {
		return s.giveUpRerun(ctx, w, request, sweep)
	}

	return err
}

// giveUpRerun settles a request that used its tries, and reports it.
func (s *Server) giveUpRerun(ctx context.Context, w *rerunWork, request alloc.RerunRequest, sweep *rerunSweep) error {
	detail := sweep.failures[w.run]
	if detail == "" {
		detail = "a request in doubt across a restart used its tries"
	}

	s.log.Warn("GitHub did not accept a re-run in its tries; the job stays red, and a person can re-run it",
		"repository", w.run.repository, "run", w.run.run, "attempt", request.Attempt, "tries", request.Tries,
		"detail", detail)

	if err := s.alloc.SettleRerun(ctx, request, false, detail); err != nil {
		return err
	}
	delete(sweep.failures, w.run)

	return nil
}

// rerunDetail is what a failed request records: its error, bounded. It is billet's
// own text around GitHub's decoded message, with the App's token already removed.
func rerunDetail(err error) string {
	text := err.Error()
	if len(text) > rerunDetailBytes {
		return text[:rerunDetailBytes]
	}

	return text
}
