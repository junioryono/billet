package server

import (
	"time"

	"github.com/junioryono/billet/internal/dispatch"
)

// queueTimeRetention is how long an offered job's queue time is kept for its
// start. GitHub cancels a job that has waited 24 hours for a runner, so an entry
// older than that names a job that can no longer start.
const queueTimeRetention = 25 * time.Hour

// offeredQueueTime is GitHub's queue time for one job and when billet saw it.
type offeredQueueTime struct {
	queued time.Time
	seen   time.Time
}

// rememberQueueTimes keeps GitHub's queue time from the jobs an offer or an
// assignment names, keyed by GitHub's job id, until the job's start records it.
//
// THE START DOES NOT CARRY IT. Every JobStarted and JobCompleted the reference
// fleet received on 2026-10-18 under v0.17.0 had queueTime 0001-01-01 while both
// assignment times were set, and actions-runner-controller reads it from
// JobAvailable for the same reason (its #4507). The first time seen wins: a
// redelivered offer names the same job and the same time.
//
// Held in memory only, so a restart between a job's offer and its start leaves
// that job's wait unknown, never another job's.
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
			if job.JobID == "" || job.QueuedAt.IsZero() {
				continue
			}
			if _, ok := l.queued[job.JobID]; !ok {
				l.queued[job.JobID] = offeredQueueTime{queued: job.QueuedAt, seen: now}
			}
		}
	}
}

// withOfferedQueueTime is job with the queue time its offer carried when the
// message itself carried none, and forgets the offer's entry once used.
func (l *Listener) withOfferedQueueTime(job dispatch.Job) dispatch.Job {
	if job.JobID == "" {
		return job
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	q, ok := l.queued[job.JobID]
	if !ok {
		return job
	}
	delete(l.queued, job.JobID)
	if job.QueuedAt.IsZero() {
		job.QueuedAt = q.queued
	}
	return job
}
