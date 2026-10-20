package server

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// QueueTimes reads when GitHub queued each job of a run, keyed by the name of
// the runner that ran it. A target's Provisioner implements it over the App's
// REST client.
//
// THE SCALE-SET PROTOCOL DOES NOT CARRY IT. Every JobStarted, JobAssigned and
// JobCompleted the reference fleet received on 2026-10-18 under v0.17.0 and
// v0.18.0 had queueTime 0001-01-01 while both assignment times were set, and no
// JobAvailable arrived at all. GitHub's REST record of the job does: its
// created_at, which fell a second or less before the start's scaleSetAssignTime.
type QueueTimes interface {
	RunQueueTimes(ctx context.Context, owner, repository string, runID int64) (map[string]time.Time, error)
}

// RESTHeadroom is an App installation's REST budget as GitHub last stated it:
// what is left of the hour's limit and when the next hour begins, false before
// any answer stated one. A target's Provisioner implements it.
type RESTHeadroom interface {
	RESTHeadroom() (remaining, limit int64, reset time.Time, ok bool)
}

var (
	// ErrQueueTimesThrottled is a QueueTimes read GitHub's rate limit refused.
	ErrQueueTimesThrottled = errors.New("server: GitHub is rate-limiting this App installation")
	// ErrQueueTimesRefused is a QueueTimes read GitHub refused the App with a
	// 401 or 403: an App without `actions: read`, a revoked key or a suspended
	// App.
	ErrQueueTimesRefused = errors.New("server: GitHub refused this App installation the job records")
)

const (
	// queueTimeEvery is how often the sweep looks for started jobs with no queue
	// time.
	queueTimeEvery = time.Minute
	// queueTimeWindow is how far back it looks: a job older than this keeps an
	// unknown wait rather than costing a request on every pass.
	queueTimeWindow = 24 * time.Hour
	// One pass reads the ledger a page of queueTimePageRows at a time, at most
	// queueTimePages pages, and the next pass resumes where it stopped, so jobs
	// it cannot read yet (backed off, paused, an unserved tier) never hide the
	// ones it can.
	queueTimePageRows = 2_000
	queueTimePages    = 10
	// queueTimeRunsPerInstallation is how many runs one pass reads through one
	// App installation, whose hourly budget the scale-set, runner-group and
	// cache-authority calls share. A run is a page per 25 of its jobs and the
	// GitHub client reads no run of more than 200, so a pass costs an
	// installation at most 24 pages, 1,440 an hour, each a request and one more
	// for each redirect it is answered with, against a budget of at least 5,000. It also caps what the sweep keeps up with: 180 runs an hour an
	// installation, past which the oldest runs reach the window unread.
	queueTimeRunsPerInstallation = 3
	// queueTimeRetryFirst is the wait after a run's read that left a job unfound
	// or failed, doubled after each until queueTimeRetryMost.
	queueTimeRetryFirst = time.Minute
	queueTimeRetryMost  = time.Hour
	// An installation GitHub throttles waits queueTimeThrottled before its next
	// read, and one it refuses waits queueTimeRefused.
	queueTimeThrottled = 15 * time.Minute
	queueTimeRefused   = time.Hour
	// The sweep starts no run's read through an installation whose budget, as
	// any of its targets' clients last saw it, has less than one
	// queueTimeReserve-th of its limit left before the next hour begins: the rest
	// is the runner-group checks' and the cache authority's, which decide whether
	// a runner starts and what a job may write, where a missing queue time
	// decides nothing. A read already started finishes, at most eight pages and
	// their redirects against a reserve of at least a thousand requests.
	queueTimeReserve = 5
)

// queueTimeRun is one run of one target's repository.
type queueTimeRun struct {
	target     string
	repository string
	run        int64
}

// queueTimeRetry is when a run may be read again, and the wait after that.
type queueTimeRetry struct {
	after time.Time
	wait  time.Duration
}

// queueTimeSweep is what the sweep remembers between passes: each run's
// backoff, each installation paused by a throttle or a refusal, and where in
// the ledger the next pass begins.
type queueTimeSweep struct {
	retries map[queueTimeRun]queueTimeRetry
	paused  map[string]time.Time
	// reserved is each installation the sweep is leaving its reserve to, so the
	// hold is logged once rather than on every pass.
	reserved map[string]bool
	cursor   alloc.QueueTimeCursor
	// pageRows and pages are queueTimePageRows and queueTimePages, smaller in a
	// test.
	pageRows, pages int
}

func newQueueTimeSweep() *queueTimeSweep {
	return &queueTimeSweep{retries: map[queueTimeRun]queueTimeRetry{}, paused: map[string]time.Time{},
		reserved: map[string]bool{}, pageRows: queueTimePageRows, pages: queueTimePages}
}

// installationOf is what a target's REST requests are counted against: its App
// installation, which several repository targets may share, or the target
// itself where the configuration names none.
func installationOf(target Target) string {
	if target.Config.InstallationID > 0 {
		return "installation " + strconv.FormatInt(target.Config.InstallationID, 10)
	}

	return "target " + target.Config.Name
}

// fillQueueTimes records GitHub's queue time for the jobs this deployment
// started, from each job's REST record, until ctx ends. It decides nothing:
// a pass that fails is logged and the next one tries again.
func (s *Server) fillQueueTimes(ctx context.Context, targets map[string]Target) {
	sweep := newQueueTimeSweep()
	tick := time.NewTicker(queueTimeEvery)
	defer tick.Stop()

	for {
		s.fillQueueTimesOnce(ctx, targets, sweep, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// queueTimeWork is one run a pass will read, and through what.
type queueTimeWork struct {
	run          queueTimeRun
	installation string
	reader       QueueTimes
	jobs         []alloc.AwaitingQueueTime
}

// installationBudgets is every target's budget by the installation it counts
// against, whether or not the target has jobs awaiting a queue time: a target
// with none may still have seen the installation's budget spent.
func installationBudgets(targets map[string]Target) map[string][]RESTHeadroom {
	budgets := map[string][]RESTHeadroom{}
	for _, target := range targets {
		if h, ok := target.Provisioner.(RESTHeadroom); ok {
			budgets[installationOf(target)] = append(budgets[installationOf(target)], h)
		}
	}

	return budgets
}

// inReserve says whether an installation's budget is down to its reserve for
// the rest of the hour, as any of its targets' clients last saw it: each keeps
// the statement its own requests drew, and within an hour the lowest of them is
// the newest. A budget no answer has stated yet, or one whose hour has ended,
// holds nothing back: the read's own throttle still pauses the installation.
func inReserve(budgets []RESTHeadroom, now time.Time) (int64, int64, time.Time, bool) {
	for _, h := range budgets {
		remaining, limit, reset, ok := h.RESTHeadroom()
		if ok && now.Before(reset) && remaining*queueTimeReserve < limit {
			return remaining, limit, reset, true
		}
	}

	return 0, 0, time.Time{}, false
}

// fillQueueTimesOnce is one pass at now.
//
// ONE READ OF A RUN SERVES EVERY JOB OF IT, so a run of forty jobs costs two
// requests rather than eighty. A job is matched by its runner's name, which is
// its lease's and unique by construction, across every attempt of the run.
// The write is the identity write, which fills only an empty queue time and
// only on the row of the job it names, and tells the job's wait.
func (s *Server) fillQueueTimesOnce(ctx context.Context, targets map[string]Target, sweep *queueTimeSweep,
	now time.Time,
) {
	for run, retry := range sweep.retries {
		if now.Sub(retry.after) > queueTimeWindow {
			delete(sweep.retries, run)
		}
	}

	work, ok := s.gatherQueueTimeWork(ctx, targets, sweep, now)
	if !ok {
		return
	}
	s.readQueueTimeWork(ctx, work, installationBudgets(targets), sweep, now)
}

// readQueueTimeWork reads work in order within each installation's share of
// the pass and its reserve, and records each run's backoff and each
// installation's pause.
func (s *Server) readQueueTimeWork(ctx context.Context, work []queueTimeWork, budgets map[string][]RESTHeadroom,
	sweep *queueTimeSweep, now time.Time,
) {
	read := map[string]int{}
	for _, w := range work {
		if ctx.Err() != nil {
			return
		}
		if read[w.installation] == queueTimeRunsPerInstallation || now.Before(sweep.paused[w.installation]) {
			continue
		}
		// BEFORE EVERY RUN, since the run read before it may have spent the
		// installation down to its reserve.
		if remaining, limit, reset, low := inReserve(budgets[w.installation], now); low {
			if !sweep.reserved[w.installation] {
				sweep.reserved[w.installation] = true
				s.log.Warn("this App installation's REST budget is down to its reserve; reading queue times "+
					"again when GitHub's next hour begins", "target", w.run.target, "installation", w.installation,
					"remaining", remaining, "limit", limit, "reset", reset)
			}
			continue
		}
		delete(sweep.reserved, w.installation)
		read[w.installation]++
		missing, err := s.fillRunQueueTimes(ctx, w.reader, w.run, w.jobs)
		switch {
		case errors.Is(err, ErrQueueTimesThrottled):
			sweep.paused[w.installation] = now.Add(queueTimeThrottled)
			s.log.Warn("GitHub is rate-limiting this App installation; reading queue times again later",
				"target", w.run.target, "installation", w.installation, "after", queueTimeThrottled, "error", err)
			continue
		case errors.Is(err, ErrQueueTimesRefused):
			sweep.paused[w.installation] = now.Add(queueTimeRefused)
			s.log.Warn("GitHub refused this App installation the job records; the App needs "+
				"`actions: read`, and a revoked key or a suspended App is refused the same way; "+
				"jobs keep an unknown queue time until it can read them",
				"target", w.run.target, "installation", w.installation, "after", queueTimeRefused, "error", err)
			continue
		case err != nil:
			s.log.Warn("could not read GitHub's queue times for a run; trying again later",
				"target", w.run.target, "repository", w.run.repository, "run", w.run.run, "error", err)
		}
		if missing == 0 {
			delete(sweep.retries, w.run)
			continue
		}
		retry := sweep.retries[w.run]
		retry.wait = min(max(2*retry.wait, queueTimeRetryFirst), queueTimeRetryMost)
		retry.after = now.Add(retry.wait)
		sweep.retries[w.run] = retry
	}
}

// gatherQueueTimeWork groups the jobs awaiting a queue time that may be read
// now by run, newest first. False is a ledger read that failed.
//
// THE NEWEST PAGE IS READ ON EVERY PASS, and the rest of the pass's pages
// continue from the sweep's cursor, which moves on to where they stopped and
// back to the newest once they reach the oldest. A backlog of jobs that cannot
// be read yet therefore never hides older ones that can, and never makes a job
// that just started wait for the backlog to be paged through.
func (s *Server) gatherQueueTimeWork(ctx context.Context, targets map[string]Target, sweep *queueTimeSweep,
	now time.Time,
) ([]queueTimeWork, bool) {
	// A JOB BOTH SCANS READ IS GATHERED ONCE: a second copy's write could time
	// out after the first landed and back off a run whose time is recorded.
	index := map[queueTimeRun]int{}
	gathered := map[string]bool{}
	var work []queueTimeWork

	// scan reads up to pages pages from from, and says where it stopped and
	// whether it reached the oldest job.
	scan := func(from alloc.QueueTimeCursor, pages int) (alloc.QueueTimeCursor, bool, bool) {
		cursor := from
		for range pages {
			jobs, err := s.alloc.JobsAwaitingQueueTime(ctx, now.Add(-queueTimeWindow), cursor, sweep.pageRows)
			if err != nil {
				s.log.Warn("could not list the jobs awaiting GitHub's queue time; trying again", "error", err)
				return cursor, false, false
			}
			for i := range jobs {
				job := &jobs[i]
				target := targets[job.Tier]
				reader, ok := target.Provisioner.(QueueTimes)
				if !ok || gathered[job.LeaseID] {
					continue
				}
				run := queueTimeRun{target: target.Config.Name, repository: job.Repository, run: job.RunID}
				if retry, ok := sweep.retries[run]; ok && now.Before(retry.after) {
					continue
				}
				gathered[job.LeaseID] = true
				at, seen := index[run]
				if !seen {
					at = len(work)
					index[run] = at
					work = append(work, queueTimeWork{run: run, installation: installationOf(target), reader: reader})
				}
				work[at].jobs = append(work[at].jobs, *job)
			}
			if len(jobs) < sweep.pageRows {
				return alloc.QueueTimeCursor{}, true, true
			}
			cursor = jobs[len(jobs)-1].Cursor
		}
		return cursor, false, true
	}

	pages := sweep.pages
	if sweep.cursor != (alloc.QueueTimeCursor{}) {
		if _, _, ok := scan(alloc.QueueTimeCursor{}, 1); !ok {
			return nil, false
		}
		pages--
	}
	cursor, oldest, ok := scan(sweep.cursor, pages)
	if !ok {
		return nil, false
	}
	if oldest {
		cursor = alloc.QueueTimeCursor{}
	}
	sweep.cursor = cursor

	return work, true
}

// fillRunQueueTimes reads one run and records each job's queue time, and says
// how many of jobs it could not; an error is the read's.
func (s *Server) fillRunQueueTimes(ctx context.Context, reader QueueTimes, run queueTimeRun,
	jobs []alloc.AwaitingQueueTime,
) (int, error) {
	owner, repository, ok := config.SplitRepository(run.repository)
	if !ok {
		return len(jobs), nil
	}
	times, err := reader.RunQueueTimes(ctx, owner, repository, run.run)
	if err != nil {
		return len(jobs), err
	}

	missing := 0
	for i := range jobs {
		job := &jobs[i]
		queued, ok := times[provider.InstanceName(job.LeaseID)]
		if !ok {
			missing++
			continue
		}
		// A JOB IS QUEUED BEFORE GITHUB HANDS IT TO THE SCALE SET, both on GitHub's
		// clock; a record that says otherwise is not this attempt's, and the wait
		// stays unknown rather than wrong. It is counted missing, so the run backs
		// off rather than being read on every pass.
		if !job.ScaleSetAssignedAt.IsZero() && queued.After(job.ScaleSetAssignedAt) {
			missing++
			continue
		}
		write, cancel := context.WithTimeout(ctx, identityWriteLimit)
		err := s.alloc.RecordJobIdentity(write, job.LeaseID, alloc.HistoryJob{JobID: job.JobID, QueuedAt: queued})
		cancel()
		if err != nil {
			s.log.Warn("could not record GitHub's queue time for a job; trying again",
				"tier", job.Tier, "lease", job.LeaseID, "error", err)
			missing++
		}
	}

	return missing, nil
}
