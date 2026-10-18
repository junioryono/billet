package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// stamp is a time as job_history keeps it.
func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// A START THAT CARRIES NO QUEUE TIME TAKES THE ONE ITS OFFER OR ASSIGNMENT
// CARRIED, which is what GitHub sends: queueTime 0001-01-01 on every JobStarted
// and JobCompleted the reference fleet received on 2026-10-18. Driven through
// handle for both message kinds that carry it, with the start on a runner
// launched for another job, so the time is keyed by the job that ran and not by
// the lease.
func TestAStartTakesTheQueueTimeItsJobWasOfferedWith(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"assigned", "available"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			queued := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Millisecond)
			launchedFor, ran := job11, job12
			launchedFor.QueuedAt = queued.Add(-time.Hour)
			ran.QueuedAt = queued

			assigned := []dispatch.Job{launchedFor}
			if kind == "assigned" {
				assigned = append(assigned, ran)
			}
			p := newRunnerlessPool(t, assigned...)
			swapped := p.launchedFor11(t)
			if kind == "available" {
				if err := p.l.handle(t.Context(), &Message{MessageID: 2, Available: []dispatch.Job{ran},
					Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
					t.Fatalf("offer job 12: %v", err)
				}
			}

			told := &waitsTold{}
			p.a.ObserveQueueWaits(told)

			started := job12
			started.RunnerID, started.RunnerName = 77, swapped.RunnerName
			started.ScaleSetAssignedAt = queued.Add(30 * time.Second)
			if err := p.l.handle(t.Context(), &Message{MessageID: 3, Started: []dispatch.Job{started},
				Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
				t.Fatalf("start job 12 on the runner launched for 11: %v", err)
			}

			got, err := p.a.Job(t.Context(), swapped.LeaseID)
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if got.GitHubQueuedAt != stamp(queued) {
				t.Errorf("the row's queue time is %q, want job 12's from its %s message, %s",
					got.GitHubQueuedAt, kind, stamp(queued))
			}
			if _, waits := told.told(); len(waits) != 1 || waits[0] < 40*time.Minute || waits[0] > 41*time.Minute {
				t.Errorf("told %v, want one wait of about 40m", waits)
			}
		})
	}
}

// AN OFFER'S QUEUE TIME BELONGS TO ONE RUN OF ONE JOB, the first offer of it
// wins, a time the message carried is its own, and offers are forgotten past
// their retention and kept to a limit, so a listener that runs for months does
// not keep every job it was ever offered.
func TestAnOfferedQueueTimeIsKeptPerRunAndForgotten(t *testing.T) {
	t.Parallel()

	l := NewListener(nil, "billet-4vcpu-a", &fakeSession{})
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	queued := now.Add(-time.Minute)
	offered := job11
	offered.QueuedAt = queued
	rerun := job11
	rerun.RunID, rerun.QueuedAt = 999, queued.Add(-time.Hour)
	l.rememberQueueTimes([]dispatch.Job{offered}, []dispatch.Job{rerun})

	redelivered := offered
	redelivered.QueuedAt = queued.Add(time.Hour)
	l.rememberQueueTimes(nil, []dispatch.Job{redelivered})

	if got := l.withOfferedQueueTime(job11).QueuedAt; !got.Equal(queued) {
		t.Errorf("the start took %s, want its own run's first offer, %s", got, queued)
	}
	other := dispatch.Job{JobID: job11.JobID, RunID: 999}
	if got := l.withOfferedQueueTime(other).QueuedAt; !got.Equal(rerun.QueuedAt) {
		t.Errorf("the other run's start took %s, want its own %s", got, rerun.QueuedAt)
	}
	if got := l.withOfferedQueueTime(dispatch.Job{JobID: job11.JobID}).QueuedAt; !got.IsZero() {
		t.Errorf("a start naming no run took %s; a job id alone may name two runs", got)
	}
	runless := dispatch.Job{JobID: "job-14", QueuedAt: queued}
	l.rememberQueueTimes([]dispatch.Job{runless})
	if got := l.withOfferedQueueTime(dispatch.Job{JobID: "job-14"}).QueuedAt; !got.IsZero() {
		t.Errorf("a start took %s from an offer that named no run", got)
	}
	carried := job11
	carried.QueuedAt = queued.Add(-time.Hour)
	if got := l.withOfferedQueueTime(carried).QueuedAt; !got.Equal(carried.QueuedAt) {
		t.Errorf("a start carrying its own queue time took %s, want its own %s", got, carried.QueuedAt)
	}

	now = now.Add(queueTimeRetention + time.Hour)
	l.rememberQueueTimes([]dispatch.Job{job12})
	if len(l.queued) != 0 {
		t.Errorf("%d offers kept past the retention, want none", len(l.queued))
	}

	for i := range queueTimeLimit + 1 {
		l.rememberQueueTimes([]dispatch.Job{{JobID: "job", RunID: int64(i + 1), QueuedAt: queued}})
	}
	if len(l.queued) != queueTimeLimit {
		t.Errorf("%d offers kept, want the limit, %d", len(l.queued), queueTimeLimit)
	}
}

// A START WHOSE IDENTITY WRITE FAILED TAKES THE OFFER'S TIME WHEN IT IS
// REDELIVERED, and an offer in the same message as its start is seen first.
func TestAQueueTimeSurvivesAFailedWriteAndArrivesWithItsStart(t *testing.T) {
	t.Parallel()

	queued := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Millisecond)
	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)

	failures := 1
	p.l.writeJobIdentity = func(ctx context.Context, leaseID string, job alloc.HistoryJob) error {
		if failures > 0 {
			failures--
			return errors.New("injected: the ledger is busy")
		}
		return p.a.RecordJobIdentity(ctx, leaseID, job)
	}

	offered := job12
	offered.QueuedAt = queued
	started := job12
	started.RunnerID, started.RunnerName = 77, runner.RunnerName
	for _, msg := range []*Message{
		{MessageID: 2, Available: []dispatch.Job{offered}, Started: []dispatch.Job{started}},
		{MessageID: 3, Started: []dispatch.Job{started}},
	} {
		msg.Statistics = &Statistics{TotalAssignedJobs: 1}
		if err := p.l.handle(t.Context(), msg); err != nil {
			t.Fatalf("start job 12 (message %d): %v", msg.MessageID, err)
		}
	}

	got, err := p.a.Job(t.Context(), runner.LeaseID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if failures != 0 || got.GitHubQueuedAt != stamp(queued) {
		t.Errorf("after a failed write the redelivered start left queue time %q (%d failures unspent), "+
			"want the offer's %s", got.GitHubQueuedAt, failures, stamp(queued))
	}
}

// A COMPLETION NEVER TAKES AN OFFER'S QUEUE TIME, whether or not it names its
// runner. Its run may be filled from the lease's launch, which can be another
// run of the job, and one that names no runner may find a lease running another
// job: either would measure a wait from someone else's queue time.
func TestACompletionNeverTakesAnOffersQueueTime(t *testing.T) {
	t.Parallel()

	for _, named := range []bool{true, false} {
		queued := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Millisecond)
		offered := job11
		offered.QueuedAt = queued
		p := newRunnerlessPool(t, offered)
		runner := p.launchedFor11(t)

		completed := job11
		completed.Result = "Succeeded"
		if named {
			completed.RunnerID, completed.RunnerName = 77, runner.RunnerName
		}
		if err := p.l.handle(t.Context(), &Message{MessageID: 2, Completed: []dispatch.Job{completed},
			Statistics: &Statistics{TotalAssignedJobs: 0}}); err != nil {
			t.Fatalf("complete job 11 (runner named: %t): %v", named, err)
		}

		got, err := p.a.Job(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if got.GitHubQueuedAt != "" {
			t.Errorf("a completion (runner named: %t) recorded the offer's queue time %q, want none",
				named, got.GitHubQueuedAt)
		}
	}
}
