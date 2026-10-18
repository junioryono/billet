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

// offeredJob is the job an offer named: GitHub's job id AND its run, because a
// job id alone has been seen in two runs (job_identity.go's rule).
type offeredJob struct {
	job string
	run int64
}

// offeredQueueTime is GitHub's queue time for one job and when billet saw it.
type offeredQueueTime struct {
	queued time.Time
	seen   time.Time
}

// rememberQueueTimes keeps GitHub's queue time from the jobs an offer or an
// assignment names until the job's start records it.
//
// THE START DOES NOT CARRY IT. Every JobStarted and JobCompleted the reference
// fleet received on 2026-10-18 under v0.17.0 had queueTime 0001-01-01 while both
// assignment times were set, and actions-runner-controller reads it from
// JobAvailable for the same reason (its #4507). The first time seen wins: a
// redelivered offer names the same job and the same time.
//
// Held in memory only, so a restart between a job's offer and its start leaves
// that job's wait unknown. Entries past the retention are dropped at most once
// an hour, as messages arrive.
func (l *Listener) rememberQueueTimes(batches ...[]dispatch.Job) {
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
	for _, jobs := range batches {
		for i := range jobs {
			job := &jobs[i]
			if job.JobID == "" || job.RunID == 0 || job.QueuedAt.IsZero() {
				continue
			}
			id := offeredJob{job: job.JobID, run: job.RunID}
			if _, ok := l.queued[id]; !ok && len(l.queued) < queueTimeLimit {
				l.queued[id] = offeredQueueTime{queued: job.QueuedAt, seen: now}
			}
		}
	}
}

// withOfferedQueueTime is job with the queue time its offer carried when the
// message itself carried none. The entry stays until forgetQueueTime, so a
// start whose identity write failed takes it again when it is redelivered.
func (l *Listener) withOfferedQueueTime(job dispatch.Job) dispatch.Job {
	if !job.QueuedAt.IsZero() || job.JobID == "" || job.RunID == 0 {
		return job
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if q, ok := l.queued[offeredJob{job: job.JobID, run: job.RunID}]; ok {
		job.QueuedAt = q.queued
	}
	return job
}

// forgetQueueTime drops a job's offered queue time once its row holds one.
func (l *Listener) forgetQueueTime(job dispatch.Job) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.queued, offeredJob{job: job.JobID, run: job.RunID})
}
