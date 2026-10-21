package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// WHAT GITHUB.COM DOES WITH AN ASSIGNMENT, MEASURED (#140).
//
// Steady advertisement (ADR-012) rests on behaviour GitHub does not document:
// how long an assignment nobody runs is kept, whether capacity that appears is
// used at once, what the advertised ceiling counts, whether lowering it takes
// an assignment back, how many sessions one installation may hold, and what a
// cancellation looks like on the wire. Each is a question about somebody
// else's service, so, like TestLiveSessionReplacement, this RECORDS rather than
// asserts: it drives one measurement against a throwaway scale set, writes
// every message, error and job status with its time to a JSON report, and
// fails only when the mechanics a finding rests on did not happen.
//
// It needs a real organization, an App billet would run with, and a repository
// whose workflow can be dispatched to queue jobs on the scale set:
//
//	BILLET_LIVE_ORG=org BILLET_LIVE_APP_ID=1 BILLET_LIVE_INSTALLATION_ID=2 \
//	BILLET_LIVE_APP_KEY=/path/key.pem BILLET_LIVE_SCALE_SET=billet-measure-u4 \
//	BILLET_LIVE_REPORT_DIR=/tmp/measure BILLET_LIVE_MEASURE=U4 \
//	BILLET_LIVE_DISPATCH_REPO=org/repo BILLET_LIVE_DISPATCH_WORKFLOW=measure.yml \
//	BILLET_LIVE_DISPATCH_TOKEN_FILE=/path/token \
//	go test ./internal/integration/ -run TestLiveAssignmentProtocol -v -count=1 -timeout 4h
//
// The workflow takes a `runs_on` input naming the scale set and a
// `sleep_seconds` input, and its one job sleeps. U2 also needs
// BILLET_LIVE_RUNNER_IMAGE, a runner image `docker run IMAGE ./run.sh
// --jitconfig CONFIG` starts. BILLET_LIVE_SAMPLE_EVERY (default 1m) spaces the
// reads of the dispatched jobs' statuses, which spend the dispatch token's
// hourly budget. Every run it dispatched is cancelled, every runner
// it started is removed and every session it opened is closed at the end; the
// scale sets are left for `billet teardown`, named in the report.
func TestLiveAssignmentProtocol(t *testing.T) {
	measure := strings.ToUpper(os.Getenv("BILLET_LIVE_MEASURE"))
	if measure == "" {
		t.Skip("BILLET_LIVE_MEASURE is unset; this measurement is opt-in (U2, U3, U4, U4-DROP, U6, U7, C1, C2, C3)")
	}

	client, set := liveScaleSet(t)

	p := &protocolProbe{
		t: t, client: client, set: set, start: time.Now(),
		report: &protocolReport{
			Measure: measure, Organization: os.Getenv("BILLET_LIVE_ORG"), ScaleSet: set.Name,
			ScaleSetID: set.ID, StartedAt: time.Now().UTC(), Findings: map[string]any{},
		},
		dispatch: liveDispatcher(t),
	}
	p.dispatch.label = set.Name

	t.Cleanup(p.finish)

	switch measure {
	case "U4":
		p.measureU4(false)
	case "U4-DROP":
		p.measureU4(true)
	case "U6":
		p.measureU6()
	case "U2":
		p.measureU2()
	case "U3":
		p.measureU3()
	case "U7":
		p.measureU7()
	case "C1":
		p.measureC1()
	case "C2":
		p.measureC2()
	case "C3":
		p.measureC3()
	default:
		t.Fatalf("BILLET_LIVE_MEASURE=%q names no measurement", measure)
	}
}

// protocolProbe is one measurement's session, dispatcher and record.
type protocolProbe struct {
	t        *testing.T
	client   *scaleset.Client
	set      *scaleset.ScaleSet
	start    time.Time
	dispatch *dispatcher

	mu      sync.Mutex
	report  *protocolReport
	session server.Session
	lastID  int64
	// runners are the containers this measurement started, removed at the end.
	runners []string
}

// protocolReport is what one measurement observed. A record, not a verdict:
// the findings are GitHub's answers, and a run that disagrees with an earlier
// one is the reason this stays a record.
type protocolReport struct {
	Measure      string    `json:"measure"`
	Organization string    `json:"organization"`
	ScaleSet     string    `json:"scale_set"`
	ScaleSetID   int       `json:"scale_set_id"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`

	// Events are every message and every failed poll, in order, with the
	// capacity the poll that carried it advertised.
	Events []protocolEvent `json:"events"`
	// Jobs are the dispatched jobs' statuses as GitHub's REST API reported
	// them, recorded each time one changed.
	Jobs []jobObservation `json:"jobs"`
	// Sessions are U7's per-session tallies.
	Sessions []sessionTally `json:"sessions,omitempty"`
	// Findings are the measurement's own derived answers, each named.
	Findings map[string]any `json:"findings"`
	// Cleanup says what was left behind, if anything.
	Cleanup []string `json:"cleanup,omitempty"`
}

type protocolEvent struct {
	At         time.Time          `json:"at"`
	Elapsed    string             `json:"elapsed"`
	Capacity   int                `json:"capacity"`
	MessageID  int64              `json:"message_id,omitempty"`
	Statistics *server.Statistics `json:"statistics,omitempty"`
	Jobs       []eventJob         `json:"jobs,omitempty"`
	Error      string             `json:"error,omitempty"`
}

type eventJob struct {
	Kind       string `json:"kind"`
	RequestID  int64  `json:"request_id"`
	RunID      int64  `json:"run_id"`
	JobID      string `json:"job_id"`
	RunnerName string `json:"runner_name,omitempty"`
	Result     string `json:"result,omitempty"`
}

type jobObservation struct {
	At         time.Time `json:"at"`
	Elapsed    string    `json:"elapsed"`
	RunID      int64     `json:"run_id"`
	JobID      int64     `json:"job_id"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion,omitempty"`
	RunnerName string    `json:"runner_name,omitempty"`
}

type sessionTally struct {
	Name     string         `json:"name"`
	Polls    int            `json:"polls"`
	Messages int            `json:"messages"`
	Errors   map[string]int `json:"errors,omitempty"`
	FirstErr string         `json:"first_error,omitempty"`
	FirstAt  string         `json:"first_error_after,omitempty"`
}

// finish closes what the measurement opened and writes the report.
func (p *protocolProbe) finish() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.t.Context()), 2*time.Minute)
	defer cancel()

	p.mu.Lock()
	session := p.session
	runners := append([]string(nil), p.runners...)
	p.mu.Unlock()

	if session != nil {
		if err := session.Close(ctx); err != nil {
			p.cleanup(fmt.Sprintf("closing the session: %v", err))
		}
	}

	for _, name := range runners {
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			p.cleanup(fmt.Sprintf("removing runner container %s: %v: %s", name, err, out))
		}
	}

	for _, run := range p.dispatch.dispatchedRuns(ctx, p.start) {
		if err := p.dispatch.cancel(ctx, run); err != nil {
			p.cleanup(fmt.Sprintf("cancelling run %d: %v", run, err))
		}
	}

	p.cleanup(fmt.Sprintf("scale set %q (id %d) is left for `billet teardown --tier %s`",
		p.set.Name, p.set.ID, p.set.Name))

	p.mu.Lock()
	p.report.EndedAt = time.Now().UTC()
	body, err := json.MarshalIndent(p.report, "", "  ")
	p.mu.Unlock()

	if err != nil {
		p.t.Errorf("render the report: %v", err)

		return
	}

	dir := os.Getenv("BILLET_LIVE_REPORT_DIR")
	if dir == "" {
		dir = p.t.TempDir()
	}

	path := filepath.Join(dir, "assignment-protocol-"+strings.ToLower(p.report.Measure)+".json")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		p.t.Errorf("write the report: %v", err)

		return
	}

	p.t.Logf("assignment-protocol findings written to %s", path)
}

func (p *protocolProbe) cleanup(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.report.Cleanup = append(p.report.Cleanup, value)
}

func (p *protocolProbe) find(key string, value any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.report.Findings[key] = value
}

func (p *protocolProbe) elapsed() string { return time.Since(p.start).Round(time.Second).String() }

// open opens the measurement's message session.
func (p *protocolProbe) open() {
	p.t.Helper()

	session, err := p.client.Session(probeCtx(p.t), p.set.ID, envOr("BILLET_LIVE_OWNER", "billet-measure"))
	if err != nil {
		p.t.Fatalf("open the message session: %v", err)
	}

	p.mu.Lock()
	p.session, p.lastID = session, 0
	p.mu.Unlock()

	p.find("session_statistics_at_open", session.Statistics())
}

// poll asks for one message at capacity, records it, and acknowledges it. A
// poll that times out empty records nothing; a failed one records its error.
func (p *protocolProbe) poll(capacity int) *server.Message {
	p.t.Helper()

	ctx, cancel := context.WithTimeout(p.t.Context(), 2*time.Minute)
	defer cancel()

	p.mu.Lock()
	session, last := p.session, p.lastID
	p.mu.Unlock()

	msg, err := session.GetMessage(ctx, last, capacity)

	switch {
	case errors.Is(err, server.ErrNoMessage):
		return nil
	case err != nil:
		p.record(protocolEvent{Capacity: capacity, Error: err.Error()})

		return nil
	case msg == nil:
		return nil
	}

	p.record(protocolEvent{Capacity: capacity, MessageID: msg.MessageID, Statistics: msg.Statistics, Jobs: jobsOf(msg)})

	if err := session.DeleteMessage(ctx, msg.MessageID); err != nil {
		p.record(protocolEvent{Capacity: capacity, Error: fmt.Sprintf("acknowledge %d: %v", msg.MessageID, err)})
	}

	p.mu.Lock()
	p.lastID = msg.MessageID
	p.mu.Unlock()

	return msg
}

func (p *protocolProbe) record(e protocolEvent) {
	e.At, e.Elapsed = time.Now().UTC(), p.elapsed()

	p.mu.Lock()
	p.report.Events = append(p.report.Events, e)
	p.mu.Unlock()

	p.t.Logf("%s capacity=%d message=%d %v %s", e.Elapsed, e.Capacity, e.MessageID, e.Jobs, e.Error)
}

func jobsOf(m *server.Message) []eventJob {
	var out []eventJob

	for _, group := range []struct {
		kind string
		jobs []dispatch.Job
	}{
		{"available", m.Available}, {"assigned", m.Assigned},
		{"started", m.Started}, {"completed", m.Completed},
	} {
		for i := range group.jobs {
			j := &group.jobs[i]
			out = append(out, eventJob{Kind: group.kind, RequestID: j.RequestID, RunID: j.RunID,
				JobID: j.JobID, RunnerName: j.RunnerName, Result: j.Result})
		}
	}

	return out
}

// pollFor polls at capacity(elapsed in this phase) until d passes or stop says
// the phase is over, sampling the jobs' statuses every minute meanwhile.
func (p *protocolProbe) pollFor(d time.Duration, capacity func(time.Duration) int,
	stop func(*server.Message) bool,
) {
	p.t.Helper()

	begin := time.Now()
	nextSample := time.Time{}

	for time.Since(begin) < d {
		if p.t.Context().Err() != nil {
			return
		}

		if time.Now().After(nextSample) {
			p.sampleJobs()

			nextSample = time.Now().Add(durationOr("BILLET_LIVE_SAMPLE_EVERY", time.Minute))
		}

		msg := p.poll(capacity(time.Since(begin)))
		if msg != nil && stop != nil && stop(msg) {
			return
		}
	}
}

// sampleJobs records each dispatched job whose status changed.
func (p *protocolProbe) sampleJobs() {
	ctx, cancel := context.WithTimeout(p.t.Context(), 30*time.Second)
	defer cancel()

	for _, run := range p.dispatch.dispatchedRuns(ctx, p.start) {
		jobs, err := p.dispatch.jobs(ctx, run)
		if err != nil {
			p.record(protocolEvent{Error: fmt.Sprintf("read run %d's jobs: %v", run, err)})

			continue
		}

		for _, j := range jobs {
			p.observeJob(jobObservation{RunID: run, JobID: j.ID, Status: j.Status,
				Conclusion: j.Conclusion, RunnerName: j.RunnerName})
		}
	}
}

func (p *protocolProbe) observeJob(o jobObservation) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := len(p.report.Jobs) - 1; i >= 0; i-- {
		prev := p.report.Jobs[i]
		if prev.JobID != o.JobID {
			continue
		}

		if prev.Status == o.Status && prev.Conclusion == o.Conclusion && prev.RunnerName == o.RunnerName {
			return
		}

		break
	}

	o.At, o.Elapsed = time.Now().UTC(), time.Since(p.start).Round(time.Second).String()
	p.report.Jobs = append(p.report.Jobs, o)
}

// queue dispatches n runs on the scale set, each one sleeping job.
func (p *protocolProbe) queue(n int) {
	p.t.Helper()

	for i := range n {
		if err := p.dispatch.dispatch(p.t.Context(), p.set.Name, sleepSeconds()); err != nil {
			p.t.Fatalf("dispatch run %d of %d: %v", i+1, n, err)
		}
	}

	p.record(protocolEvent{Error: fmt.Sprintf("dispatched %d run(s)", n)})
}

// measureU4 is the life of an assignment nobody runs: advertise 1, queue one
// job, never start a runner, and poll at 1 for the window (90 minutes by
// default), recording every assignment and cancellation. With drop, the
// advertisement falls to 0 after the first assignment.
func (p *protocolProbe) measureU4(drop bool) {
	p.open()
	p.queue(1)

	assigned := false

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 90*time.Minute), func(time.Duration) int {
		if drop && assigned {
			return 0
		}

		return 1
	}, func(m *server.Message) bool {
		if len(m.Assigned) > 0 {
			assigned = true
		}

		return false
	})

	p.sampleJobs()
	p.summarizeCycles()
}

// summarizeCycles derives the assignment and cancellation pairs from the
// events, for the report's findings.
func (p *protocolProbe) summarizeCycles() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var assignments, completions []string

	for _, e := range p.report.Events {
		for _, j := range e.Jobs {
			switch j.Kind {
			case "assigned":
				assignments = append(assignments, e.Elapsed)
			case "completed":
				completions = append(completions, e.Elapsed+" "+j.Result)
			}
		}
	}

	p.report.Findings["assignments_at"] = assignments
	p.report.Findings["completions_at"] = completions
}

// measureU6 asks whether GitHub assigns as soon as capacity appears, and
// whether the last advertised capacity is honoured between polls. Phase one
// polls at 0 with a job queued, then at 1, timing the first assignment from the
// switch. Phase two polls once at 1, stops polling, queues a job while silent,
// and resumes at 0, recording whether the job is assigned anyway.
func (p *protocolProbe) measureU6() {
	p.open()
	p.queue(1)

	zero := durationOr("BILLET_LIVE_PHASE", 5*time.Minute)

	p.pollFor(zero, func(time.Duration) int { return 0 }, nil)

	switched := time.Now()

	var assignedAfter time.Duration

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 15*time.Minute), func(time.Duration) int { return 1 },
		func(m *server.Message) bool {
			if len(m.Assigned) > 0 {
				assignedAfter = time.Since(switched)

				return true
			}

			return false
		})

	if assignedAfter > 0 {
		p.find("u6a_assigned_after_capacity_appeared", assignedAfter.Round(time.Second).String())
	} else {
		p.find("u6a_assigned_after_capacity_appeared", "not within the window")
	}

	// PHASE TWO on a fresh session, so nothing from phase one is in flight.
	p.closeSession()
	p.open()

	p.poll(1)

	silent := durationOr("BILLET_LIVE_PHASE", 5*time.Minute)
	p.record(protocolEvent{Capacity: 1, Error: fmt.Sprintf("silent for %s after one poll at 1", silent)})
	p.queue(1)

	select {
	case <-time.After(silent):
	case <-p.t.Context().Done():
		return
	}

	assignedAtZero := false

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 15*time.Minute), func(time.Duration) int { return 0 },
		func(m *server.Message) bool {
			if len(m.Assigned) > 0 {
				assignedAtZero = true

				return true
			}

			return false
		})

	p.find("u6b_assigned_while_last_poll_said_1_and_resumed_at_0", assignedAtZero)
	p.sampleJobs()
}

func (p *protocolProbe) closeSession() {
	p.mu.Lock()
	session := p.session
	p.session = nil
	p.mu.Unlock()

	if session == nil {
		return
	}

	if err := session.Close(probeCtx(p.t)); err != nil {
		p.record(protocolEvent{Error: fmt.Sprintf("close the session: %v", err)})
	}
}

// measureU2 asks what the advertised ceiling counts: advertise 2, queue 5,
// start one runner on the first assignment, and count the assignments while
// it runs. Two assigned plus a third while one runs means running work is not
// counted against the ceiling.
func (p *protocolProbe) measureU2() {
	image := required(p.t, "BILLET_LIVE_RUNNER_IMAGE")

	p.open()
	p.queue(5)

	started := false
	assigned := 0

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 30*time.Minute), func(time.Duration) int { return 2 },
		func(m *server.Message) bool {
			assigned += len(m.Assigned)

			if !started && assigned > 0 {
				p.startRunner(image)

				started = true
			}

			return false
		})

	p.find("u2_assignments_counted", assigned)
	p.sampleJobs()
	p.summarizeCycles()
}

// startRunner starts one runner container on a single-use registration.
func (p *protocolProbe) startRunner(image string) {
	p.t.Helper()

	name := fmt.Sprintf("%s-runner-%d", p.set.Name, time.Now().Unix())

	jit, err := p.client.JITConfig(probeCtx(p.t), p.set.ID, name, "")
	if err != nil {
		p.t.Fatalf("generate a runner registration: %v", err)
	}

	out, err := exec.CommandContext(p.t.Context(), "docker", "run", "-d", "--name", name, image,
		"./run.sh", "--jitconfig", jit.Config()).CombinedOutput()
	if err != nil {
		p.t.Fatalf("start the runner container: %v: %s", err, out)
	}

	p.mu.Lock()
	p.runners = append(p.runners, name)
	p.mu.Unlock()

	p.record(protocolEvent{Error: "started runner " + name})
}

// measureU3 asks whether lowering the advertised capacity takes an assignment
// back: advertise 2 until two jobs are assigned, none started, then poll at 0
// for the window (20 minutes by default), recording every message.
func (p *protocolProbe) measureU3() {
	p.open()
	p.queue(2)

	assigned := 0

	p.pollFor(durationOr("BILLET_LIVE_PHASE", 15*time.Minute), func(time.Duration) int { return 2 },
		func(m *server.Message) bool {
			assigned += len(m.Assigned)

			return assigned >= 2
		})

	if assigned < 2 {
		p.t.Fatalf("only %d of 2 jobs were assigned at capacity 2, so lowering it measures nothing", assigned)
	}

	p.record(protocolEvent{Error: "dropping the advertisement to 0 with two assigned"})
	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 20*time.Minute), func(time.Duration) int { return 0 }, nil)
	p.sampleJobs()
	p.summarizeCycles()
}

// measureU7 holds many sessions on one installation at once, each on a scale
// set of its own and polling at 0, for the window (2 hours by default), and
// tallies what each was told: 409s, 429s, 403s, and the REST budget the
// installation reported.
func (p *protocolProbe) measureU7() {
	n, err := strconv.Atoi(envOr("BILLET_LIVE_SESSIONS", "10"))
	if err != nil || n < 1 {
		p.t.Fatalf("BILLET_LIVE_SESSIONS=%q is not a positive count", os.Getenv("BILLET_LIVE_SESSIONS"))
	}

	window := durationOr("BILLET_LIVE_WINDOW", 2*time.Hour)
	tallies := make([]sessionTally, n)

	var wg sync.WaitGroup

	for i := range n {
		name := fmt.Sprintf("%s-%02d", p.set.Name, i)
		tallies[i] = sessionTally{Name: name, Errors: map[string]int{}}

		set, err := p.client.EnsureScaleSet(probeCtx(p.t), name, "", []string{name})
		if err != nil {
			p.t.Fatalf("ensure scale set %s: %v", name, err)
		}

		wg.Go(func() { p.holdSession(set, window, &tallies[i]) })
	}

	budgets := map[string]any{}
	done := make(chan struct{})
	sampled := make(chan struct{})

	go func() {
		defer close(sampled)

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}

			if b, ok := p.client.RESTBudget(); ok {
				p.mu.Lock()
				budgets[p.elapsed()] = fmt.Sprintf("%d of %d, resets %s", b.Remaining, b.Limit, b.Reset.UTC())
				p.mu.Unlock()
			}
		}
	}()

	wg.Wait()
	close(done)
	<-sampled

	p.mu.Lock()
	p.report.Sessions = tallies
	p.report.Findings["rest_budget"] = budgets
	p.mu.Unlock()
}

// holdSession polls one session at 0 until window passes, tallying failures by
// their text's status.
func (p *protocolProbe) holdSession(set *scaleset.ScaleSet, window time.Duration, tally *sessionTally) {
	begin := time.Now()

	session, err := p.client.Session(probeCtx(p.t), set.ID, "billet-measure-"+set.Name)
	if err != nil {
		tally.Errors["open"]++
		tally.FirstErr, tally.FirstAt = err.Error(), time.Since(begin).Round(time.Second).String()

		return
	}

	defer func() {
		if err := session.Close(probeCtx(p.t)); err != nil {
			tally.Errors["close"]++
		}
	}()

	var last int64

	for time.Since(begin) < window && p.t.Context().Err() == nil {
		ctx, cancel := context.WithTimeout(p.t.Context(), 2*time.Minute)
		msg, err := session.GetMessage(ctx, last, 0)

		tally.Polls++

		switch {
		case errors.Is(err, server.ErrNoMessage):
		case err != nil:
			tally.Errors[statusOf(err)]++

			if tally.FirstErr == "" {
				tally.FirstErr, tally.FirstAt = err.Error(), time.Since(begin).Round(time.Second).String()
			}

			select {
			case <-time.After(30 * time.Second):
			case <-p.t.Context().Done():
			}
		case msg != nil:
			tally.Messages++
			last = msg.MessageID

			if err := session.DeleteMessage(ctx, msg.MessageID); err != nil {
				tally.Errors["acknowledge"]++
			}
		}

		cancel()
	}
}

// statusOf names a failure by the first HTTP status its text carries.
func statusOf(err error) string {
	text := err.Error()

	for _, code := range []string{"409", "429", "403", "401", "404", "500", "502", "503"} {
		if strings.Contains(text, code) {
			return code
		}
	}

	return "other"
}

// measureC1 compares on the wire a cancellation GitHub makes to reassign an
// assignment nobody ran with one an operator makes: queue one job, poll at 1,
// record the first completion after an assignment, then cancel the run and
// record the next.
func (p *protocolProbe) measureC1() {
	p.open()
	p.queue(1)

	var reassignment *eventJob

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 20*time.Minute), func(time.Duration) int { return 1 },
		func(m *server.Message) bool {
			for _, j := range jobsOf(m) {
				if j.Kind == "completed" {
					reassignment = &j

					return true
				}
			}

			return false
		})

	p.find("c1_reassignment_completion", reassignment)

	ctx, cancel := context.WithTimeout(p.t.Context(), time.Minute)
	defer cancel()

	for _, run := range p.dispatch.dispatchedRuns(ctx, p.start) {
		if err := p.dispatch.cancel(ctx, run); err != nil {
			p.t.Fatalf("cancel run %d: %v", run, err)
		}

		p.record(protocolEvent{Error: fmt.Sprintf("cancelled run %d", run)})
	}

	var cancellation *eventJob

	p.pollFor(durationOr("BILLET_LIVE_PHASE", 10*time.Minute), func(time.Duration) int { return 1 },
		func(m *server.Message) bool {
			for _, j := range jobsOf(m) {
				if j.Kind == "completed" {
					cancellation = &j

					return true
				}
			}

			return false
		})

	p.find("c1_cancellation_completion", cancellation)
	p.sampleJobs()
}

// measureC2 asks whether session statistics restore an acknowledged backlog:
// queue two jobs, acknowledge their assignments at capacity 2, close the
// session, open a new one, and record what its statistics say.
func (p *protocolProbe) measureC2() {
	p.open()
	p.queue(2)

	assigned := 0

	p.pollFor(durationOr("BILLET_LIVE_PHASE", 15*time.Minute), func(time.Duration) int { return 2 },
		func(m *server.Message) bool {
			assigned += len(m.Assigned)

			return assigned >= 2
		})

	p.find("c2_assigned_before_restart", assigned)
	p.closeSession()
	p.open()

	p.mu.Lock()
	stats := p.session.Statistics()
	p.mu.Unlock()

	p.find("c2_statistics_after_restart", stats)
	p.sampleJobs()
}

// measureC3 asks whether a message carries every assignment it counts: queue
// many jobs at a capacity covering them all, and record, per message, how many
// jobs it carried beside what its statistics say.
func (p *protocolProbe) measureC3() {
	n, err := strconv.Atoi(envOr("BILLET_LIVE_JOBS", "40"))
	if err != nil || n < 1 {
		p.t.Fatalf("BILLET_LIVE_JOBS=%q is not a positive count", os.Getenv("BILLET_LIVE_JOBS"))
	}

	p.open()
	p.queue(n)

	assigned := 0

	p.pollFor(durationOr("BILLET_LIVE_WINDOW", 20*time.Minute), func(time.Duration) int { return n },
		func(m *server.Message) bool {
			assigned += len(m.Assigned)

			return assigned >= n
		})

	p.find("c3_assignments_carried", assigned)
	p.find("c3_jobs_queued", n)
}

// sleepSeconds is how long each dispatched job sleeps once a runner takes it.
func sleepSeconds() int {
	n, err := strconv.Atoi(envOr("BILLET_LIVE_SLEEP_SECONDS", "300"))
	if err != nil || n < 1 {
		return 300
	}

	return n
}

// dispatcher queues jobs by dispatching a workflow, reads their statuses and
// cancels their runs, over GitHub's REST API with a token of its own: the App
// billet runs with holds no actions permission, and must not.
type dispatcher struct {
	repo, workflow, ref, token string
	// label is the scale set's label every job this measurement queues runs on.
	label string
	http  *http.Client

	mu sync.Mutex
	// ours is, for each run already asked, whether its job is on label.
	ours map[int64]bool
}

func liveDispatcher(t *testing.T) *dispatcher {
	t.Helper()

	tokenPath := required(t, "BILLET_LIVE_DISPATCH_TOKEN_FILE")

	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read the dispatch token: %v", err)
	}

	return &dispatcher{
		repo:     required(t, "BILLET_LIVE_DISPATCH_REPO"),
		workflow: required(t, "BILLET_LIVE_DISPATCH_WORKFLOW"),
		ref:      envOr("BILLET_LIVE_DISPATCH_REF", "main"),
		token:    strings.TrimSpace(string(token)),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (d *dispatcher) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader

	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")

	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		text, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if readErr != nil {
			text = []byte("(body unreadable: " + readErr.Error() + ")")
		}

		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(text)))
	}

	if out == nil {
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func (d *dispatcher) dispatch(ctx context.Context, runsOn string, sleep int) error {
	return d.do(ctx, http.MethodPost, "/repos/"+d.repo+"/actions/workflows/"+d.workflow+"/dispatches",
		map[string]any{"ref": d.ref, "inputs": map[string]string{
			"runs_on": runsOn, "sleep_seconds": strconv.Itoa(sleep),
		}}, nil)
}

// dispatchedRuns are the workflow's dispatched runs created since since whose
// job is queued on this measurement's label. ONLY THOSE: other sessions
// dispatch the same workflow, and the cleanup cancels every run this returns.
func (d *dispatcher) dispatchedRuns(ctx context.Context, since time.Time) []int64 {
	var out []int64

	for _, run := range d.workflowRuns(ctx, since) {
		d.mu.Lock()
		ours, known := d.ours[run]
		d.mu.Unlock()

		// A RUN'S LABEL NEVER CHANGES, so each is asked once: the token's hourly
		// budget is shared, and every other session's runs are listed too.
		if !known {
			jobs, err := d.jobs(ctx, run)
			if err != nil || len(jobs) == 0 {
				continue
			}

			ours = slices.ContainsFunc(jobs, func(j restJob) bool { return slices.Contains(j.Labels, d.label) })

			d.mu.Lock()
			if d.ours == nil {
				d.ours = map[int64]bool{}
			}
			d.ours[run] = ours
			d.mu.Unlock()
		}

		if ours {
			out = append(out, run)
		}
	}

	return out
}

// workflowRuns are the workflow's dispatched runs created since since, by
// anyone.
func (d *dispatcher) workflowRuns(ctx context.Context, since time.Time) []int64 {
	var page struct {
		Runs []struct {
			ID        int64     `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}

	path := "/repos/" + d.repo + "/actions/workflows/" + d.workflow +
		"/runs?event=workflow_dispatch&per_page=100&created=%3E%3D" + since.Add(-time.Minute).UTC().Format(time.RFC3339)

	if err := d.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil
	}

	var out []int64

	for _, r := range page.Runs {
		if !r.CreatedAt.Before(since.Add(-time.Minute)) {
			out = append(out, r.ID)
		}
	}

	return out
}

type restJob struct {
	ID         int64    `json:"id"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	RunnerName string   `json:"runner_name"`
	Labels     []string `json:"labels"`
}

func (d *dispatcher) jobs(ctx context.Context, run int64) ([]restJob, error) {
	var page struct {
		Jobs []restJob `json:"jobs"`
	}

	err := d.do(ctx, http.MethodGet, "/repos/"+d.repo+"/actions/runs/"+strconv.FormatInt(run, 10)+"/jobs", nil, &page)

	return page.Jobs, err
}

func (d *dispatcher) cancel(ctx context.Context, run int64) error {
	err := d.do(ctx, http.MethodPost, "/repos/"+d.repo+"/actions/runs/"+strconv.FormatInt(run, 10)+"/cancel", nil, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 409") {
		// Already finished: nothing left to cancel.
		return nil
	}

	return err
}
