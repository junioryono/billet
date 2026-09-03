package main

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// seedAttributedFailure stages the two facts the failures report pairs: a job
// GitHub reported as failed, on a lease billet's own infrastructure had disrupted.
//
// STAGED THROUGH THE REAL ALLOCATOR, not by hand-inserting rows. A fixture that
// writes the columns itself agrees with whatever the report expects, and would
// stay green if NodeGone stopped recording anything at all.
func seedAttributedFailure(t *testing.T, stateDir, result string, requestID int64) string {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the ledger: %v", err)
		}
	}()

	tier := config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu:24.04",
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, requestID); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if err := a.NodeGone(t.Context(), "epyc-1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if err := a.RecordJobResult(t.Context(), lease.ID, result, 0); err != nil {
		t.Fatalf("RecordJobResult: %v", err)
	}

	return lease.ID
}

// `billet leases failures` HAS TO SAY THAT NOTHING WAS RE-RUN.
//
// That sentence is the deliverable, not decoration. The rule is explicit that
// billet must never re-run a job it believes was lost — a re-run is a side effect
// on somebody's repository — and a report that shows an operator "your build
// failed because the node went away" without saying what billet did NOT do about
// it invites them to assume it was handled.
func TestLeaseFailuresReportsBothFactsAndDisclaimsAnyRerun(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	lease := seedAttributedFailure(t, stateDir, "failed", 77)

	out := capture(t, func() {
		if err := cmdLeases(t.Context(), []string{"failures", "--config", cfg}); err != nil {
			t.Errorf("billet leases failures: %v", err)
		}
	})

	for _, want := range []string{
		// GitHub's half, quoted because it is an upstream string printed into a
		// report an operator reads as billet's own output.
		`"failed"`,
		// billet's half, in words rather than as the stored token.
		"its host stopped answering this control plane",
		// The workflow run, so the operator can find the build on GitHub.
		"4242", "epyc-1",
		// And the lease, so a row can be carried to the other `billet leases`
		// views for a job whose teardown is still wedged on the host.
		lease,
		// The two sentences that keep this a report rather than a verdict.
		"CIRCUMSTANTIAL",
		"Nothing has been re-run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "node-forgotten") {
		t.Errorf("the report printed the raw token instead of explaining it:\n%s", out)
	}
}

// A POOLED LEASE'S INTERNAL REQUEST ID IS NOT SHOWN TO ANYBODY.
//
// A pooled runner is launched before GitHub chooses its job, so its lease is
// assigned a NEGATIVE synthetic request id — billet's own scheduler identity,
// issued by IdentifyPoolSlot. Printing it beside the workflow run that actually
// executed pairs two different identities under headings that read as one, and
// shows an operator "-3" where they expect a GitHub id.
func TestLeaseFailuresDoesNotPrintAPooledLeasesInternalRequestID(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	seedAttributedFailure(t, stateDir, "failed", -3)

	out := capture(t, func() {
		if err := cmdLeases(t.Context(), []string{"failures", "--config", cfg}); err != nil {
			t.Errorf("billet leases failures: %v", err)
		}
	})

	// The row is there — otherwise the absence below is the absence of everything.
	if !strings.Contains(out, "4242") {
		t.Fatalf("the row is missing, so this test proves nothing:\n%s", out)
	}

	if strings.Contains(out, "-3") {
		t.Errorf("the report printed a pooled lease's internal request id:\n%s", out)
	}
}

// A JOB THAT SUCCEEDED IS NOT LISTED, however disrupted its lease was. Without
// this the report is an accusation: hosts go quiet during builds that finish
// green, and often do.
func TestLeaseFailuresOmitsASucceededJob(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	seedAttributedFailure(t, stateDir, "succeeded", 77)

	out := capture(t, func() {
		if err := cmdLeases(t.Context(), []string{"failures", "--config", cfg}); err != nil {
			t.Errorf("billet leases failures: %v", err)
		}
	})

	if !strings.Contains(out, "No job in the last") {
		t.Errorf("a succeeded job was reported as an infrastructure failure:\n%s", out)
	}
}

// AND THE WINDOW IS REAL. A `--since` that excludes the job must exclude it, or
// the flag is decoration and every report is the whole of history.
func TestLeaseFailuresHonoursItsWindow(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	seedAttributedFailure(t, stateDir, "failed", 77)

	inside := capture(t, func() {
		if err := cmdLeases(t.Context(), []string{"failures", "--config", cfg,
			"--since", "1h"}); err != nil {
			t.Errorf("billet leases failures --since 1h: %v", err)
		}
	})

	if strings.Contains(inside, "No job in the last") {
		t.Fatalf("the job is not in a one-hour window, so the narrow one proves nothing:\n%s",
			inside)
	}

	// A nanosecond is shorter than the time between seeding and reading, so the
	// job is genuinely outside it.
	outside := capture(t, func() {
		if err := cmdLeases(t.Context(), []string{"failures", "--config", cfg,
			"--since", "1ns"}); err != nil {
			t.Errorf("billet leases failures --since 1ns: %v", err)
		}
	})

	if !strings.Contains(outside, "No job in the last") {
		t.Errorf("--since did not bound the report:\n%s", outside)
	}

	// AND IT NAMES THE WINDOW IT LOOKED AT. Rounded to the second, a nanosecond
	// reads as "the last 0s" — which says billet looked at no time at all rather
	// than that it looked and found nothing.
	if strings.Contains(outside, "last 0s") {
		t.Errorf("the empty report rounded its window away to nothing:\n%s", outside)
	}
}

// A NONSENSE WINDOW IS REFUSED RATHER THAN NORMALISED. `--since 0` would
// otherwise report nothing and read as "you have no problems", which is the one
// answer this command must never give by accident.
func TestLeaseFailuresRefusesAnImpossibleWindow(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	for _, args := range [][]string{
		{"failures", "--config", cfg, "--since", "0"},
		{"failures", "--config", cfg, "--limit", "0"},
	} {
		err := cmdLeases(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("cmdLeases %v = %v, want a refusal", args, err)
		}
	}
}

// THE VOCABULARY IS CLOSED IN GO, NOT IN THE SCHEMA, so this renderer has to be
// total: a token written by a newer binary must still reach the operator. It is
// quoted rather than dropped, because a row hidden by the report is a row that
// might as well not have been recorded.
func TestAnUnknownDisruptionIsStillRendered(t *testing.T) {
	t.Parallel()

	got := explainDisruption(alloc.Disruption("something-new"), "")
	if got != `"something-new"` {
		t.Errorf("explainDisruption of an unknown token = %q, want it quoted verbatim", got)
	}

	// And the free-form node text beside a reclaim is quoted too — it reaches a
	// report an operator reads as billet's own output.
	reclaimed := explainDisruption(alloc.DisruptionReclaimed, "ec2 spot\ninterruption")
	if !strings.Contains(reclaimed, `"ec2 spot\ninterruption"`) {
		t.Errorf("a node's reason was not quoted into the report: %s", reclaimed)
	}
}

func TestAgoRendersARecordedTimestamp(t *testing.T) {
	t.Parallel()

	if got := ago("not a timestamp"); got != "not a timestamp" {
		t.Errorf("ago of an unparseable stamp = %q, want the bytes back", got)
	}

	stamp := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339Nano)
	if got := ago(stamp); !strings.HasSuffix(got, " ago") || strings.HasPrefix(got, "<1m") {
		t.Errorf("ago(%s) = %q, want an elapsed duration", stamp, got)
	}
}
