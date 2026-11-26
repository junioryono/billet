package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
)

// A JOB GITHUB STOPPED OFFERING IS DECIDED STRANDED, ITS RUN CANCELLED ONCE, AND
// ITS FAILED JOBS RE-RUN ONCE (#911).
//
// Through the real listener, node plane, allocator and re-run loop, against a
// scripted GitHub answering what GitHub.com was measured to answer, in the shape of
// 2026-11-20: the job's launch is queued for a host that has registered and then
// takes nothing (#909), GitHub cancels the unrun assignment, which withdraws the
// launch (#910), and offers the job no more, while its REST record keeps the job
// queued with no runner, as it did for ten hours. The wait before a launch is
// asked about is shortened; every other condition of the decision holds as in
// production.
func TestAStrandedJobIsDecidedThenItsRunCancelledAndRerun(t *testing.T) {
	const (
		request     int64 = 8001
		run         int64 = 801
		job         int64 = 9001
		name              = "lint"
		silent            = "silent-1"
		incarnation       = "0123456789abcdef0123456789abcdef"
	)

	s, _, _ := simulatedStack(t, overTheWire, withRetryStranded(), withStrandedAfter(time.Second),
		func(c *stackConfig) { c.noFirstNode = true })
	runs := newGitHubRuns(t)
	s.plane.setREST(runs.serve)

	deployment, err := state.DeploymentID(s.dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}
	registerSilentHost(t, s.wireAddr, deployment, silent, incarnation)

	// ASKED BEFORE ITS JOB, as a loop that had already passed once would have: a
	// recovery begins only for a launch that ended after its tier asked.
	if _, err := s.alloc.AskStranded(t.Context(), []string{simulatedTier}); err != nil {
		t.Fatalf("AskStranded: %v", err)
	}

	stop := s.run(t)
	defer stop()

	// WITHDRAWN BEFORE THE PLANE STOPS, PASS OR FAIL: a launch queued for a host
	// that never polls is otherwise held for its ten-minute command timeout, past
	// the plane's own stop. A withdrawal answers it as nothing started.
	defer func() {
		if err := s.wire.Withdraw(context.WithoutCancel(t.Context()), silent, incarnation); err != nil &&
			!errors.Is(err, nodeplane.ErrUnregistered) {
			t.Errorf("withdraw the silent host: %v", err)
		}
	}()

	fields := fakeactions.JobFields{RunID: run, Name: name}
	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fields.Apply(fakeactions.JobJSON("JobAvailable", request, "push", simulatedTier)))

	deadline := time.Now().Add(30 * time.Second)
	for !containsID(s.plane.acquiredIDs(), request) {
		if time.Now().After(deadline) {
			t.Fatalf("billet never bid for job %d", request)
		}

		time.Sleep(50 * time.Millisecond)
	}

	// GITHUB'S RECORD FROM HERE ON: the run is going and its job is queued with no
	// runner, whatever happens to the assignment.
	runs.queued(run, 1, job, name)

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fields.Apply(fakeactions.JobJSON("JobAssigned", request, "push", simulatedTier)))

	// QUEUED FOR THE HOST BEFORE GITHUB CANCELS, so the cancellation withdraws a
	// launch waiting in the host's queue (#910), not one remembered as withdrawn
	// before it got there. The launch itself, by its request: the harness's reaper
	// queues tends and sweeps for the same host.
	deadline = time.Now().Add(30 * time.Second)
	for !s.wire.QueuedLaunchForTest(silent, request) {
		if time.Now().After(deadline) {
			t.Fatalf("the launch of job %d never reached %s's queue", request, silent)
		}

		time.Sleep(20 * time.Millisecond)
	}

	// THE ASSIGNMENT NOBODY RAN IS CANCELLED, and the job is offered no more.
	cancelled := fakeactions.JobFields{RunID: run, Name: name, Result: "canceled"}
	stranded := awaitStranded(t, s, func() {
		s.plane.queue(fakeactions.StatisticsJSON(0, 0),
			cancelled.Apply(fakeactions.JobJSON("JobCompleted", request, "push", simulatedTier)))
	})
	if stranded.RunID != run || stranded.JobName != name {
		t.Fatalf("decided stranded: %+v, want run %d's %q", stranded, run, name)
	}

	deadline = time.Now().Add(60 * time.Second)
	for {
		status, attempt := runs.state(run)
		recovering, err := s.alloc.RecoveringRun(t.Context(), stranded.Repository, run)
		if err == nil && !recovering && status == "queued" && attempt == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run was never recovered: GitHub says %s on attempt %d, billet recovering %v (%v); "+
				"%d cancel(s), %d re-run(s)", status, attempt, recovering, err, runs.cancelled(run), runs.requests(run))
		}

		time.Sleep(50 * time.Millisecond)
	}

	// ONCE EACH, though the loop passed many times while it waited.
	if got := runs.cancelled(run); got != 1 {
		t.Errorf("%d cancels of the run, want 1", got)
	}
	if got := runs.requests(run); got != 1 {
		t.Errorf("%d re-runs of the run's failed jobs, want 1", got)
	}
	if calls := s.plane.Calls("/cancel"); len(calls) != 1 {
		t.Errorf("%d cancel requests reached GitHub, want 1: %+v", len(calls), calls)
	}
}

// awaitStranded runs end, then waits for the loop to decide a launch stranded, and
// returns it.
func awaitStranded(t *testing.T, s *stack, end func()) alloc.AlertJob {
	t.Helper()

	end()

	deadline := time.Now().Add(60 * time.Second)
	for {
		jobs, err := s.alloc.StrandedAlertJobs(t.Context(), time.Now().Add(-time.Hour), alloc.AlertCursor{}, 10)
		if err == nil && len(jobs) == 1 {
			return jobs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no launch was decided stranded: %+v, %v", jobs, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// registerSilentHost registers a simulated host over the real wire that never asks
// for a command afterwards: a host that has stopped taking work while its
// registration still stands, which a launch is placed on and never taken from.
func registerSilentHost(t *testing.T, addr, deployment, name, incarnation string) {
	t.Helper()

	body, err := json.Marshal(nodeapi.RegisterRequest{
		Version: nodeapi.Version, MinVersion: nodeapi.MinVersion, Incarnation: incarnation,
		Node: name, Provider: config.ProviderSimulated, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
		InventoryKnown: true,
	})
	if err != nil {
		t.Fatalf("marshal a registration: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+addr+"/v1/register", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build a registration: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("register %s: %s, and its body would not read: %v", name, res.Status, err)
		}

		t.Fatalf("register %s: %s: %s", name, res.Status, out)
	}
}
