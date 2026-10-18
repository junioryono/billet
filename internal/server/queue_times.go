package server

import (
	"context"
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

const (
	// queueTimeEvery is how often the sweep looks for started jobs with no queue
	// time.
	queueTimeEvery = time.Minute
	// queueTimeWindow is how far back it looks: a job older than this keeps an
	// unknown wait rather than costing a request on every pass.
	queueTimeWindow = 24 * time.Hour
	// queueTimeRows bounds one pass's ledger read, and queueTimeRuns how many
	// runs it reads from GitHub, each a request per 25 of its jobs.
	queueTimeRows = 1000
	queueTimeRuns = 30
	// queueTimeRetryFirst is the wait after a run's first read that left a job
	// unfound or failed, doubled after each until queueTimeRetryMost.
	queueTimeRetryFirst = time.Minute
	queueTimeRetryMost  = time.Hour
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

// fillQueueTimes records GitHub's queue time for the jobs this deployment
// started, from each job's REST record, until ctx ends. It decides nothing:
// a pass that fails is logged and the next one tries again.
func (s *Server) fillQueueTimes(ctx context.Context, targets map[string]Target) {
	retries := map[queueTimeRun]queueTimeRetry{}
	tick := time.NewTicker(queueTimeEvery)
	defer tick.Stop()

	for {
		s.fillQueueTimesOnce(ctx, targets, retries, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// fillQueueTimesOnce is one pass at now. retries carries each run's backoff
// between passes.
//
// ONE READ OF A RUN SERVES EVERY JOB OF IT, so a run of forty jobs costs two
// requests rather than eighty. A job is matched by its runner's name, which is
// its lease's and unique by construction, across every attempt of the run.
// The write is the identity write, which fills only an empty queue time and
// only on the row of the job it names, and tells the job's wait.
func (s *Server) fillQueueTimesOnce(ctx context.Context, targets map[string]Target,
	retries map[queueTimeRun]queueTimeRetry, now time.Time,
) {
	for run, retry := range retries {
		if now.Sub(retry.after) > queueTimeWindow {
			delete(retries, run)
		}
	}

	jobs, err := s.alloc.JobsAwaitingQueueTime(ctx, now.Add(-queueTimeWindow), queueTimeRows)
	if err != nil {
		s.log.Warn("could not list the jobs awaiting GitHub's queue time; trying again", "error", err)
		return
	}

	byRun := map[queueTimeRun][]alloc.AwaitingQueueTime{}
	var order []queueTimeRun
	readers := map[string]QueueTimes{}
	for i := range jobs {
		job := &jobs[i]
		target := targets[job.Tier]
		reader, ok := target.Provisioner.(QueueTimes)
		if !ok {
			continue
		}
		run := queueTimeRun{target: target.Config.Name, repository: job.Repository, run: job.RunID}
		if retry, ok := retries[run]; ok && now.Before(retry.after) {
			continue
		}
		if _, seen := byRun[run]; !seen {
			order = append(order, run)
		}
		byRun[run] = append(byRun[run], *job)
		readers[run.target] = reader
	}

	for i, run := range order {
		if i == queueTimeRuns || ctx.Err() != nil {
			return
		}
		missing := s.fillRunQueueTimes(ctx, readers[run.target], run, byRun[run])
		if missing == 0 {
			delete(retries, run)
			continue
		}
		retry := retries[run]
		retry.wait = min(max(2*retry.wait, queueTimeRetryFirst), queueTimeRetryMost)
		retry.after = now.Add(retry.wait)
		retries[run] = retry
	}
}

// fillRunQueueTimes reads one run and records each job's queue time, and says
// how many of jobs it could not.
func (s *Server) fillRunQueueTimes(ctx context.Context, reader QueueTimes, run queueTimeRun,
	jobs []alloc.AwaitingQueueTime,
) int {
	owner, repository, ok := config.SplitRepository(run.repository)
	if !ok {
		return len(jobs)
	}
	times, err := reader.RunQueueTimes(ctx, owner, repository, run.run)
	if err != nil {
		s.log.Warn("could not read GitHub's queue times for a run; trying again later",
			"target", run.target, "repository", run.repository, "run", run.run, "error", err)
		return len(jobs)
	}

	missing := 0
	for i := range jobs {
		job := &jobs[i]
		queued, ok := times[provider.InstanceName(job.LeaseID)]
		if !ok {
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

	return missing
}
