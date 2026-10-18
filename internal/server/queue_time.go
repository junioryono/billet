package server

import (
	"time"

	"github.com/junioryono/billet/internal/dispatch"
)

const (
	// queueTimeRetention is how long an offered job's queue time is kept for its
	// start. GitHub cancels a job that has waited 24 hours for a runner, so an
	// entry older than that names a job that can no longer start.
	queueTimeRetention = 25 * time.Hour

	// queueTimeLimit bounds the offers one listener keeps. Past it a new offer's
	// time is not kept and that job's wait is unknown, never another job's.
	queueTimeLimit = 50_000
)

// offeredJob is the job an offer named: GitHub's job id and its run, the two
// aliases sameActualJob refuses to let contradict each other.
type offeredJob struct {
	job string
	run int64
}

// offeredQueueTime is GitHub's queue time for one job and when billet saw it.
type offeredQueueTime struct {
	queued time.Time
	seen   time.Time
}

// rememberQueueTimes keeps GitHub's queue time from the jobs an offer and an
// assignment name, for the job's start to record, and says how many of each
// carried one.
//
// THE START DOES NOT CARRY IT. Every JobStarted and JobCompleted the reference
// fleet received on 2026-10-18 under v0.17.0 had queueTime 0001-01-01 while both
// assignment times were set. Whether the offer and the assignment carry it is
// NOT yet measured; the counts this returns are logged so the fleet can answer.
// The newest time wins: a redelivery carries the same one, and a re-run attempt
// of the same job is queued again, later.
//
// Held in memory only, so a restart between a job's offer and its start leaves
// that job's wait unknown. Entries past the retention are dropped at most once
// an hour, as messages arrive.
func (l *Listener) rememberQueueTimes(available, assigned []dispatch.Job) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if now.Sub(l.queuedPruned) >= time.Hour {
		for id, q := range l.queued {
			if now.Sub(q.seen) > queueTimeRetention {
				delete(l.queued, id)
			}
		}
		l.queuedPruned = now
	}
	remember := func(jobs []dispatch.Job) int {
		timed := 0
		for i := range jobs {
			job := &jobs[i]
			if job.QueuedAt.IsZero() {
				continue
			}
			timed++
			if job.JobID == "" || job.RunID == 0 {
				continue
			}
			id := offeredJob{job: job.JobID, run: job.RunID}
			prior, ok := l.queued[id]
			switch {
			case ok && prior.queued.After(job.QueuedAt):
				prior.seen = now
				l.queued[id] = prior
			case ok || len(l.queued) < queueTimeLimit:
				l.queued[id] = offeredQueueTime{queued: job.QueuedAt, seen: now}
			}
		}
		return timed
	}
	return remember(available), remember(assigned)
}

// withOfferedQueueTime is a started job with the queue time its offer carried
// when the JobStarted itself carried none.
//
// ONLY A START TAKES IT, with the job id and run the message itself named: the
// start is the runner-to-job binding, while a completion's run may be filled
// from the lease's launch, which can be another run. A start must carry its own
// hand-over to the scale set, and a time after it belongs to a later attempt
// and is not taken. That still assumes a job id and run name one attempt, as
// job_identity.go measured (no job id started twice); an untimed message cannot
// clear an entry, because it may be the same attempt's other kind. And it is
// never consumed, because a write the ledger's write-once filter
// skips still returns nil: the entry stays for a redelivered start and expires
// with the rest.
func (l *Listener) withOfferedQueueTime(job dispatch.Job) dispatch.Job {
	if !job.QueuedAt.IsZero() {
		return job
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	q, ok := l.queued[offeredJob{job: job.JobID, run: job.RunID}]
	if ok && !job.ScaleSetAssignedAt.IsZero() && !q.queued.After(job.ScaleSetAssignedAt) {
		job.QueuedAt = q.queued
	}
	return job
}
