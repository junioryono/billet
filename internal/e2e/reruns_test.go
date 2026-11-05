package e2e

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider"
)

// The steps GitHub.com records for each way a job ends, measured on 2026-11-04
// (docs/reference/records/job-reruns.md).
var (
	lostRunnerSteps = []map[string]any{
		{"number": 1, "name": "Set up job", "status": "completed", "conclusion": "success"},
		{"number": 2, "name": "Hold the runner", "status": "in_progress", "conclusion": nil},
	}
	ownFailureSteps = []map[string]any{
		{"number": 1, "name": "Set up job", "status": "completed", "conclusion": "success"},
		{"number": 2, "name": "Hold the runner", "status": "completed", "conclusion": "failure"},
		{"number": 3, "name": "Complete job", "status": "completed", "conclusion": "success"},
	}
)

// githubRuns is GitHub's REST record of the runs a test's jobs belong to, and the
// two re-run routes, which the scripted plane hands it. Scripted as the plane is:
// each record is what a test says GitHub would answer.
type githubRuns struct {
	t *testing.T

	mu      sync.Mutex
	status  map[int64]string
	attempt map[int64]int64
	jobs    map[int64][]map[string]any
	failed  map[int64]int
	oneJob  map[int64]int
}

func newGitHubRuns(t *testing.T) *githubRuns {
	t.Helper()

	return &githubRuns{t: t, status: map[int64]string{}, attempt: map[int64]int64{},
		jobs: map[int64][]map[string]any{}, failed: map[int64]int{}, oneJob: map[int64]int{}}
}

// record says what GitHub holds about one run, a completed attempt with one job.
func (g *githubRuns) record(run, attempt, job int64, runner string, steps []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.status[run], g.attempt[run] = "completed", attempt
	g.jobs[run] = []map[string]any{{"id": job, "name": "hold", "run_attempt": attempt, "runner_name": runner,
		"status": "completed", "conclusion": "failure", "steps": steps}}
}

func (g *githubRuns) requests(run int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.failed[run]
}

// serve answers a REST path the plane did not recognise as the scale-set API's.
func (g *githubRuns) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	parts := strings.Split(r.URL.Path, "/")
	// `/api/v3/repos/{owner}/{repository}/actions/{runs|jobs}/{id}[/{what}]`: the id
	// is the ninth segment.
	id := func() int64 {
		n, err := strconv.ParseInt(parts[8], 10, 64)
		if err != nil {
			g.t.Errorf("%s: %v", r.URL.Path, err)
		}

		return n
	}

	switch tail := parts[7:]; {
	case r.Method == http.MethodGet && len(tail) == 2 && tail[0] == "runs":
		run := id()
		fakeactions.WriteJSON(g.t, w, map[string]any{"id": run, "status": g.status[run], "run_attempt": g.attempt[run]})
	case r.Method == http.MethodGet && len(tail) == 3 && tail[0] == "runs" && tail[2] == "jobs":
		jobs := g.jobs[id()]
		fakeactions.WriteJSON(g.t, w, map[string]any{"total_count": len(jobs), "jobs": jobs})
	case r.Method == http.MethodPost && len(tail) == 3 && tail[0] == "runs" && tail[2] == "rerun-failed-jobs":
		run := id()
		g.failed[run]++
		// MEASURED: the run is `queued` on its attempt from the moment GitHub accepts.
		g.status[run] = "queued"
		w.WriteHeader(http.StatusCreated)
		fakeactions.WriteJSON(g.t, w, map[string]any{})
	case r.Method == http.MethodPost && len(tail) == 3 && tail[0] == "jobs" && tail[2] == "rerun":
		g.oneJob[id()]++
		w.WriteHeader(http.StatusCreated)
		fakeactions.WriteJSON(g.t, w, map[string]any{})
	default:
		g.t.Errorf("unexpected REST call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// redJob is one job a test takes to GitHub's red completion. record, when set, is
// what GitHub holds about its run by the time it completes, called with its runner:
// the run is finished before its last job's completion reaches billet, so a pass
// never reads a run GitHub has no record of. disrupt runs while the job does.
type redJob struct {
	request, run int64
	record       func(runner string)
	disrupt      func(lease string)
}

// runJobToItsEnd takes one job through the real listener and node, from the offer
// to the completion, and returns its lease.
func runJobToItsEnd(t *testing.T, s *stack, job redJob) string {
	t.Helper()

	request, run, disrupt := job.request, job.run, job.disrupt

	fields := fakeactions.JobFields{RunID: run}
	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fields.Apply(fakeactions.JobJSON("JobAvailable", request, "push", simulatedTier)))

	deadline := time.Now().Add(30 * time.Second)
	for !containsID(s.plane.acquiredIDs(), request) {
		if time.Now().After(deadline) {
			t.Fatalf("billet never bid for job %d", request)
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fields.Apply(fakeactions.JobJSON("JobAssigned", request, "push", simulatedTier)))

	names := s.awaitOneRunning(t)
	lease, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("instance %q does not carry a lease", names[0])
	}

	started := fakeactions.JobFields{RunID: run, RunnerID: 99, RunnerName: names[0]}
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		started.Apply(fakeactions.JobJSON("JobStarted", request, "push", simulatedTier)))

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), lease)
		if err == nil && rec.StartedAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the start of job %d was never recorded: %+v, %v", request, rec, err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	if disrupt != nil {
		disrupt(lease)
	}
	if job.record != nil {
		job.record(names[0])
	}

	completed := fakeactions.JobFields{RunID: run, RunnerID: 99, RunnerName: names[0], Result: "failed"}
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		completed.Apply(fakeactions.JobJSON("JobCompleted", request, "push", simulatedTier)))

	s.awaitGone(t)

	return lease
}

func containsID(ids []int64, id int64) bool {
	for _, have := range ids {
		if have == id {
			return true
		}
	}

	return false
}

// reclaim records what a node reports when its machine is taken: the reason the
// wire's own route hands MarkFailure, which writes the disruption token.
func reclaim(t *testing.T, s *stack) func(string) {
	t.Helper()

	return func(lease string) {
		t.Helper()

		held, err := s.alloc.Lease(t.Context(), lease)
		if err != nil {
			t.Fatalf("read lease %s: %v", lease, err)
		}
		if err := s.alloc.MarkFailure(t.Context(), lease, held.Epoch, "ec2 spot interruption: terminate"); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
	}
}

// awaitRerunDecision waits for the loop to decide a job, on its real ticker.
func awaitRerunDecision(t *testing.T, s *stack, lease string, want alloc.RerunDecision) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), lease)
		if err == nil && rec.RerunDecision == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s was never decided %q: %+v, %v", lease, want, rec, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// A DISRUPTED JOB IS RE-RUN ONCE, AND A JOB THAT FAILED ON ITS OWN IS LEFT ALONE.
//
// Through the real listener, node and loop, against a scripted GitHub answering what
// GitHub.com was measured to answer. The later jobs are also what prove the first
// was asked about once: each is decided by a later pass of the same loop, so those
// passes ran with the first job's row in the ledger and did not ask again.
func TestADisruptedJobIsRerunOnceAndOneThatFailedOnItsOwnIsLeftAlone(t *testing.T) {
	s, _, _ := simulatedStack(t, overTheWire, withRetryDisrupted(1))
	runs := newGitHubRuns(t)
	s.plane.setREST(runs.serve)

	stop := s.run(t)
	defer stop()

	disrupted := runJobToItsEnd(t, s, redJob{request: 6001, run: 601, disrupt: reclaim(t, s),
		record: func(runner string) { runs.record(601, 1, 7001, runner, lostRunnerSteps) }})
	awaitRerunDecision(t, s, disrupted, alloc.RerunRequested)

	if got := runs.requests(601); got != 1 {
		t.Fatalf("%d requests to re-run the disrupted job's run, want 1", got)
	}

	// A LOST LEASE WHOSE RECORD SHOWS THE JOB'S OWN FAILURE. Its disruption is what
	// makes it a candidate, and what GitHub recorded is why it is not re-run.
	own := runJobToItsEnd(t, s, redJob{request: 6002, run: 602, disrupt: reclaim(t, s),
		record: func(runner string) { runs.record(602, 1, 7002, runner, ownFailureSteps) }})
	awaitRerunDecision(t, s, own, alloc.RerunUnproven)

	// AND A JOB BILLET DID NOTHING TO, whose run GitHub records as a lost runner's: no
	// disruption, so it is never even read about.
	plain := runJobToItsEnd(t, s, redJob{request: 6003, run: 603,
		record: func(runner string) { runs.record(603, 1, 7003, runner, lostRunnerSteps) }})
	last := runJobToItsEnd(t, s, redJob{request: 6004, run: 604, disrupt: reclaim(t, s),
		record: func(runner string) { runs.record(604, 1, 7004, runner, ownFailureSteps) }})
	awaitRerunDecision(t, s, last, alloc.RerunUnproven)

	rec, err := s.alloc.Job(t.Context(), plain)
	if err != nil {
		t.Fatalf("read the undisrupted job: %v", err)
	}
	if rec.RerunDecision != "" || rec.Result != "failed" {
		t.Errorf("a job billet did nothing to = decision %q, result %q", rec.RerunDecision, rec.Result)
	}

	for run, want := range map[int64]int{601: 1, 602: 0, 603: 0, 604: 0} {
		if got := runs.requests(run); got != want {
			t.Errorf("run %d: %d requests, want %d", run, got, want)
		}
	}
	if calls := s.plane.Calls("/actions/runs/603"); len(calls) != 0 {
		t.Errorf("the run of a job billet did nothing to was read: %+v", calls)
	}
	if calls := s.plane.Calls("/rerun"); len(calls) != 1 {
		t.Errorf("%d re-run requests reached GitHub, want 1: %+v", len(calls), calls)
	}

	first, err := s.alloc.Job(t.Context(), disrupted)
	if err != nil {
		t.Fatalf("read the disrupted job: %v", err)
	}
	if first.RerunAttempt != 1 || first.RerunDecidedAt == "" {
		t.Errorf("the re-run is not recorded on the original job: %+v", first)
	}
}

// THE LIMIT HOLDS. The same run loses its runner again on the attempt billet asked
// for, and a tier allowing one re-run leaves it red and says why.
func TestTheLimitOfRerunsHolds(t *testing.T) {
	s, _, _ := simulatedStack(t, overTheWire, withRetryDisrupted(1))
	runs := newGitHubRuns(t)
	s.plane.setREST(runs.serve)

	stop := s.run(t)
	defer stop()

	first := runJobToItsEnd(t, s, redJob{request: 6101, run: 611, disrupt: reclaim(t, s),
		record: func(runner string) { runs.record(611, 1, 7101, runner, lostRunnerSteps) }})
	awaitRerunDecision(t, s, first, alloc.RerunRequested)

	second := runJobToItsEnd(t, s, redJob{request: 6102, run: 611, disrupt: reclaim(t, s),
		record: func(runner string) { runs.record(611, 2, 7102, runner, lostRunnerSteps) }})
	awaitRerunDecision(t, s, second, alloc.RerunLimit)

	if got := runs.requests(611); got != 1 {
		t.Errorf("%d requests, want the limit's 1", got)
	}
	if calls := s.plane.Calls("/rerun"); len(calls) != 1 {
		t.Errorf("%d re-run requests reached GitHub: %s", len(calls), fmt.Sprint(calls))
	}
}
