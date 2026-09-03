package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// runningLease reserves, binds, assigns and advances one lease to busy on a
// registered host, for tests about what is recorded against a job that was
// running. Assigned rather than merely advanced, so the history row a real
// assignment opens exists and the busy edge can record the job's start on it.
func runningLease(t *testing.T, a *Allocator, label string) *Lease {
	t.Helper()

	lease := launchingLease(t, a, label)
	for _, phase := range []Phase{PhaseOnline, PhaseBusy} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}

	return lease
}

// launchingLease is a lease whose compute is being created and whose job has
// NOT started: GitHub was never seen to place work on it.
func launchingLease(t *testing.T, a *Allocator, label string) *Lease {
	t.Helper()

	lease := reserve(t, a, label)
	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: lease.TargetNode, Provider: lease.Providers[0], VCPU: 8, Memory: 32 * config.GiB,
		Incarnation: "process-one",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("advance to launching: %v", err)
	}

	return lease
}

// A FAILURE REASON DECIDES ITS DISRUPTION BY EXACT VALUE, NEVER BY PREFIX — AND
// THE LEDGER'S OWN EVIDENCE OUTRANKS THE VALUE.
//
// A launch that never ran and a runner found idle are billet's own conclusions
// about compute no job was on, and carry no token: `reclaimed` would attribute
// a failed build to an interruption that never happened. A job destroyed under
// an operator's custody bound WAS running and billet ended it, which is a
// disruption the attribution report must show. A reason that merely LOOKS like
// billet's — the text arrives over the wire from a node — is an external
// party's and keeps the reclaim token a prefix rule would have let it suppress.
// And a reason CLAIMING no job ran, sent for a lease the ledger saw GitHub
// start a job on, is contradicted by the ledger and gets the reclaim token too:
// otherwise a node could suppress the token for any job it held by sending the
// right text.
func TestAFailureReasonBilletWritesCarriesTheDisruptionItMeans(t *testing.T) {
	cases := []struct {
		name    string
		reason  string
		started bool
		want    Disruption
	}{
		{"a launch that never ran", LaunchFailedReason, false, ""},
		{"a runner found idle and retired", RunnerRetiredReason, false, ""},
		{"a job destroyed under the custody bound", HeldPastLimitReason, true, DisruptionHeldPastLimit},
		{"a spot interruption", "ec2 spot interruption: terminate", true, DisruptionReclaimed},
		{"a reason that only looks like billet's", "billet:forged", true, DisruptionReclaimed},
		{"'never ran' claimed for a job the ledger saw start", LaunchFailedReason, true, DisruptionReclaimed},
		{"'retired idle' claimed for a job the ledger saw start", RunnerRetiredReason, true, DisruptionReclaimed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
			a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)

			var lease *Lease
			if tc.started {
				lease = runningLease(t, a, tiers[0].Label)
			} else {
				lease = launchingLease(t, a, tiers[0].Label)
			}

			if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, tc.reason); err != nil {
				t.Fatalf("MarkFailure(%q): %v", tc.reason, err)
			}

			marked, err := a.Lease(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("read lease: %v", err)
			}

			if marked.FailureReason != tc.reason {
				t.Fatalf("reason = %q, want %q", marked.FailureReason, tc.reason)
			}
			if marked.Disruption != tc.want {
				t.Fatalf("disruption for %q = %q, want %q", tc.reason, marked.Disruption, tc.want)
			}
		})
	}
}

// A REASON THAT LANDS BEFORE THE START IT CONTRADICTS IS REPAIRED BY THE START.
//
// GitHub's message stream and the node wire are not ordered, so a node can
// record `launch-failed` after the job has begun and before the control plane
// processes the JobStarted that says so; at that instant the ledger has no
// start and writes no token. The start looks back — on the busy edge, on a
// pooled runner's start, and on a repeated report of either, because the
// reason may land between two reports of the same start.
func TestAReasonSentBeforeTheJobStartIsRepairedByTheStart(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}

	t.Run("on the busy edge", func(t *testing.T) {
		a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
		lease := launchingLease(t, a, tiers[0].Label)

		if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, LaunchFailedReason); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
		if marked, err := a.Lease(t.Context(), lease.ID); err != nil || marked.Disruption != "" {
			t.Fatalf("before the start: disruption %q err=%v, want none yet", marked.Disruption, err)
		}

		for _, phase := range []Phase{PhaseOnline, PhaseBusy} {
			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("advance to %s: %v", phase, err)
			}
		}
		if marked, err := a.Lease(t.Context(), lease.ID); err != nil || marked.Disruption != DisruptionReclaimed {
			t.Fatalf("after the start: disruption %q err=%v, want %q", marked.Disruption, err, DisruptionReclaimed)
		}
	})

	t.Run("on a repeated busy report", func(t *testing.T) {
		a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
		lease := runningLease(t, a, tiers[0].Label)

		// A ROW FROM BEFORE THE START WAS RECORDED: a job busy across the upgrade
		// that added started_at has none, so a reason landing now finds no start
		// and writes no token. The next report of the same start is what can
		// repair it, and it must.
		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(),
				`UPDATE job_history SET started_at = NULL WHERE lease_id = $1`, lease.ID)

			return err
		}); err != nil {
			t.Fatalf("clear the recorded start: %v", err)
		}
		if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, RunnerRetiredReason); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
		if marked, err := a.Lease(t.Context(), lease.ID); err != nil || marked.Disruption != "" {
			t.Fatalf("with no recorded start: disruption %q err=%v, want none yet", marked.Disruption, err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseBusy); err != nil {
			t.Fatalf("repeated advance to busy: %v", err)
		}
		if marked, err := a.Lease(t.Context(), lease.ID); err != nil || marked.Disruption != DisruptionReclaimed {
			t.Fatalf("after the repeated report: disruption %q err=%v, want %q", marked.Disruption, err, DisruptionReclaimed)
		}
	})

	t.Run("on a pooled runner's start", func(t *testing.T) {
		a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
		lease := launchingLease(t, a, tiers[0].Label)

		const runnerName = "billet-pooled"
		if err := a.RegisterPoolRunner(t.Context(), PoolRunner{
			LeaseID: lease.ID, Tier: tiers[0].Label, LaunchRequestID: 1, RunnerName: runnerName,
		}); err != nil {
			t.Fatalf("RegisterPoolRunner: %v", err)
		}
		if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, LaunchFailedReason); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
		if _, err := a.StartPoolRunner(t.Context(), lease.ID, tiers[0].Label, 91, runnerName, -1, 5, "job-5"); err != nil {
			t.Fatalf("StartPoolRunner: %v", err)
		}
		if marked, err := a.Lease(t.Context(), lease.ID); err != nil || marked.Disruption != DisruptionReclaimed {
			t.Fatalf("after the pooled start: disruption %q err=%v, want %q", marked.Disruption, err, DisruptionReclaimed)
		}
	})
}

// THE OPERATOR'S REASON IS NOT A NODE'S TO SEND. It is written by the command
// that requests or applies the release; over MarkFailure it could only be a
// node asserting an operator did something, which suppresses the reclaim token
// for a job that ran. Refused before anything is read, like the inventory
// marker.
func TestMarkFailureRefusesTheOperatorsReason(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	lease := runningLease(t, a, tiers[0].Label)

	err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, ForceReleasedReason)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("MarkFailure(%q) = %v, want a refusal naming the reason as reserved",
			ForceReleasedReason, err)
	}

	current, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if current.FailureReason != "" || current.Disruption != "" {
		t.Fatalf("the refused reason was recorded anyway: reason=%q disruption=%q",
			current.FailureReason, current.Disruption)
	}
}

// A CONCLUSIVE FAILURE RELEASES WITH ITS REASON IN ONE TRANSACTION, so the
// archive carries it; a release and a reason written a call apart leave a
// history row that has already closed without one. Idempotent as Release is,
// and an earlier reason stands.
func TestReleaseFailedRecordsItsReasonWithTheRelease(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)

	// A launch that never ran: failed, explained, and no disruption token.
	lease := launchingLease(t, a, tiers[0].Label)
	if err := a.ReleaseFailed(t.Context(), lease.ID, lease.Epoch, LaunchFailedReason); err != nil {
		t.Fatalf("ReleaseFailed: %v", err)
	}
	if outcome, err := a.HistoryOutcome(t.Context(), lease.ID); err != nil || outcome != string(PhaseFailed) {
		t.Fatalf("history outcome = %q err=%v, want failed", outcome, err)
	}
	if reason, err := a.HistoryFailureReason(t.Context(), lease.ID); err != nil || reason != LaunchFailedReason {
		t.Fatalf("history reason = %q err=%v, want %q", reason, err, LaunchFailedReason)
	}
	if token, _ := historyDisruption(t, a, lease.ID); token != "" {
		t.Fatalf("a launch that never ran was archived with disruption %q", token)
	}

	// IDEMPOTENT: the same call again is not an error, and a done lease is
	// refused rather than rewritten.
	if err := a.ReleaseFailed(t.Context(), lease.ID, lease.Epoch, LaunchFailedReason); err != nil {
		t.Fatalf("second ReleaseFailed: %v", err)
	}
	finished := runningLease(t, a, tiers[0].Label)
	if err := a.Release(t.Context(), finished.ID, finished.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release done: %v", err)
	}
	if err := a.ReleaseFailed(t.Context(), finished.ID, finished.Epoch, LaunchFailedReason); !errors.Is(err, ErrConflict) {
		t.Fatalf("ReleaseFailed on a done lease = %v, want ErrConflict", err)
	}

	// AN EARLIER REASON STANDS, and so does its token.
	explained := runningLease(t, a, tiers[0].Label)
	if err := a.MarkFailure(t.Context(), explained.ID, explained.Epoch, "ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if err := a.ReleaseFailed(t.Context(), explained.ID, explained.Epoch, LaunchFailedReason); err != nil {
		t.Fatalf("ReleaseFailed after an earlier reason: %v", err)
	}
	if reason, err := a.HistoryFailureReason(t.Context(), explained.ID); err != nil ||
		reason != "ec2 spot interruption: terminate" {
		t.Fatalf("history reason = %q err=%v, want the earlier reason kept", reason, err)
	}
	if token, _ := historyDisruption(t, a, explained.ID); token != string(DisruptionReclaimed) {
		t.Fatalf("archived disruption = %q, want %q kept", token, DisruptionReclaimed)
	}

	// THE RESERVED REASONS ARE REFUSED HERE TOO.
	other := launchingLease(t, a, tiers[0].Label)
	if err := a.ReleaseFailed(t.Context(), other.ID, other.Epoch, ForceReleasedReason); err == nil {
		t.Fatal("ReleaseFailed accepted the operator's reserved reason")
	}
	if _, err := a.Lease(t.Context(), other.ID); err != nil {
		t.Fatalf("a refused ReleaseFailed still released the lease: %v", err)
	}
}

// A FORCED RELEASE SAYS SO IN THE HISTORY, on both of its paths.
//
// Both archive the lease as failed; without a reason the history carries a
// failure nothing explains, and an operator reading it later has no way to
// learn that an operator caused it. An earlier reason stands: it explains the
// failure better than the release that followed it.
func TestAForcedReleaseRecordsTheOperatorsAssertion(t *testing.T) {
	now := time.Now().UTC()
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers,
		WithClock(func() time.Time { return now }), WithLeaseTTL(30*time.Second))

	// A QUARANTINED LEASE IS RELEASED OUTRIGHT, carrying the assertion.
	quarantined := runningLease(t, a, tiers[0].Label)
	now = now.Add(31 * time.Second)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	result, err := a.ForceRelease(t.Context(), quarantined.ID)
	if err != nil || result.Pending {
		t.Fatalf("force-release quarantined = %+v err=%v, want released", result, err)
	}
	if got, err := a.HistoryFailureReason(t.Context(), quarantined.ID); err != nil ||
		got != ForceReleasedReason {
		t.Fatalf("quarantined force reason = %q err=%v, want %q", got, err, ForceReleasedReason)
	}

	// A HELD LEASE IS ASKED OF ITS HOLDER, and the assertion is recorded with
	// the request, so the holder's own failed release is explained.
	held := runningLease(t, a, tiers[0].Label)
	if err := a.Advance(t.Context(), held.ID, held.Epoch, PhaseTeardown); err != nil {
		t.Fatalf("advance to teardown: %v", err)
	}
	result, err = a.ForceRelease(t.Context(), held.ID)
	if err != nil || !result.Pending {
		t.Fatalf("force-release held = %+v err=%v, want pending", result, err)
	}
	current, err := a.Lease(t.Context(), held.ID)
	if err != nil {
		t.Fatalf("read held lease: %v", err)
	}
	if !current.ForceRelease || current.FailureReason != ForceReleasedReason {
		t.Fatalf("held lease after force = force=%v reason=%q, want requested with %q",
			current.ForceRelease, current.FailureReason, ForceReleasedReason)
	}

	// AN EARLIER REASON STANDS.
	explained := runningLease(t, a, tiers[0].Label)
	if err := a.MarkFailure(t.Context(), explained.ID, explained.Epoch, "ec2 spot interruption: terminate"); err != nil {
		t.Fatalf("mark failure: %v", err)
	}
	if err := a.Advance(t.Context(), explained.ID, explained.Epoch, PhaseTeardown); err != nil {
		t.Fatalf("advance to teardown: %v", err)
	}
	if _, err := a.ForceRelease(t.Context(), explained.ID); err != nil {
		t.Fatalf("force-release explained: %v", err)
	}
	current, err = a.Lease(t.Context(), explained.ID)
	if err != nil {
		t.Fatalf("read explained lease: %v", err)
	}
	if current.FailureReason != "ec2 spot interruption: terminate" {
		t.Fatalf("a forced release replaced an earlier reason with %q", current.FailureReason)
	}
}

// A POOLED RUNNER'S JOB START IS RECORDED ON THE POOL EDGE. A pool member's
// lease stays where the launch left it while GitHub binds work to the physical
// runner, so the evidence a job ran cannot come from a lease phase there — and
// measured on a real deployment, the lease of a pooled CodeBuild runner sat in
// `launching` for the whole job. A reason claiming the launch never ran is
// contradicted all the same.
func TestAPooledRunnersJobStartIsRecorded(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers)
	lease := launchingLease(t, a, tiers[0].Label)

	const runnerName = "billet-pooled"
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{
		LeaseID: lease.ID, Tier: tiers[0].Label, LaunchRequestID: 1, RunnerName: runnerName,
	}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, tiers[0].Label, 91, runnerName, -1, 5, "job-5"); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}

	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch, LaunchFailedReason); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	marked, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if marked.Disruption != DisruptionReclaimed {
		t.Fatalf("disruption = %q, want %q: the ledger saw a pooled runner start a job on this lease",
			marked.Disruption, DisruptionReclaimed)
	}
}
