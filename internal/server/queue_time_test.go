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

// AN OFFER'S QUEUE TIME BELONGS TO ONE RUN OF ONE JOB, the latest offer of it
// wins, a time the message carried is its own, each kind's timed jobs are
// counted, and offers are forgotten past their retention and kept to a limit,
// so a listener that runs for months does not keep every job it was offered.
func TestAnOfferedQueueTimeIsKeptPerRunAndForgotten(t *testing.T) {
	t.Parallel()

	l := NewListener(nil, "billet-4vcpu-a", &fakeSession{})
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	queued := now.Add(-time.Minute)
	offered := job11
	offered.QueuedAt = queued.Add(-2 * time.Hour)
	otherRun := job11
	otherRun.RunID, otherRun.QueuedAt = 999, queued.Add(-time.Hour)
	untimed := job13
	if a, b := l.rememberQueueTimes([]dispatch.Job{offered, untimed}, []dispatch.Job{otherRun}); a != 1 || b != 1 {
		t.Errorf("counted %d offered and %d assigned jobs with a queue time, want 1 and 1", a, b)
	}

	requeued := offered
	requeued.QueuedAt = queued
	l.rememberQueueTimes(nil, []dispatch.Job{requeued})

	if got := l.withOfferedQueueTime(job11).QueuedAt; !got.Equal(queued) {
		t.Errorf("the start took %s, want its own run's latest offer, %s", got, queued)
	}
	other := dispatch.Job{JobID: job11.JobID, RunID: 999}
	if got := l.withOfferedQueueTime(other).QueuedAt; !got.Equal(otherRun.QueuedAt) {
		t.Errorf("the other run's start took %s, want its own %s", got, otherRun.QueuedAt)
	}
	if got := l.withOfferedQueueTime(dispatch.Job{JobID: job11.JobID}).QueuedAt; !got.IsZero() {
		t.Errorf("a start naming no run took %s; a job id alone may name two runs", got)
	}
	runless := dispatch.Job{JobID: "job-14", QueuedAt: queued}
	l.rememberQueueTimes([]dispatch.Job{runless}, nil)
	if got := l.withOfferedQueueTime(dispatch.Job{JobID: "job-14"}).QueuedAt; !got.IsZero() {
		t.Errorf("a start took %s from an offer that named no run", got)
	}
	carried := job11
	carried.QueuedAt = queued.Add(-time.Hour)
	if got := l.withOfferedQueueTime(carried).QueuedAt; !got.Equal(carried.QueuedAt) {
		t.Errorf("a start carrying its own queue time took %s, want its own %s", got, carried.QueuedAt)
	}

	now = now.Add(queueTimeRetention - time.Hour)
	recent := job12
	recent.QueuedAt = now
	l.rememberQueueTimes([]dispatch.Job{recent}, nil)
	if got := l.withOfferedQueueTime(job11).QueuedAt; !got.Equal(queued) {
		t.Errorf("an offer inside its retention was dropped: the start took %s, want %s", got, queued)
	}

	now = now.Add(2 * time.Hour)
	l.rememberQueueTimes(nil, nil)
	if got := l.withOfferedQueueTime(job11).QueuedAt; !got.IsZero() {
		t.Errorf("a start took %s from an offer past its retention", got)
	}
	if got := l.withOfferedQueueTime(job12).QueuedAt; !got.Equal(recent.QueuedAt) {
		t.Errorf("an offer two hours old was dropped with the expired ones: the start took %s", got)
	}

	for i := range queueTimeLimit + 1 {
		l.rememberQueueTimes([]dispatch.Job{{JobID: "job", RunID: int64(i + 1), QueuedAt: queued}}, nil)
	}
	if len(l.queued) != queueTimeLimit {
		t.Errorf("%d offers kept, want the limit, %d", len(l.queued), queueTimeLimit)
	}
}

// AN OFFER IN THE SAME MESSAGE AS ITS START IS SEEN FIRST, and a start whose
// identity write failed takes the offer's time when it is redelivered.
func TestAQueueTimeArrivesWithItsStartAndSurvivesAFailedWrite(t *testing.T) {
	t.Parallel()

	for _, failures := range []int{0, 1} {
		queued := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Millisecond)
		p := newRunnerlessPool(t, job11)
		runner := p.launchedFor11(t)

		left := failures
		p.l.writeJobIdentity = func(ctx context.Context, leaseID string, job alloc.HistoryJob) error {
			if left > 0 {
				left--
				return errors.New("injected: the ledger is busy")
			}
			return p.a.RecordJobIdentity(ctx, leaseID, job)
		}

		offered := job12
		offered.QueuedAt = queued
		started := job12
		started.RunnerID, started.RunnerName = 77, runner.RunnerName
		msgs := []*Message{{MessageID: 2, Available: []dispatch.Job{offered}, Started: []dispatch.Job{started}}}
		if failures > 0 {
			msgs = append(msgs, &Message{MessageID: 3, Started: []dispatch.Job{started}})
		}
		for _, msg := range msgs {
			msg.Statistics = &Statistics{TotalAssignedJobs: 1}
			if err := p.l.handle(t.Context(), msg); err != nil {
				t.Fatalf("start job 12 (message %d): %v", msg.MessageID, err)
			}
		}

		got, err := p.a.Job(t.Context(), runner.LeaseID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if left != 0 || got.GitHubQueuedAt != stamp(queued) {
			t.Errorf("with %d failed writes the row's queue time is %q (%d failures unspent), want the offer's %s",
				failures, got.GitHubQueuedAt, left, stamp(queued))
		}
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
