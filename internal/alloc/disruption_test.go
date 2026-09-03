package alloc

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// leaseDisruption reads what the LEDGER records against a lease, not what an
// in-memory Lease happens to carry. A struct field that feeds a write is an
// input; only the row is a record.
func leaseDisruption(t *testing.T, a *Allocator, leaseID string) (string, string) {
	t.Helper()

	var token, at string

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT disruption, disrupted_at FROM leases WHERE id = $1`, leaseID).
		Scan(&token, &at); err != nil {
		t.Fatalf("read the disruption of lease %s: %v", leaseID, err)
	}

	return token, at
}

func historyDisruption(t *testing.T, a *Allocator, leaseID string) (string, string) {
	t.Helper()

	var token, at string

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT disruption, disrupted_at FROM job_history WHERE lease_id = $1`, leaseID).
		Scan(&token, &at); err != nil {
		t.Fatalf("read the archived disruption of lease %s: %v", leaseID, err)
	}

	return token, at
}

func historyResult(t *testing.T, a *Allocator, leaseID string) string {
	t.Helper()

	var result string

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT result FROM job_history WHERE lease_id = $1`, leaseID).Scan(&result); err != nil {
		t.Fatalf("read the recorded result of lease %s: %v", leaseID, err)
	}

	return result
}

func historyResultAt(t *testing.T, a *Allocator, leaseID string) string {
	t.Helper()

	var at string

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT result_at FROM job_history WHERE lease_id = $1`, leaseID).Scan(&at); err != nil {
		t.Fatalf("read when the result of lease %s was recorded: %v", leaseID, err)
	}

	return at
}

func historyRunID(t *testing.T, a *Allocator, leaseID string) int64 {
	t.Helper()

	var runID int64

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT COALESCE(run_id, 0) FROM job_history WHERE lease_id = $1`, leaseID).
		Scan(&runID); err != nil {
		t.Fatalf("read the recorded run of lease %s: %v", leaseID, err)
	}

	return runID
}

// nodeLive reports what the ledger believes about a host, so a test can tell
// "NodeGone did nothing" from "NodeGone did half of it".
func nodeLive(t *testing.T, a *Allocator, name string) bool {
	t.Helper()

	var live int

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT live FROM nodes WHERE name = $1`, name).Scan(&live); err != nil {
		t.Fatalf("read the liveness of node %s: %v", name, err)
	}

	return live == 1
}

// A HOST THAT VANISHES MID-JOB IS THE CASE THE DISRUPTION RECORD EXISTS FOR,
// and this is the only moment it can be recorded. The lease does not expire —
// the control plane goes on renewing it — so it is never quarantined and no
// inventory ever reports its guest absent. Once the host re-registers, `live`
// goes back to 1 and nothing anywhere remembers that it was ever gone.
func TestANodeGoingSilentMarksTheJobsItWasRunning(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	token, at := leaseDisruption(t, a, lease.ID)
	if token != string(DisruptionNodeForgotten) {
		t.Errorf("lease disruption = %q, want %q", token, DisruptionNodeForgotten)
	}

	if at != ts(now) {
		t.Errorf("disrupted_at = %q, want the moment it was observed, %q", at, ts(now))
	}

	if nodeLive(t, a, "epyc-1") {
		t.Error("NodeGone left the host marked live")
	}
}

// AND A CONTROL PLANE THAT HAS JUST STARTED HAS OBSERVED NOTHING.
//
// ForgetEveryNode marks the whole fleet unreachable because liveness is the
// plane's judgement and a plane that has just started has not formed one. That
// is the opposite of an observation, and recording disruptions there would
// attribute every failure replayed across a restart to a host that was fine —
// which is precisely the trap that ruled out deriving the verdict at completion
// time from a snapshot of `nodes.live`.
func TestForgettingTheWholeFleetRecordsNoDisruption(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.ForgetEveryNode(t.Context()); err != nil {
		t.Fatalf("ForgetEveryNode: %v", err)
	}

	if nodeLive(t, a, "epyc-1") {
		t.Fatal("ForgetEveryNode did not mark the host unreachable, so this proves nothing")
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != "" {
		t.Errorf("a control plane starting up recorded %q against a healthy job", token)
	}
}

// A SUPERSEDED INCARNATION MUST NOT MARK ITS REPLACEMENT'S WORK. Registration
// commits to the ledger before the plane's expiry runs, so a host that restarts
// quickly can be given up on by the expiry of the incarnation it replaced.
func TestANodeGoneFromASupersededIncarnationMarksNothing(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	stale := nodeEpoch(t, a)

	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 4, Memory: 16 * config.GiB})

	if current := nodeEpoch(t, a); current == stale {
		t.Fatal("re-registering did not move the epoch, so there is no fence to test")
	}

	if err := a.NodeGone(t.Context(), "epyc-1", stale); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != "" {
		t.Errorf("a superseded incarnation recorded %q against the replacement's job", token)
	}

	if !nodeLive(t, a, "epyc-1") {
		t.Error("a superseded incarnation marked the replacement unreachable")
	}
}

// A DISRUPTION THAT LANDS AFTER THE JOB ENDED CANNOT HAVE CAUSED IT.
//
// An ordinary EC2 teardown outlives the completion by minutes, and a host
// forgotten during that window would otherwise be reported to the author of a
// perfectly ordinary test failure as billet's fault.
func TestALeaseWhoseJobGitHubAlreadyReportedIsNotMarkedDisrupted(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != "" {
		t.Errorf("recorded %q against a job github had already reported", token)
	}
}

// ESCROW NOBODY LAUNCHED RAN NO BUILD. Its capacity comes back through the
// ordinary failed-launch path and GitHub reassigns; attributing a build failure
// to it would be attributing one to a machine that never ran the build.
func TestAnUnlaunchedEscrowLeaseIsNotMarkedDisrupted(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if lease.TargetNode != "epyc-1" {
		t.Fatalf("the reservation targeted %q, so NodeGone would miss it anyway", lease.TargetNode)
	}

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != "" {
		t.Errorf("recorded %q against escrow that never launched anything", token)
	}
}

// AN INVENTORY THAT DOES NOT CONTAIN THE GUEST IS THE STRONGEST THING A HOST CAN
// SAY. It is recorded before the row is terminalized, because a terminal lease
// is past the phases in which a job could still have been running.
func TestInventoryAbsenceRecordsGuestAbsent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	now = now.Add(2 * quarantineGrace)

	resolved, err := a.ResolveQuarantineFor(t.Context(), "epyc-1",
		[]string{"some-other-lease"}, nodeEpoch(t, a))
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 1 {
		t.Fatalf("resolved %d leases, want 1", resolved)
	}

	if token, _ := historyDisruption(t, a, lease.ID); token != string(DisruptionGuestAbsent) {
		t.Errorf("archived disruption = %q, want %q", token, DisruptionGuestAbsent)
	}
}

// AND CORRECTING THE PROVISIONAL VERDICT DOES NOT UNDO IT. GitHub's own result
// overrides billet's guess about how the job ENDED — it says nothing about
// whether the guest vanished, which is what was actually observed.
func TestCorrectingAProvisionalInventoryVerdictKeepsTheDisruption(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	now = now.Add(2 * quarantineGrace)

	if _, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", nil, nodeEpoch(t, a)); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	settled, err := a.SettleCompletionOnTerminalLease(t.Context(), lease.ID, lease.Epoch, PhaseDone)
	if err != nil {
		t.Fatalf("SettleCompletionOnTerminalLease: %v", err)
	}

	if !settled {
		t.Fatal("the completion did not settle the lease, so nothing was corrected")
	}

	// The correction landed: the provisional marker is gone and the conclusion is
	// GitHub's. Without this the assertion below would pass on a no-op.
	reason, err := a.HistoryFailureReason(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryFailureReason: %v", err)
	}

	if reason != "" {
		t.Fatalf("the provisional marker survived as %q, so no correction happened", reason)
	}

	if token, _ := historyDisruption(t, a, lease.ID); token != string(DisruptionGuestAbsent) {
		t.Errorf("correcting the verdict erased the disruption: got %q, want %q",
			token, DisruptionGuestAbsent)
	}
}

// AN EXTERNAL RECLAIM IS RECORDED IN BILLET'S OWN WORDS AS WELL AS THE NODE'S.
// The free-form reason is text a node supplied; the token is what a report acts
// on, and one without the other is invisible to `billet leases failures`.
func TestASpotInterruptionRecordsReclaimed(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch,
		"ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != string(DisruptionReclaimed) {
		t.Errorf("lease disruption = %q, want %q", token, DisruptionReclaimed)
	}
}

// THE FIRST OBSERVATION IS THE ONE KEPT. A later disruption is a consequence of
// the earlier one at least as often as it is a separate event, and only the
// earliest can still have been causal.
func TestOnlyTheFirstDisruptionIsRecorded(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch,
		"ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	now = now.Add(time.Minute)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != string(DisruptionReclaimed) {
		t.Errorf("the later observation overwrote the first: got %q, want %q",
			token, DisruptionReclaimed)
	}
}

// ARCHIVING NEVER ERASES A DISRUPTION, the same rule node/run_id/request_id
// already follow. Not every caller of archive loads the columns, and one that
// does not must not wipe an observation an earlier one recorded.
func TestArchivingNeverErasesADisruption(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	token, at := historyDisruption(t, a, lease.ID)
	if token != string(DisruptionNodeForgotten) || at == "" {
		t.Fatalf("archive carried disruption %q at %q, want %q with a time",
			token, at, DisruptionNodeForgotten)
	}

	// A SECOND ARCHIVE FROM A LEASE THAT CARRIES NOTHING. Release is idempotent
	// on the same outcome, so archive is reached again with a value assembled by
	// hand — which is what a caller that did not select the columns looks like.
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		return a.archive(t.Context(), tx, &Lease{ID: lease.ID, Tier: lease.Tier}, PhaseDone)
	}); err != nil {
		t.Fatalf("second archive: %v", err)
	}

	if got, gotAt := historyDisruption(t, a, lease.ID); got != token || gotAt != at {
		t.Errorf("a second archive erased the disruption: got %q at %q, want %q at %q",
			got, gotAt, token, at)
	}
}

// A JOB THAT SUCCEEDED IS NOT AN ATTRIBUTED FAILURE, however disrupted its lease
// was. This is what makes the pair of facts a report rather than an accusation:
// a host can go quiet during a build that finishes green, and it often does.
func TestAttributedFailuresExcludesASucceededJobOnADisruptedLease(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "succeeded", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The disruption really is on the record, so an empty report below is the
	// result filtering rather than nothing having been written.
	if token, _ := historyDisruption(t, a, lease.ID); token != string(DisruptionNodeForgotten) {
		t.Fatalf("archived disruption = %q, so this test proves nothing", token)
	}

	failures, err := a.AttributedFailures(t.Context(), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 0 {
		t.Errorf("reported %d failures for a job github said succeeded: %+v",
			len(failures), failures)
	}
}

// AND A FAILED ONE IS REPORTED IN FULL, with both facts and the free-form detail
// the node supplied beside the reclaim.
func TestAttributedFailuresReportsBothFacts(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch,
		"ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	failures, err := a.AttributedFailures(t.Context(), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 1 {
		t.Fatalf("reported %d failures, want 1: %+v", len(failures), failures)
	}

	got := failures[0]
	switch {
	case got.LeaseID != lease.ID:
		t.Errorf("lease = %q, want %q", got.LeaseID, lease.ID)
	case got.Node != "epyc-1":
		t.Errorf("node = %q, want epyc-1", got.Node)
	case got.Result != "failed":
		t.Errorf("result = %q, want failed", got.Result)
	case got.Disruption != DisruptionReclaimed:
		t.Errorf("disruption = %q, want %q", got.Disruption, DisruptionReclaimed)
	case got.Detail != "ec2 spot interruption: terminate":
		t.Errorf("detail = %q, want the reason the node gave", got.Detail)
	case got.RunID != 1:
		t.Errorf("run = %d, want 1", got.RunID)
	}

	// OUTSIDE THE WINDOW IS OUTSIDE THE REPORT.
	later, err := a.AttributedFailures(t.Context(), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures beyond the window: %v", err)
	}

	if len(later) != 0 {
		t.Errorf("a window starting after the job reported %d rows", len(later))
	}
}

// BILLET'S OWN BOOKKEEPING SENTINEL IS NOT AN EXPLANATION. It means "inventory
// inferred this from an absence", which the disruption token already says
// better, and printing it into an operator's report would be printing an
// internal marker at them.
func TestTheProvisionalInventoryMarkerIsNotReportedAsDetail(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	now = now.Add(2 * quarantineGrace)

	if _, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", nil, nodeEpoch(t, a)); err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	failures, err := a.AttributedFailures(t.Context(), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 1 {
		t.Fatalf("reported %d failures, want 1", len(failures))
	}

	if failures[0].Detail != "" {
		t.Errorf("detail = %q, want the provisional marker stripped", failures[0].Detail)
	}
}

// A RESULT BILLET DOES NOT RECOGNISE IS NOT A SUCCESS. The vendored client
// enumerates none of them, so an unknown value is "could not tell" — and
// collapsing that into "fine" hides exactly the job an operator came looking
// for.
func TestAnUnrecognisedResultIsTreatedAsNotSucceeded(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "abandonedSomehow", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	failures, err := a.AttributedFailures(t.Context(), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 1 || failures[0].Result != "abandonedSomehow" {
		t.Errorf("an unrecognised result produced %+v, want it reported verbatim", failures)
	}
}

// RecordJobResult IS UNFENCED, like MarkDeregistered, because GitHub's
// conclusion is monotonic and says nothing about who holds the lease. A reap
// that quarantined the row in between does not make the job unfinished.
func TestARecordedResultSurvivesAReapBumpingTheEpoch(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult after a reap: %v", err)
	}

	if got := historyResult(t, a, lease.ID); got != "failed" {
		t.Errorf("recorded result = %q, want failed", got)
	}
}

// A LEASE THAT NEVER RAN A JOB HAS NOTHING TO RECORD, and that is success rather
// than an error: a promise GitHub cancelled before assigning it has no history
// row, and a completion for it must not stop the listener.
func TestRecordingAResultForALeaseWithNoHistoryIsHarmless(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	if err := a.RecordJobResult(t.Context(), "never-assigned", "failed", 0); err != nil {
		t.Errorf("RecordJobResult on an unassigned lease = %v, want nil", err)
	}
}

// AN EMPTY RESULT IS A CALLER MISTAKE, not a value. Storing it would make
// "GitHub said nothing" indistinguishable from "GitHub has not said yet", which
// is the fact the disruption guard reads.
func TestAnEmptyResultIsRefused(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	err := a.RecordJobResult(t.Context(), "any", "   ", 0)
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("RecordJobResult with a blank result = %v, want a refusal", err)
	}
}

// A REDELIVERY MUST NOT MOVE THE TIME GITHUB REPORTED THE JOB.
//
// GitHub redelivers an unacknowledged message, and `--since` windows on
// result_at. Re-writing it would drag a days-old job back into every report for
// as long as its teardown kept retrying, so the operator reading "in the last
// 24h" would be reading a lie about when it happened.
func TestARedeliveredResultDoesNotMoveItsRecordedTime(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	first := historyResultAt(t, a, lease.ID)
	if first == "" {
		t.Fatal("the first record wrote no timestamp, so this test proves nothing")
	}

	now = now.Add(48 * time.Hour)

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult on a redelivery: %v", err)
	}

	if got := historyResultAt(t, a, lease.ID); got != first {
		t.Errorf("a redelivery moved result_at from %q to %q", first, got)
	}
}

// AND A CONTRADICTION IS REFUSED RATHER THAN ALLOWED TO REPLACE THE FIRST WORD.
// A lease runs exactly one job, so two different results for it means one of
// them is wrong — and the earlier one is the one GitHub actually said first.
func TestAContradictoryResultIsRefused(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	err := a.RecordJobResult(t.Context(), lease.ID, "succeeded", 0)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("recording a contradictory result = %v, want ErrConflict", err)
	}

	if got := historyResult(t, a, lease.ID); got != "failed" {
		t.Errorf("the recorded result became %q; the first word must stand", got)
	}
}

// A RESULT IS STORED VERBATIM, AND PADDING IS NOT SILENTLY REMOVED.
//
// Trimming would decide the report: `" succeeded "` normalised to `"succeeded"`
// disappears from it. The one thing this column must never do is turn a value
// billet does not recognise into one it does — an unknown result fails OPEN into
// the report, quoted, where a person can see what arrived.
func TestAPaddedResultIsStoredVerbatimAndStillReported(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, " succeeded ", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if got := historyResult(t, a, lease.ID); got != " succeeded " {
		t.Errorf("recorded result = %q, want the bytes github sent", got)
	}

	failures, err := a.AttributedFailures(t.Context(), now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("AttributedFailures: %v", err)
	}

	if len(failures) != 1 {
		t.Errorf("a result billet does not recognise produced %d rows, want it reported",
			len(failures))
	}
}

// GITHUB HAVING REPORTED IS RECORDED IN TWO PLACES, AND EITHER IS ENOUGH.
//
// The listener makes the delivery durable in `pending_completions` and writes
// job_history.result in a SECOND transaction. Reading only the second leaves a
// committed interval in which a concurrent NodeGone takes the next BEGIN
// IMMEDIATE and attributes a job that had already finished — and leaves a
// control plane that crashed between the two writes permanently attributable.
func TestADisruptionIsRefusedWhileACompletionIsPendingForThatLease(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if _, err := a.db.PutPendingCompletion(t.Context(), state.PendingCompletion{
		Tier: "small", RequestID: 1, RunID: 1, Result: "failed",
		LeaseID: lease.ID, LeaseEpoch: lease.Epoch, Outcome: string(PhaseDone), MessageID: 4,
	}); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	// Nothing has reached job_history yet — the second write is exactly what the
	// crash window loses — so only the pending row can refuse this.
	if got := historyResult(t, a, lease.ID); got != "" {
		t.Fatalf("the result reached job_history, so this test proves nothing: %q", got)
	}

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != "" {
		t.Errorf("recorded %q against a job whose completion was already durable", token)
	}
}

// AND A PENDING COMPLETION FOR A DIFFERENT LEASE REFUSES NOTHING. The clause is
// keyed on the lease, so a busy deployment with completions in flight elsewhere
// must still be able to attribute the job that was actually disrupted.
func TestAPendingCompletionForAnotherLeaseDoesNotRefuseADisruption(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if _, err := a.db.PutPendingCompletion(t.Context(), state.PendingCompletion{
		Tier: "small", RequestID: 999, RunID: 999, Result: "failed",
		LeaseID: "some-other-lease", Outcome: string(PhaseDone), MessageID: 4,
	}); err != nil {
		t.Fatalf("PutPendingCompletion: %v", err)
	}

	if err := a.NodeGone(t.Context(), "epyc-1", nodeEpoch(t, a)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if token, _ := leaseDisruption(t, a, lease.ID); token != string(DisruptionNodeForgotten) {
		t.Errorf("another lease's completion refused this disruption: got %q", token)
	}
}

// THE GUARD'S SUBQUERY HAS TO USE ITS INDEX, and that is asserted rather than
// assumed.
//
// markNodeDisruptedTx evaluates this correlated NOT EXISTS once per candidate
// lease, inside the single SQLite writer, on the path where a host is given up
// on and every lease it holds is considered at once. Without an index it is a
// full scan of pending_completions each time, against a table failed
// acknowledgements leave rows in.
//
// MEASURED, AND THE FIRST VERSION FAILED IT. The index was written partial
// (excluding the empty lease id), which reads like the tighter choice — and SQLite
// takes a partial index only when the query's own WHERE syntactically implies
// the index's, which `p.lease_id = leases.id` does not. EXPLAIN QUERY PLAN said
// `SCAN p`: the index was decoration answering a cost problem that was still
// there. A plan is the only thing that can tell those apart, so the plan is what
// this reads.
func TestTheDisruptionGuardsSubqueryUsesItsIndex(t *testing.T) {
	// EXPLAIN QUERY PLAN, and the vocabulary of its answer, are SQLite's.
	// PostgreSQL has its own planner and its own output, so the same property
	// there is a test written against EXPLAIN rather than this one made portable.
	skipOnPostgres(t, "EXPLAIN QUERY PLAN and the SEARCH/USING INDEX vocabulary are SQLite's")

	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	// THE PRODUCTION STATEMENTS THEMSELVES, not copies assembled here. A copy
	// would let a change to either one stop using the index with this still green.
	//
	// READ OUT OF THE GENERATED PACKAGE, which is what actually executes: the
	// constants are unexported and internal/alloc could not name them even if it
	// imported it, so the text is parsed from the file sqlc wrote.
	for name, query := range map[string]string{
		"one lease": generatedQuery(t, "disruptableLease"),
		"one host":  generatedQuery(t, "disruptableLeasesOnNode"),
	} {
		plan := queryPlan(t, a, query, "anything")

		// SEARCH rather than SCAN, on the alias the subquery uses, naming the
		// index — the three properties together. Matched as properties rather than
		// as one literal because SQLite says "USING COVERING INDEX" when it can
		// answer from the index alone, which is this plan getting BETTER; an
		// assertion on the exact wording would fail on the improvement.
		searched := false

		for _, step := range strings.Split(plan, "\n") {
			if strings.HasPrefix(step, "SEARCH p ") &&
				strings.Contains(step, "pending_completions_lease_idx") {
				searched = true
			}
		}

		if !searched {
			t.Errorf("%s: the pending-completions subquery scans rather than searching its index:\n%s",
				name, plan)
		}
	}
}

// BOTH DISRUPTION STATEMENTS CARRY THE SAME GUARD.
//
// SQL cannot compose a predicate, so the guard is written out twice -- once in
// each statement -- where the Go constant it replaced was written once and
// concatenated. That is a second source of truth, and the direction that hurts is
// silent: a clause dropped from the by-host statement alone would attribute
// somebody's ordinary test failure to billet on the one path where a whole host
// is being given up at once, and every existing test would stay green because the
// by-lease path still refuses.
//
// NOTHING IS EXTRACTED: EACH WHOLE STATEMENT IS COMPARED. Three narrower forms
// came first, each broken in one edit, and all in the same way -- a fragment says
// nothing about what surrounds it.
//
// Comparing the two statements' guards against EACH OTHER, from `disruption`
// onward, left out the operator ATTACHING the guard: changing the by-host
// statement's `AND` to an `OR` left both suffixes identical and the test green,
// while SQLite's precedence turned the predicate into `node = @node OR (the whole
// guard)` -- every lease on the named host whatever its phase or its recorded
// completion, plus every disruptable lease on every other host. Measured on this
// tree before the fix. Including the operator fixed that one edit and not the
// class: appending `OR 1=1` to the LOCATOR does the same thing and leaves the
// guards identical again. Comparing the whole extracted WHERE fixed THAT and left
// the extraction itself, which these statements defeat by construction -- their
// guard is two NOT EXISTS subqueries, each with a WHERE of its own, so a bound
// that starts at the first WHERE reads a subquery's predicate, and one that stops
// at ORDER BY, LIMIT or a semicolon stops early the moment a subquery carries one.
//
// So the comparison is the entire statement, normalised for layout only. A
// changed projection, a filtering JOIN and a trailing LIMIT all fail here too,
// which is the point: none of them appears in a WHERE at all.

func TestBothDisruptionStatementsPinTheirWholeStatement(t *testing.T) {
	// THE GUARD, WRITTEN ONCE HERE, is what both statements must carry -- and it
	// is compared as part of a WHOLE STATEMENT rather than on its own.
	//
	// Comparing only the two guards against EACH OTHER was the first version, and
	// a review broke it in one edit: appending `OR 1=1` to the by-host locator
	// leaves both extracted guards identical while SQLite's precedence turns the
	// predicate into `node = $1 OR (guard)` -- every lease in the deployment
	// disruptable by any host that gives up on one. Comparing the extracted WHERE
	// fixed that and left the EXTRACTION as the way in, since these statements
	// already contain subqueries with WHERE clauses of their own. So nothing is
	// extracted: the comparison is the entire statement.
	const guard = "AND disruption = '' " +
		"AND phase IN ('launching','online','busy','custody','teardown','quarantine') " +
		"AND NOT EXISTS (SELECT 1 FROM job_history h " +
		"WHERE h.lease_id = leases.id AND h.result != '') " +
		"AND NOT EXISTS (SELECT 1 FROM pending_completions p " +
		"WHERE p.lease_id = leases.id AND p.result != '')"

	// COALESCE(node, target_node) IS THE HOST ATTRIBUTION and is pinned as such:
	// narrowed to `node` alone, a reservation aimed at a machine being given up on
	// records no disruption, and the operator reading that job's failure is told
	// billet did nothing to it.
	want := map[string]string{
		"disruptableLease": "SELECT id FROM leases WHERE id = $1 " + guard,
		"disruptableLeasesOnNode": "SELECT id FROM leases " +
			"WHERE COALESCE(node, target_node) = CAST($1 AS TEXT) " + guard,
	}

	for name, want := range want {
		got := generatedQuery(t, name)

		// THE `-- name:` HEADER SQLC PREPENDS is the one thing dropped, because it
		// is a comment carrying the query's own name and says nothing about what
		// the statement does.
		//
		// ANCHORED TO THE EXACT HEADER, not cut at the first `:many`. Cutting at a
		// token wherever it appears discards everything before it, so a statement
		// that stopped being :many and later contained that token in a comment or
		// a literal would have its whole meaning-carrying prefix thrown away and
		// the remainder compared -- the opposite of the guarantee this is here to
		// make. Requiring the header as a PREFIX means a changed cardinality, a
		// renamed query or a reordered file all fail loudly.
		header := "-- name: " + strings.ToUpper(name[:1]) + name[1:] + " :many\n"
		if !strings.HasPrefix(got, header) {
			// REPORTED RATHER THAN FATAL, so the second statement is still compared:
			// these two exist to be read against each other, and stopping at the
			// first hides whether the other drifted the same way or differently.
			t.Errorf("%s does not begin with %q. If sqlc's header changed or the query "+
				"is no longer :many, update this pin deliberately:\n%s", name, header, got)

			continue
		}

		got = strings.TrimPrefix(got, header)

		// THE SAME NORMALISER THE PHASE PINS USE, which is quote-aware: `'a, b'`
		// and `'a,b'` are different values, and a pin that maps them to one has
		// stopped pinning the statement.
		got, want := normalizeSQL(got), normalizeSQL(want)

		if got != want {
			t.Errorf("%s is no longer the statement this test pins:\n  is:   %s\n  want: %s",
				name, got, want)
		}
	}
}

// generatedQuery returns the SQL sqlc compiled for one query constant.
//
// PARSED FROM THE GENERATED FILE rather than reconstructed, so a test asserting
// something about a statement is asserting it about the bytes that run. The
// constants are unexported in another package, which is why this reads the source
// instead of referencing them.
func generatedQuery(t *testing.T, name string) string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("..", "state", "ledgerdb", "disruption.sql.go"))
	if err != nil {
		t.Fatalf("read the generated disruption queries: %v", err)
	}

	opener := []byte("const " + name + " = `")

	i := bytes.Index(src, opener)
	if i < 0 {
		t.Fatalf("the generated package has no constant %s; if the query was renamed, "+
			"rename it here too rather than deleting the assertion", name)
	}

	rest := src[i+len(opener):]

	j := bytes.IndexByte(rest, '`')
	if j < 0 {
		t.Fatalf("the constant %s is not terminated", name)
	}

	return string(rest[:j])
}

// queryPlan returns SQLite's own plan for a statement, one step per line.
func queryPlan(t *testing.T, a *Allocator, query string, arg any) string {
	t.Helper()

	rows, err := a.db.Reader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, arg)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}

	defer func() { _ = rows.Close() }()

	var plan strings.Builder

	for rows.Next() {
		var id, parent, notUsed int

		var detail string

		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan a plan step: %v", err)
		}

		plan.WriteString(detail)
		plan.WriteString("\n")
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read the plan: %v", err)
	}

	if plan.Len() == 0 {
		t.Fatal("sqlite returned no plan, so nothing below can fail")
	}

	return plan.String()
}

// THE VOCABULARY IS CLOSED IN GO, because the schema does not close it. A token
// nothing can render is worse than a refused write.
// A POOLED JOB'S WORKFLOW RUN COMES FROM ITS COMPLETION, because nothing knew it
// earlier.
//
// A pooled runner is launched before GitHub chooses its job, so assignPoolSlot
// records run 0 and the lease's own launch request id. Without this the report
// names no run at all for exactly the tiers that use a warm pool — and a run is
// the one thing an operator needs to find the build and decide whether to re-run
// it.
func TestARecordedResultFillsInAPooledJobsWorkflowRun(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The pool's own shape: a launch request id, and no run yet.
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 55); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 4242); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if got := historyRunID(t, a, lease.ID); got != 4242 {
		t.Errorf("recorded run = %d, want the run the completion named", got)
	}
}

// AND A REDELIVERY CARRYING THE SAME RESULT STILL BACKFILLS THE RUN.
//
// The two are lost in different places: a crash between the result write and the
// run write leaves the result recorded and the run absent, and recovery
// redelivers the SAME result. Returning early on the repeat would make the row
// that most needs backfilling the one that never gets it.
func TestARepeatedResultStillFillsInAMissingWorkflowRun(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 55); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// The crash: the result landed, the run did not.
	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if got := historyRunID(t, a, lease.ID); got != 0 {
		t.Fatalf("the run was already recorded as %d, so this test proves nothing", got)
	}

	// Recovery redelivers the same result, this time with the run beside it.
	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 4242); err != nil {
		t.Fatalf("RecordJobResult on recovery: %v", err)
	}

	if got := historyRunID(t, a, lease.ID); got != 4242 {
		t.Errorf("recovery left the run as %d, want it filled in", got)
	}
}

// AND A RUN THE LEDGER ALREADY HAS IS NEVER REPLACED. An ordinary lease is
// assigned with its own run, and a completion arriving for a pool member that
// GitHub swapped must not rewrite another job's history.
func TestARecordedResultNeverReplacesAKnownWorkflowRun(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 111, 55); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, "failed", 4242); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	if got := historyRunID(t, a, lease.ID); got != 111 {
		t.Errorf("recorded run = %d, want the run this lease was assigned", got)
	}
}

// AND THE REFUSAL IS ON THE WRITE PATH, not just in Valid(). The schema carries no
// CHECK on purpose, so this guard is the ONLY enforcement there is — and a test
// that exercises Valid() alone stays green with it deleted.
func TestWritingADisruptionTokenBilletDoesNotKnowIsRefused(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, markErr := markLeaseDisruptedTx(t.Context(), tx, lease.ID, Disruption("invented"), now)

		return markErr
	})
	if err == nil || !strings.Contains(err.Error(), "not a disruption billet records") {
		t.Errorf("writing an invented token = %v, want a refusal naming it", err)
	}

	// AND NOTHING WAS WRITTEN. An error value is the cheapest thing a function
	// produces; what matters is that the ledger is untouched.
	if token, at := leaseDisruption(t, a, lease.ID); token != "" || at != "" {
		t.Errorf("an invented token left %q at %q on the lease", token, at)
	}

	if token, _ := historyDisruption(t, a, lease.ID); token != "" {
		t.Errorf("an invented token reached the history as %q", token)
	}
}

func TestADisruptionTokenBilletDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()

	for _, token := range []Disruption{"", "node_forgotten", "whatever"} {
		if token.Valid() {
			t.Errorf("Disruption(%q).Valid() = true, want false", token)
		}
	}

	for _, token := range []Disruption{
		DisruptionNodeForgotten, DisruptionGuestAbsent, DisruptionReclaimed,
		DisruptionHeldPastLimit,
	} {
		if !token.Valid() {
			t.Errorf("Disruption(%q).Valid() = false, want true", token)
		}
	}
}
