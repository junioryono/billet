package server

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
)

// withdrawingLaunches holds every launch as a node that has not taken it does, and
// takes one back when asked, as the node plane does for a launch no node has taken.
type withdrawingLaunches struct {
	fakeRunner

	mu        sync.Mutex
	held      map[string]chan struct{}
	early     map[string]bool
	jobs      map[string]dispatch.Job
	forgotten []string
	started   int
	withdrawn []string
}

func (r *withdrawingLaunches) Launch(_ context.Context, lease *alloc.Lease, job dispatch.Job) error {
	gate := make(chan struct{})

	r.mu.Lock()
	r.started++
	if r.jobs == nil {
		r.jobs = map[string]dispatch.Job{}
	}
	r.jobs[lease.ID] = job
	if r.early[lease.ID] {
		delete(r.early, lease.ID)
		r.mu.Unlock()

		return dispatch.ErrWithdrawn
	}
	if r.held == nil {
		r.held = map[string]chan struct{}{}
	}
	r.held[lease.ID] = gate
	r.mu.Unlock()

	<-gate

	return dispatch.ErrWithdrawn
}

func (r *withdrawingLaunches) WithdrawLaunch(leaseID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.withdrawn = append(r.withdrawn, leaseID)

	gate, held := r.held[leaseID]
	if !held {
		if r.early == nil {
			r.early = map[string]bool{}
		}
		r.early[leaseID] = true

		return true
	}
	delete(r.held, leaseID)
	close(gate)

	return true
}

func (r *withdrawingLaunches) ForgetWithdrawal(leaseID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.early, leaseID)
	r.forgotten = append(r.forgotten, leaseID)
}

func (r *withdrawingLaunches) forgottenNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.forgotten)
}

func (r *withdrawingLaunches) withdrawnNow() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.withdrawn)
}

func (r *withdrawingLaunches) release() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, gate := range r.held {
		delete(r.held, id)
		close(gate)
	}
}

func (r *withdrawingLaunches) counts() (started, withdrawn int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.started, len(r.withdrawn)
}

// A JOB GITHUB CANCELLED BEFORE ANY NODE TOOK ITS LAUNCH DOES NOT HOLD THIS TIER'S
// MESSAGES. The launch is withdrawn, the cancellation is handled at once, and GitHub's
// next assignment of the job is bought. On 2026-11-20 a node did not take a launch for
// ten minutes; the tier waited on it while GitHub cancelled the assignment, assigned the
// job again and cancelled that too, so the second assignment was read together with its
// cancellation and never bought, and GitHub did not offer the job a third time.
func TestACancelledJobsLaunchNoNodeTookIsWithdrawn(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := &withdrawingLaunches{}
		t.Cleanup(runner.release)

		var (
			mu           sync.Mutex
			acknowledged []int64
			delivered    int
		)

		job := dispatch.Job{RunID: 6, JobID: "job-61"}
		cancelled := job
		cancelled.Result = "canceled"
		again := dispatch.Job{RunID: 6, JobID: "job-62"}

		session := &fakeSession{
			onGet: func() (*Message, error) {
				mu.Lock()
				delivered++
				n := delivered
				mu.Unlock()

				switch n {
				case 1:
					return &Message{MessageID: 1, Assigned: []dispatch.Job{job},
						Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
				case 2:
					return &Message{MessageID: 2, Completed: []dispatch.Job{cancelled},
						Statistics: &Statistics{TotalAssignedJobs: 0}}, nil
				// THE SAME JOB ASSIGNED AGAIN, and under a new job id, as GitHub does it.
				case 3:
					return &Message{MessageID: 3, Assigned: []dispatch.Job{job},
						Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
				case 4:
					return &Message{MessageID: 4, Assigned: []dispatch.Job{again},
						Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
			onDelete: func(id int64) error {
				mu.Lock()
				defer mu.Unlock()

				acknowledged = append(acknowledged, id)

				return nil
			},
		}
		acked := func(id int64) bool {
			mu.Lock()
			defer mu.Unlock()

			return slices.Contains(acknowledged, id)
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
		synctest.Wait()

		if !acked(2) {
			t.Fatal("the cancellation waited on a launch no node had taken")
		}
		if !acked(3) || !acked(4) {
			t.Fatal("GitHub's next assignments of the job were not handled")
		}
		if started, withdrawn := runner.counts(); withdrawn != 1 || started != 3 {
			t.Fatalf("%d launches started and %d withdrawn, want the first withdrawn and each "+
				"next assignment launched", started, withdrawn)
		}

		// WHAT THE RUNNER REMEMBERED ENDS WITH THE LAUNCH IT WAS FOR.
		if withdrawn, forgotten := runner.withdrawnNow(), runner.forgottenNow(); !slices.Equal(withdrawn, forgotten) {
			t.Fatalf("withdrew %v and forgot %v, want each withdrawal forgotten once its launch returned",
				withdrawn, forgotten)
		}

		stop()
		<-done
	})
}

// withdrawnJobs is the job of each launch withdrawn, in order.
func (r *withdrawingLaunches) withdrawnJobs() []dispatch.Job {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]dispatch.Job, 0, len(r.withdrawn))
	for _, id := range r.withdrawn {
		out = append(out, r.jobs[id])
	}

	return out
}

// A CANCELLATION WITHDRAWS ONLY THE LAUNCH OF THE JOB IT NAMES, matched as the resolver
// matches a job: GitHub's job id with its run, since a job id alone is not unique across
// runs, and nothing at all when more than one launch could be that job.
func TestACancellationWithdrawsOnlyItsOwnJobsLaunch(t *testing.T) {
	t.Parallel()

	one := dispatch.Job{RequestID: 11, RunID: 101, JobID: "shared"}
	other := dispatch.Job{RequestID: 12, RunID: 102, JobID: "shared"}
	twin := dispatch.Job{RequestID: 12, RunID: 101, JobID: "shared"}
	runless := dispatch.Job{RequestID: 11, JobID: "shared"}

	for _, tc := range []struct {
		name     string
		assigned []dispatch.Job
		cancel   dispatch.Job
		want     []int64
	}{
		{"its run named", []dispatch.Job{one, other}, dispatch.Job{RequestID: 11, RunID: 101, JobID: "shared"}, []int64{11}},
		{"its request not named, for a launch that has one", []dispatch.Job{one},
			dispatch.Job{RunID: 101, JobID: "shared"}, nil},
		{"no run named", []dispatch.Job{one, other}, dispatch.Job{JobID: "shared"}, nil},
		{"another run named", []dispatch.Job{one, other}, dispatch.Job{RunID: 103, JobID: "shared"}, nil},
		{"a launch alone, its cancellation naming no run", []dispatch.Job{one}, dispatch.Job{JobID: "shared"}, nil},
		{"a launch alone whose assignment named no run", []dispatch.Job{runless},
			dispatch.Job{RequestID: 11, RunID: 101, JobID: "shared"}, nil},
		{"neither naming a run", []dispatch.Job{runless}, dispatch.Job{JobID: "shared"}, nil},
		{"the one job assigned twice, so launched once", []dispatch.Job{one, twin},
			dispatch.Job{RequestID: 11, RunID: 101, JobID: "shared"}, []int64{11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				tiers := []config.Tier{tier("a-work")}
				a := flightAllocator(t, tiers)
				runner := &withdrawingLaunches{}
				t.Cleanup(runner.release)

				cancelled := tc.cancel
				cancelled.Result = "canceled"

				var (
					mu           sync.Mutex
					delivered    int
					acknowledged []int64
				)
				session := &fakeSession{
					onGet: func() (*Message, error) {
						mu.Lock()
						delivered++
						n := delivered
						mu.Unlock()

						switch n {
						case 1:
							return &Message{MessageID: 1, Assigned: tc.assigned,
								Statistics: &Statistics{TotalAssignedJobs: len(tc.assigned)}}, nil
						case 2:
							return &Message{MessageID: 2, Completed: []dispatch.Job{cancelled}}, nil
						}
						time.Sleep(time.Minute)

						return nil, ErrNoMessage
					},
					onDelete: func(id int64) error {
						mu.Lock()
						defer mu.Unlock()

						acknowledged = append(acknowledged, id)

						return nil
					},
				}

				stop, done := runFlightListener(t, a, tiers[0].Label, session, runner)
				synctest.Wait()

				// ONE FOR EACH ASSIGNMENT AT LEAST, so each case has the launches it names; a
				// withdrawn one is bought again for the work still assigned.
				if started, _ := runner.counts(); started < len(tc.assigned) {
					t.Fatalf("%d launches started, want one for each of %d assignments", started, len(tc.assigned))
				}

				var got []int64
				for _, job := range runner.withdrawnJobs() {
					got = append(got, job.RequestID)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("withdrew the launches of requests %v, want %v", got, tc.want)
				}

				// A CANCELLATION THAT WITHDREW ITS LAUNCH IS HANDLED, its job still found:
				// one whose launch's lease is gone before it is resolved would be refused.
				mu.Lock()
				handled := slices.Contains(acknowledged, 2)
				mu.Unlock()
				if len(tc.want) > 0 && !handled {
					t.Fatal("a cancellation that withdrew its launch was not handled")
				}

				runner.release()
				stop()
				<-done
			})
		})
	}
}

// TWO LAUNCHES IN FLIGHT THAT ARE BOTH THE CANCELLED JOB ARE AMBIGUOUS, and neither is
// withdrawn. The listener holds one launch for one job, so this is asked of the match
// itself.
func TestTwoLaunchesOfTheCancelledJobWithdrawNeither(t *testing.T) {
	t.Parallel()

	runner := &withdrawingLaunches{}
	l := &Listener{runner: runner, flights: map[string]*launchFlight{
		"a": {leaseID: "a", jobID: "shared", runID: 101, done: make(chan struct{})},
		"b": {leaseID: "b", jobID: "shared", runID: 101, done: make(chan struct{})},
	}}

	l.withdrawCancelledLaunch(dispatch.Job{JobID: "shared", RunID: 101, Result: "canceled"})

	if got := runner.withdrawnNow(); len(got) != 0 {
		t.Fatalf("withdrew %v of two launches either of which could be the cancelled job", got)
	}
}

// A CANCELLED LAUNCH STILL WAITING FOR ITS TURN TAKES NONE: it settles at once, as one
// that started nothing, rather than behind unrelated launches holding every slot.
func TestACancelledLaunchWaitingForItsTurnTakesNone(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("a-work")}
		a := flightAllocator(t, tiers)
		runner := &withdrawingLaunches{}
		t.Cleanup(runner.release)

		busy := dispatch.Job{RunID: 7, JobID: "busy"}
		waiting := dispatch.Job{RunID: 7, JobID: "waiting"}
		cancelled := waiting
		cancelled.Result = "canceled"

		var (
			mu           sync.Mutex
			delivered    int
			acknowledged []int64
		)
		session := &fakeSession{
			onGet: func() (*Message, error) {
				mu.Lock()
				delivered++
				n := delivered
				mu.Unlock()

				switch n {
				case 1:
					return &Message{MessageID: 1, Assigned: []dispatch.Job{busy},
						Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
				case 2:
					return &Message{MessageID: 2, Assigned: []dispatch.Job{waiting},
						Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
				case 3:
					return &Message{MessageID: 3, Completed: []dispatch.Job{cancelled},
						Statistics: &Statistics{TotalAssignedJobs: 1}}, nil
				}
				time.Sleep(time.Minute)

				return nil, ErrNoMessage
			},
			onDelete: func(id int64) error {
				mu.Lock()
				defer mu.Unlock()

				acknowledged = append(acknowledged, id)

				return nil
			},
		}

		stop, done := runFlightListener(t, a, tiers[0].Label, session, runner, WithPoolLaunchBatch(1))
		synctest.Wait()

		mu.Lock()
		handled := slices.Contains(acknowledged, 3)
		mu.Unlock()
		if !handled {
			t.Fatal("the cancellation waited for a launch that never had its turn")
		}
		if started, _ := runner.counts(); started != 1 {
			t.Fatalf("%d launches called, want only the one holding the slot", started)
		}

		runner.release()
		stop()
		<-done
	})
}
