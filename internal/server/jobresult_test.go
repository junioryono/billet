package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// GITHUB'S OWN CONCLUSION HAS TO REACH THE LEDGER, because nothing else in it
// says how a job ended. job_history.conclusion is the LEASE's terminal phase —
// whether billet's compute lifecycle finished tidily — so a build that went red
// on a lease billet tore down perfectly is recorded as `done`, and the
// attribution report's whole question ("was that failure the node or my code?")
// has no first half.
//
// DRIVEN THROUGH handle RATHER THAN recordJobResult. A test that calls the
// helper proves the helper works and stays green when the call site is deleted,
// which is the failure this repository keeps finding in its own tests.
func TestHandlingACompletionRecordsWhatGitHubConcluded(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	l := NewListener(a, tiers[0].Label, &fakeSession{},
		WithCompletionStore(openState(t)), WithRunner(&fakeRunner{}))

	job := Job{RequestID: 21, RunID: 210, Result: "failed"}
	lease := holdRunning(t, l, a, tiers[0].Label, job.RequestID)

	if err := l.handle(t.Context(), &Message{MessageID: 1, Completed: []Job{job}}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	assertRecordedResult(t, a, lease.ID, "failed")
}

// AND A JOB THAT SUCCEEDED IS RECORDED AS SUCCEEDED, so the report can tell them
// apart. A single-direction test passes against a recorder that writes "failed"
// unconditionally.
func TestHandlingASucceededCompletionRecordsThat(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	l := NewListener(a, tiers[0].Label, &fakeSession{},
		WithCompletionStore(openState(t)), WithRunner(&fakeRunner{}))

	job := Job{RequestID: 22, RunID: 220, Result: "succeeded"}
	lease := holdRunning(t, l, a, tiers[0].Label, job.RequestID)

	if err := l.handle(t.Context(), &Message{MessageID: 1, Completed: []Job{job}}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	assertRecordedResult(t, a, lease.ID, "succeeded")
}

// A DIAGNOSTIC MUST NOT BE ABLE TO STOP THE CONTROL PLANE.
//
// recordCompletion's error is fatal to the listener, and a listener error
// cancels every other listener, whose shutdowns tear down what they owe. A busy
// database while GitHub happened to report a completion must not cost the fleet
// its bookkeeping for the sake of a report — the same disproportion `complete`
// already refuses for an unbacked assignment.
//
// The failure is staged by closing the ledger under the listener, which is the
// most honest version of "the database will not answer": every allocator call
// fails, including this one.
func TestAFailureToRecordTheResultDoesNotStopTheListener(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "host", Provider: config.ProviderFirecracker, VCPU: 1 << 20,
		Memory: 1 << 20 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	var logged bytes.Buffer

	l := NewListener(a, tiers[0].Label, &fakeSession{},
		WithLogger(slog.New(slog.NewTextHandler(&logged, nil))),
		WithRunner(&fakeRunner{}))

	job := Job{RequestID: 23, RunID: 230, Result: "failed"}
	holdRunning(t, l, a, tiers[0].Label, job.RequestID)

	if err := db.Close(); err != nil {
		t.Fatalf("closing the ledger: %v", err)
	}

	// A closed ledger fails everything downstream too, so `complete` records its
	// own obligations and logs — what is asserted is only that handle did not
	// return, which is the thing that would cancel every listener.
	if err := l.handle(t.Context(), &Message{MessageID: 1, Completed: []Job{job}}); err != nil {
		t.Fatalf("a ledger that could not record a diagnostic stopped the listener: %v", err)
	}

	if !strings.Contains(logged.String(), "could not record what github concluded") {
		t.Errorf("nothing said the result had been lost: %s", logged.String())
	}
}

// THE TWO FACTS MEET IN THE REPORT AND NOWHERE ELSE. This is the end-to-end
// shape of the attribution report: an observation recorded when the host went
// silent, GitHub's conclusion recorded when the completion arrived, and a view
// that shows both without asserting a causal link between them.
func TestAFailedJobOnAForgottenHostIsReported(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)

	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderFirecracker, VCPU: 1 << 20,
		Memory: 1 << 20 * config.GiB,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	l := NewListener(a, tiers[0].Label, &fakeSession{},
		WithCompletionStore(openState(t)), WithRunner(&fakeRunner{}))

	job := Job{RequestID: 24, RunID: 240, Result: "failed"}
	lease := holdRunning(t, l, a, tiers[0].Label, job.RequestID)

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// The host goes silent. This is the plane giving up on it, which is the only
	// moment the fact exists to be recorded.
	if err := a.NodeGone(t.Context(), "epyc-1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := l.handle(t.Context(), &Message{MessageID: 1, Completed: []Job{job}}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	failures, err := a.AttributedFailures(t.Context(), time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 1 {
		t.Fatalf("reported %d attributed failures, want 1: %+v", len(failures), failures)
	}

	got := failures[0]
	if got.LeaseID != lease.ID || got.Result != "failed" ||
		got.Disruption != alloc.DisruptionNodeForgotten {
		t.Errorf("reported %+v, want lease %s failed after %s",
			got, lease.ID, alloc.DisruptionNodeForgotten)
	}
}

// refusedAcknowledgement is a session whose DeleteMessage fails.
//
// THE COHERENT WAY A TOMBSTONE SURVIVES. handle acknowledges to GitHub FIRST and
// records the local acknowledgement second, so a failed DeleteMessage leaves the
// pending row retired-but-unacknowledged — which is exactly the row that is not
// deleted — and GitHub redelivers the very same message. Failing only the local
// write would leave a tombstone nothing would ever redeliver against.
//
// A POINTER RECEIVER, because fakeSession carries a mutex and a value receiver
// copies it on every call.
type refusedAcknowledgement struct {
	fakeSession
}

func (*refusedAcknowledgement) DeleteMessage(context.Context, int64) error {
	return errors.New("the acknowledgement did not reach github")
}

// A REDELIVERY OF A SETTLED COMPLETION MUST NOT WRITE ONTO THE LEASE THAT HOLDS
// ITS REQUEST ID NOW.
//
// GitHub reuses a request id, and the escrow maps are keyed on it — so an old
// completion redelivered after a new job has taken that id resolves to the NEW
// lease. Recorded before PutPendingCompletion has said the delivery is already
// settled, the old job's result lands on its replacement's history. That
// fabricates an attributed failure for a job still running, and it also DISARMS
// the attribution for that job: a recorded result is how billet knows GitHub has
// already reported, so a genuine disruption afterwards is refused.
//
// THE REDELIVERY IS IDENTICAL TO THE FIRST DELIVERY — same message id, same
// result — because that is what a redelivery IS. Changing either would make a
// wrong write detectable by its value rather than by its absence, and the second
// half below depends on a wrong write being indistinguishable from a right one.
func TestARedeliveredSettledCompletionDoesNotRecordAgainstAReplacementLease(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, tiers)
	l := NewListener(a, tiers[0].Label, &refusedAcknowledgement{},
		WithCompletionStore(openState(t)), WithRunner(&fakeRunner{}))

	const requestID = 77

	delivery := &Message{MessageID: 5, Completed: []Job{
		{RequestID: requestID, RunID: 500, Result: "failed"},
	}}

	first := holdRunning(t, l, a, tiers[0].Label, requestID)

	// The completion settles and its tombstone is written, and only then does the
	// acknowledgement fail — so handle returns the acknowledgement's error.
	if err := l.handle(t.Context(), delivery); err == nil {
		t.Fatal("the acknowledgement was expected to fail, so no tombstone survived")
	}

	assertRecordedResult(t, a, first.ID, "failed")

	// GitHub hands the same request id to a new job, which is running now.
	replacement := holdRunning(t, l, a, tiers[0].Label, requestID)
	if replacement.ID == first.ID {
		t.Fatal("the replacement reused the same lease, so there is nothing to protect")
	}

	if err := l.handle(t.Context(), delivery); err == nil {
		t.Fatal("the redelivery's acknowledgement was expected to fail too")
	}

	got, err := a.RecordedJobResult(t.Context(), replacement.ID)
	if err != nil {
		t.Fatalf("RecordedJobResult: %v", err)
	}

	if got != "" {
		t.Errorf("a redelivered settled completion recorded %q against a job that has not finished",
			got)
	}

	// AND THE REPLACEMENT'S OWN ATTRIBUTION IS STILL ARMED. This is the half that
	// matters: a recorded result is how billet knows GitHub has reported, so a
	// wrong write would silently refuse every later disruption for the job that is
	// actually running — and refuse it in a way no assertion about the result
	// value could see, since the two deliveries carry the same word.
	bindSomewhere(t, a, replacement)

	if err := a.MarkFailure(t.Context(), replacement.ID, replacement.Epoch,
		"ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), replacement.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if !reportsLease(t, a, replacement.ID) {
		t.Error("the replacement's disruption was refused, so its attribution was disarmed")
	}
}

// A RESULT LOST TO A CRASH IS RECOVERED FROM THE ROW THAT SURVIVED IT.
//
// PutPendingCompletion commits before RecordJobResult, so a process that died
// between them left GitHub's conclusion on disk and nothing in job_history. The
// cleanup clock then settles the completion and retires its tombstone, after
// which every redelivery is classified as already handled — so without this the
// diagnostic is gone for good, inside a window where the evidence was still
// there to be read.
func TestARestoredCompletionRecordsTheResultItsCrashLost(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	db := openState(t)

	original := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))

	job := Job{RequestID: 91, RunID: 910, Result: "failed", CompletionID: 6}
	lease := holdRunning(t, original, a, tiers[0].Label, job.RequestID)

	// The delivery is made durable. The crash lands here, before the second write.
	if _, err := db.PutPendingCompletion(t.Context(), state.PendingCompletion{
		Tier: tiers[0].Label, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		LeaseID: lease.ID, LeaseEpoch: lease.Epoch, Outcome: string(alloc.PhaseDone),
		MessageID: job.CompletionID,
	}); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	if got, err := a.RecordedJobResult(t.Context(), lease.ID); err != nil {
		t.Fatalf("RecordedJobResult: %v", err)
	} else if got != "" {
		t.Fatalf("the result was already recorded as %q, so this test proves nothing", got)
	}

	restarted := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))
	if err := restarted.restoreCompletions(t.Context()); err != nil {
		t.Fatalf("restoreCompletions: %v", err)
	}

	if got, err := a.RecordedJobResult(t.Context(), lease.ID); err != nil {
		t.Fatalf("RecordedJobResult after recovery: %v", err)
	} else if got != "failed" {
		t.Errorf("recovery recorded %q, want the result the crash lost", got)
	}
}

// AND A RETIRED ROW STILL CARRIES THE EVIDENCE.
//
// Retired means the TEARDOWN obligation settled, not that the diagnostic was
// written — and this state is reachable: the result write fails transiently,
// teardown then settles and retires the row, and the acknowledgement to GitHub
// fails so the row survives. From that moment every redelivery is classified as
// already handled and refuses to retry the result, so recovery is the last thing
// that will ever see the evidence before an acknowledgement deletes it. Skipping
// retired rows here also leaves the lease attributable: with no recorded result
// and the pending row eventually gone, a later disruption on a lease still in
// teardown is recorded against a job GitHub already reported.
func TestARetiredCompletionStillRecoversItsResult(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	db := openState(t)

	original := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))

	job := Job{RequestID: 93, RunID: 930, Result: "failed", CompletionID: 8}
	lease := holdRunning(t, original, a, tiers[0].Label, job.RequestID)

	if _, err := db.PutPendingCompletion(t.Context(), state.PendingCompletion{
		Tier: tiers[0].Label, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		LeaseID: lease.ID, LeaseEpoch: lease.Epoch, Outcome: string(alloc.PhaseDone),
		MessageID: job.CompletionID,
	}); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	// The result write failed, and teardown then settled and retired the row. It
	// survives because the acknowledgement to GitHub never landed.
	if err := db.RetirePendingCompletion(t.Context(), tiers[0].Label, job.RequestID,
		job.CompletionID); err != nil {
		t.Fatalf("RetirePendingCompletion: %v", err)
	}

	pending, err := db.PendingCompletions(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}

	if len(pending) != 1 || !pending[0].Retired {
		t.Fatalf("the row is %+v, want one retired row — otherwise this test proves nothing",
			pending)
	}

	restarted := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))
	if err := restarted.restoreCompletions(t.Context()); err != nil {
		t.Fatalf("restoreCompletions: %v", err)
	}

	if got, err := a.RecordedJobResult(t.Context(), lease.ID); err != nil {
		t.Fatalf("RecordedJobResult after recovery: %v", err)
	} else if got != "failed" {
		t.Errorf("recovery recorded %q for a retired row, want the result it carried", got)
	}
}

// A COMPLETION FOR A LEASE ONLY THE RUNNER NAME NAMES IS STILL RECORDED.
//
// After a restart the escrow maps are empty, so a completion arriving for a
// request this process never held resolves its lease from the runner's name
// alone. This listener does not hold that lease — `pending_completions.lease_id`
// is correctly empty for it, because teardown is not this process's to finish —
// so recovery has nothing to restore from and `complete` is what has to record.
//
// DRIVEN THROUGH THE PRODUCER PATH. The other recovery tests insert the lease id
// into the pending row by hand, which is exactly the state this case does NOT
// produce; without this one they cover for the gap rather than testing it.
func TestACompletionKnownOnlyByItsRunnerNameStillRecordsItsResult(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	db := openState(t)

	// A lease in the ledger that no listener remembers, the shape a restart
	// leaves behind.
	staged := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))

	const requestID = 96

	lease := holdRunning(t, staged, a, tiers[0].Label, requestID)

	restarted := NewListener(a, tiers[0].Label, &fakeSession{}, WithCompletionStore(db),
		WithRunner(&fakeRunner{}))

	job := Job{RequestID: requestID, RunID: 960, Result: "failed",
		RunnerName: provider.InstanceName(lease.ID)}

	if err := restarted.handle(t.Context(), &Message{MessageID: 9,
		Completed: []Job{job}}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// The producer really did leave the pending row without a lease — otherwise
	// this is testing the ordinary path with extra steps.
	pending, err := db.PendingCompletions(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("PendingCompletions: %v", err)
	}

	for i := range pending {
		if pending[i].RequestID == requestID && pending[i].LeaseID != "" {
			t.Fatalf("the pending row carries lease %q, so the runner-name path was not taken",
				pending[i].LeaseID)
		}
	}

	if got, err := a.RecordedJobResult(t.Context(), lease.ID); err != nil {
		t.Fatalf("RecordedJobResult: %v", err)
	} else if got != "failed" {
		t.Errorf("recorded result = %q, want it recovered from the runner name", got)
	}
}

// bindSomewhere places a lease on whichever registered host will take it, so a
// test about attribution does not have to know the fleet's shape.
func bindSomewhere(t *testing.T, a *alloc.Allocator, lease *alloc.Lease) {
	t.Helper()

	nodes, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}

	for i := range nodes {
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, nodes[i].Name); err != nil {
			continue
		}

		if err := a.Advance(t.Context(), lease.ID, lease.Epoch,
			alloc.PhaseLaunching); err != nil {
			t.Fatalf("Advance: %v", err)
		}

		return
	}

	t.Fatalf("no registered host would take lease %s", lease.ID)
}

// reportsLease asks the operator-facing report whether it names this lease.
func reportsLease(t *testing.T, a *alloc.Allocator, leaseID string) bool {
	t.Helper()

	failures, err := a.AttributedFailures(t.Context(), time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	for i := range failures {
		if failures[i].LeaseID == leaseID {
			return true
		}
	}

	return false
}

// assertRecordedResult checks the durable record rather than anything in memory.
//
// The column is read directly rather than through AttributedFailures, because
// that view deliberately shows only jobs that ALSO carry a disruption — so a
// succeeded job could not be asserted through it at all, and the two directions
// have to be comparable.
func assertRecordedResult(t *testing.T, a *alloc.Allocator, leaseID, want string) {
	t.Helper()

	got, err := a.RecordedJobResult(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("RecordedJobResult: %v", err)
	}

	if got != want {
		t.Errorf("recorded result = %q, want %q", got, want)
	}
}
