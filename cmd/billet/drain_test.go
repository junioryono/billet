package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// drainFixture opens a ledger the way a running control plane holds it, and
// returns the config path an operator command is given.
//
// THE LEDGER STAYS OPEN THROUGH state.Open, which is the interesting case rather
// than a convenience: `billet drain` exists to be run against a LIVE deployment,
// so every one of these tests exercises the admin path that proceeds without the
// directory lock. Opening it here first is what makes that true.
func drainFixture(t *testing.T) (*state.DB, string) {
	t.Helper()

	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db, cfg
}

func admissionNow(t *testing.T, db *state.DB) state.Admission {
	t.Helper()

	a, err := db.Admission(t.Context())
	if err != nil {
		t.Fatalf("read admission: %v", err)
	}

	return a
}

// A DRAIN SEALS THE DEPLOYMENT, AND THE SEAL CARRIES ITS ATTRIBUTION.
func TestDrainSealsTheDeploymentWithAnOperatorProvenance(t *testing.T) {
	db, cfg := drainFixture(t)

	out := capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg, "--reason", "replacing a disk"}); err != nil {
			t.Errorf("drain: %v", err)
		}
	})

	got := admissionNow(t, db)

	if got.Mode != state.AdmissionSealed {
		t.Fatalf("admission is %s after a drain, want sealed", got.Mode)
	}

	// OPERATOR, NOT local-down, and this is the whole difference between a drain
	// an operator can rely on and one a service restart silently undoes.
	if got.Provenance != state.ProvenanceOperator {
		t.Errorf("the seal's provenance is %q, want %q; a %s seal is cleared by the next "+
			"`billet local up`, so the drain would not survive a restart",
			got.Provenance, state.ProvenanceOperator, state.ProvenanceLocalDown)
	}
	if got.Reason != "replacing a disk" {
		t.Errorf("the seal records reason %q, want the one given", got.Reason)
	}
	if got.Actor == "" {
		t.Error("the seal records no actor; a seal nobody can attribute is one nobody dares clear")
	}

	if !strings.Contains(out, "survives a control-plane restart") {
		t.Errorf("the report does not say the seal survives a restart:\n%s", out)
	}
}

// RE-DRAINING CHANGES NOTHING, INCLUDING THE GENERATION.
//
// The generation is a fence: an unrelated operator may be holding one and about
// to act on it. Moving it because somebody ran a command that changed nothing
// would invalidate their decision, and the failure is silent — their next write
// is refused for a reason that never happened.
func TestASecondDrainDoesNotMoveTheGeneration(t *testing.T) {
	db, cfg := drainFixture(t)

	capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg, "--reason", "first"}); err != nil {
			t.Errorf("first drain: %v", err)
		}
	})

	first := admissionNow(t, db)

	out := capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("second drain: %v", err)
		}
	})

	second := admissionNow(t, db)

	if second.Generation != first.Generation {
		t.Errorf("the generation moved from %d to %d for a command that changed nothing; "+
			"that invalidates any fence another operator is holding",
			first.Generation, second.Generation)
	}
	if second.Reason != "first" || second.Actor != first.Actor {
		t.Errorf("the second drain rewrote the attribution: %+v, want %+v", second, first)
	}
	if !strings.Contains(out, "Already sealed") {
		t.Errorf("the second drain does not report that it changed nothing:\n%s", out)
	}
}

// A REASON THAT WILL NOT BE RECORDED IS SAID OUT LOUD. Silently discarding it
// leaves the operator believing the ledger carries their explanation, and the
// next person to find the seal reads somebody else's.
func TestADrainThatKeepsAnExistingSealSaysTheReasonWasNotRecorded(t *testing.T) {
	db, cfg := drainFixture(t)

	capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg, "--reason", "first"}); err != nil {
			t.Errorf("first drain: %v", err)
		}
	})

	out := capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg, "--reason", "second"}); err != nil {
			t.Errorf("second drain: %v", err)
		}
	})

	if !strings.Contains(out, "NOT recorded") {
		t.Errorf("a discarded --reason was not reported:\n%s", out)
	}
	if got := admissionNow(t, db); got.Reason != "first" {
		t.Errorf("the reason is now %q; the report and the ledger disagree", got.Reason)
	}
}

// A SHUTDOWN'S SEAL IS ESCALATED, AND THE CHANGE IS REPORTED.
//
// Left as it was, the next successful `billet local up` clears it and the
// deployment starts taking work again — which is not what somebody who ran
// `billet drain` asked for.
func TestDrainEscalatesAShutdownSealSoARestartWillNotReopenIt(t *testing.T) {
	db, cfg := drainFixture(t)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Actor: "billet local down",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	out := capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("drain: %v", err)
		}
	})

	got := admissionNow(t, db)

	if got.Provenance != state.ProvenanceOperator {
		t.Errorf("the seal is still %q, so `billet local up` would reopen a deployment an "+
			"operator deliberately drained", got.Provenance)
	}
	if !strings.Contains(out, "local up") {
		t.Errorf("the escalation was not reported, so the operator cannot know a restart no "+
			"longer reopens it:\n%s", out)
	}
}

// RESUME WILL NOT CLEAR A SHUTDOWN'S SEAL, AND NAMES WHAT DOES.
func TestResumeRefusesAShutdownSealAndLeavesItInPlace(t *testing.T) {
	db, cfg := drainFixture(t)

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Actor: "billet local down",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	before := admissionNow(t, db)

	err := cmdResume(t.Context(), []string{"--config", cfg})
	if err == nil {
		t.Fatal("resume cleared a shutdown's seal, reopening admission onto services that " +
			"are stopping")
	}
	if !strings.Contains(err.Error(), "billet local up") {
		t.Errorf("the refusal does not name the command that does work: %v", err)
	}

	// AND THEREFORE NOTHING ELSE HAPPENED. An error return says the command
	// refused; it does not say the ledger is untouched.
	after := admissionNow(t, db)

	if after.Mode != state.AdmissionSealed || after.Provenance != state.ProvenanceLocalDown {
		t.Errorf("the refused resume still changed admission: %+v, want %+v", after, before)
	}
	if after.Generation != before.Generation {
		t.Errorf("the refused resume moved the generation from %d to %d",
			before.Generation, after.Generation)
	}
}

// RESUME CLEARS AN OPERATOR'S SEAL, which is the direction that must work or the
// refusal above is just a command that never works.
func TestResumeClearsAnOperatorSeal(t *testing.T) {
	db, cfg := drainFixture(t)

	capture(t, func() {
		if err := cmdDrain(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("drain: %v", err)
		}
	})

	capture(t, func() {
		if err := cmdResume(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("resume: %v", err)
		}
	})

	if got := admissionNow(t, db); got.Mode != state.AdmissionOpen {
		t.Errorf("admission is %s after a resume, want open", got.Mode)
	}
}

// RESUMING AN OPEN DEPLOYMENT MOVES NOTHING, for the same fence reason as a
// second drain.
func TestResumingAnOpenDeploymentDoesNotMoveTheGeneration(t *testing.T) {
	db, cfg := drainFixture(t)

	before := admissionNow(t, db)

	capture(t, func() {
		if err := cmdResume(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("resume: %v", err)
		}
	})

	if after := admissionNow(t, db); after.Generation != before.Generation {
		t.Errorf("the generation moved from %d to %d for a resume that changed nothing",
			before.Generation, after.Generation)
	}
}

// outstandingLease makes the deployment hold something, so a drain has to wait.
func outstandingLease(t *testing.T, db *state.DB, cfg *config.Config) *alloc.Lease {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "drain-host", Provider: config.ProviderDocker,
		VCPU: 1 << 20, Memory: 1 << 20 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	lease, err := a.Reserve(t.Context(), cfg.Tiers[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "drain-host"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 4242, 77); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	return lease
}

func loadFixtureConfig(t *testing.T, cfgPath string) *config.Config {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	return cfg
}

// A DRAIN WITH NOTHING RUNNING RETURNS IMMEDIATELY.
func TestDrainWaitReturnsWhenTheDeploymentHoldsNothing(t *testing.T) {
	_, cfg := drainFixture(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out := capture(t, func() {
		if err := cmdDrain(ctx, []string{"--config", cfg, "--wait"}); err != nil {
			t.Errorf("drain --wait: %v", err)
		}
	})

	if !strings.Contains(out, "Drained") {
		t.Errorf("a deployment holding nothing did not report itself drained:\n%s", out)
	}
}

// A DRAIN WAITS FOR RUNNING WORK, REPORTS WHAT IT IS WAITING FOR, AND GIVING UP
// IS AN ANSWER RATHER THAN A FAILURE.
//
// The exit status is the load-bearing part: a monitor running this on a timer
// has to tell "drained" from "still draining" from "billet broke", and folding
// the middle one into either of the others makes a maintenance window look like
// an outage or an outage look like a maintenance window.
func TestDrainWaitTimesOutWithItsOwnStatusAndKeepsTheSeal(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	lease := outstandingLease(t, db, cfg)

	// AN INDEPENDENT BOUND, so deleting the --timeout enforcement makes this test
	// FAIL rather than hang forever. It is far longer than the timeout under test,
	// and the assertion below says which one fired: the deadline produces
	// errStillDraining, this outer cancellation produces errWaitInterrupted.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out := capture(t, func() {
		err := cmdDrain(ctx, []string{"--config", cfgPath, "--wait", "--timeout", "1s"})
		if err == nil {
			t.Error("a drain with a job still running reported success")

			return
		}

		if !errors.Is(err, errStillDraining) {
			t.Errorf("the timeout returned %v, want the still-draining answer", err)
		}
		if got := exitStatus(err); got != 2 {
			t.Errorf("giving up waiting exits %d; a monitor cannot tell it from a failure", got)
		}
	})

	// WHAT IT WAS WAITING FOR, by name. A count tells an operator nothing about
	// whether to keep waiting; the run id tells them whose build it is.
	if !strings.Contains(out, cfg.Tiers[0].Label) {
		t.Errorf("the wait does not name the tier it is waiting on:\n%s", out)
	}
	if !strings.Contains(out, "4242") {
		t.Errorf("the wait does not name the run it is waiting on:\n%s", out)
	}

	// AND THE SEAL SURVIVES GIVING UP. Reopening on the way out would mean a
	// timed-out drain silently started taking work again.
	if got := admissionNow(t, db); got.Mode != state.AdmissionSealed {
		t.Errorf("admission is %s after a timed-out wait, want it still sealed", got.Mode)
	}

	_ = lease
}

// AND IT STOPS WAITING WHEN THE WORK FINISHES, so the timeout above is a wait
// rather than a command that can only ever give up.
func TestDrainWaitReturnsOnceTheRunningWorkIsReleased(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	lease := outstandingLease(t, db, cfg)

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	released := make(chan struct{})

	go func() {
		defer close(released)

		// ORDERED AGAINST THE SEAL rather than against the clock: the release has
		// to land after the drain is under way, which is what a finishing job
		// does. The output assertion below is what proves it actually landed
		// during the wait rather than before the first sample.
		waitUntilSealed(t, db, 30*time.Second)
		time.Sleep(drainPollInterval)

		if err := a.Release(context.WithoutCancel(ctx), lease.ID, lease.Epoch,
			alloc.PhaseDone); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

	// WITHOUT THE COMPUTE PROOF, because this test is about the LEDGER barrier's
	// waiting path. The fixture registers a host and runs no control plane, so
	// nothing is there to put the second stage's question to it — waiting for
	// that here would be waiting for a process this test never starts, and the
	// second stage has tests of its own.
	out := capture(t, func() {
		if err := cmdDrain(ctx, []string{
			"--config", cfgPath, "--wait", "--without-compute-proof",
		}); err != nil {
			t.Errorf("drain --wait: %v", err)
		}
	})

	<-released

	// IT ACTUALLY WAITED. Without this the test passes when the release wins the
	// race and the drain takes the idle path on its first sample — proving only
	// that an empty deployment reports quiet, which another test already covers.
	if !strings.Contains(out, "waiting on") {
		t.Errorf("the drain never reported waiting, so it never exercised the waiting "+
			"path:\n%s", out)
	}
	if !strings.Contains(out, "Drained") {
		t.Errorf("the drain did not report itself drained once the work finished:\n%s", out)
	}
}

// A RESUME UNDERNEATH A WAITING DRAIN STOPS IT, rather than leaving it waiting
// for a condition that can no longer arrive: without the seal, the next poll can
// fill the deployment straight back up.
func TestDrainWaitStopsIfSomebodyReopensAdmission(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	outstandingLease(t, db, cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	reopened := make(chan struct{})

	go func() {
		defer close(reopened)

		// SYNCHRONISED ON THE SEAL, not on a sleep. Resuming before the drain has
		// sealed would leave the waiter seeing an open ledger on its first sample
		// and aborting for that reason instead — the test would then pass with
		// takeTheSeal deleted, proving nothing about a seal being cleared.
		sealed := waitUntilSealed(t, db, 30*time.Second)

		if _, err := db.Resume(context.WithoutCancel(ctx), state.ResumeRequest{
			Expect: sealed.Generation, Clears: state.ProvenanceOperator, Actor: "someone else",
		}); err != nil {
			t.Errorf("Resume: %v", err)
		}
	}()

	var got error

	out := capture(t, func() { got = cmdDrain(ctx, []string{"--config", cfgPath, "--wait"}) })

	<-reopened

	if got == nil {
		t.Fatal("a drain whose seal was cleared underneath it reported success; nothing was " +
			"stopping the deployment taking work again")
	}

	// THE DRAIN SEALED FIRST, so the abort below is about a seal being cleared
	// rather than about a deployment that was never sealed at all.
	if !strings.Contains(out, "Sealed") {
		t.Errorf("the drain never reported sealing:\n%s", out)
	}
	if !strings.Contains(got.Error(), "admission moved") {
		t.Errorf("the diagnostic does not say admission moved underneath the wait: %v", got)
	}
}

// waitUntilSealed blocks until the ledger records a seal, so a test can order
// itself against a command running concurrently instead of guessing with a
// sleep.
func waitUntilSealed(t *testing.T, db *state.DB, budget time.Duration) state.Admission {
	t.Helper()

	deadline := time.Now().Add(budget)

	for time.Now().Before(deadline) {
		a, err := db.Admission(t.Context())
		if err != nil {
			t.Errorf("read admission: %v", err)

			return state.Admission{}
		}

		if a.Mode == state.AdmissionSealed {
			return a
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Error("the deployment was never sealed within the budget")

	return state.Admission{}
}

// THE ACTOR IS A PERSON WHERE THERE IS ONE. Every sudo-using operator is root by
// the time billet sees them, and "sealed by root" tells the next one nothing.
func TestTheSealIsAttributedToTheInvokingPersonRatherThanRoot(t *testing.T) {
	t.Setenv("SUDO_USER", "aisha")

	if got := actor(); !strings.HasPrefix(got, "aisha") {
		t.Errorf("actor() is %q, want the invoking person; a seal attributed to root gives "+
			"the next operator nobody to ask", got)
	}
}

// INTERRUPTING A WAIT IS NOT SUCCESS.
//
// The process-wide signal handler cancels this context, so a SIGTERM in a
// pipeline arrives here without anybody pressing anything. Exiting 0 would tell
// a script that seals, waits and proceeds on success that it may proceed — while
// jobs are still running.
func TestInterruptingAWaitDoesNotReportTheDeploymentDrained(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	outstandingLease(t, db, cfg)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stop := make(chan struct{})

	go func() {
		defer close(stop)

		waitUntilSealed(t, db, 30*time.Second)
		time.Sleep(drainPollInterval / 2)
		cancel()
	}()

	var got error

	capture(t, func() { got = cmdDrain(ctx, []string{"--config", cfgPath, "--wait"}) })

	<-stop

	if got == nil {
		t.Fatal("an interrupted wait reported success; a script would read that as drained " +
			"and proceed while jobs were still running")
	}
	if !errors.Is(got, errWaitInterrupted) {
		t.Errorf("an interrupted wait returned %v, want the interrupted answer", got)
	}
	if code := exitStatus(got); code != 2 {
		t.Errorf("an interrupted wait exits %d, want 2 — the same answer as any other "+
			"not-drained outcome, and distinct from both success and failure", code)
	}
}

// RESUME NAMES A REMEDY THAT CAN ACTUALLY WORK.
//
// An unreadable row with a stale local-down provenance would otherwise send the
// operator to `billet local up`, which cannot open it either: state refuses to
// open a mode it does not recognise.
func TestResumeOnAnUnreadableRowAsksForItToBeRepaired(t *testing.T) {
	db, cfgPath := drainFixture(t)

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`UPDATE admission SET mode = 'quiescing', provenance = 'local-down' WHERE id = 1`)

		return err
	}); err != nil {
		t.Skipf("this SQLite build enforces the check regardless: %v", err)
	}

	if got := admissionNow(t, db); got.Mode != state.AdmissionUnknown {
		t.Skipf("the fixture did not reach the case: mode is %v", got.Mode)
	}

	err := cmdResume(t.Context(), []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("resume opened an admission row billet cannot read")
	}
	if !strings.Contains(err.Error(), "repaired") {
		t.Errorf("the diagnostic does not ask for the row to be repaired: %v", err)
	}
	if strings.Contains(err.Error(), "local up") {
		t.Errorf("the diagnostic names `billet local up`, which cannot open an unreadable "+
			"row either: %v", err)
	}
}

// A TIMEOUT THAT WOULD NOT DO WHAT IT SAYS IS REFUSED RATHER THAN REINTERPRETED.
func TestDrainRefusesATimeoutThatWouldNotBound(t *testing.T) {
	_, cfgPath := drainFixture(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			// Falls through a `> 0` test and waits forever, which is the opposite
			// of what somebody typing a timeout is asking for.
			"negative", []string{"--wait", "--timeout", "-5s"}, "negative",
		},
		{
			// Accepted and ignored, leaving the operator believing the command was
			// bounded when it returns immediately.
			"without --wait", []string{"--timeout", "5s"}, "only applies to --wait",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdDrain(t.Context(), append([]string{"--config", cfgPath}, tc.args...))
			if err == nil {
				t.Fatalf("a %s timeout was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not explain itself: %v", err)
			}

			// AND IT DID NOT SEAL ON THE WAY TO REFUSING. A rejected invocation
			// that still changed admission is worse than one that runs.
			if got := admissionNow(t, dbFor(t, cfgPath)); got.Mode != state.AdmissionOpen {
				t.Errorf("a refused invocation left admission %s", got.Mode)
			}
		})
	}
}

// dbFor reopens the ledger a config names, for assertions after a command that
// opened and closed its own handle.
func dbFor(t *testing.T, cfgPath string) *state.DB {
	t.Helper()

	cfg := loadFixtureConfig(t, cfgPath)

	db, err := state.OpenAdmin(t.Context(), cfg.Server.IdentityDir)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// A RESUME FOLLOWED BY A RESEAL BETWEEN SAMPLES IS STILL AN ABORT.
//
// This is the case a boolean cannot see. Watching only `Sealed`, the waiter
// samples "sealed", somebody resumes and seals again, and the next sample is
// "sealed" too — so it waits on placidly, never learning that admission was OPEN
// in between and the deployment could have taken work. The generation is what
// makes the gap observable.
func TestDrainWaitAbortsWhenAdmissionIsResealedBySomebodyElse(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	outstandingLease(t, db, cfg)

	// BOUNDED INDEPENDENTLY, so a waiter that wrongly carries on fails this test
	// instead of hanging it. That outcome is errWaitInterrupted, which the
	// assertion below distinguishes from the abort under test.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	moved := make(chan struct{})

	go func() {
		defer close(moved)

		sealed := waitUntilSealed(t, db, 20*time.Second)

		// BOTH INSIDE ONE POLL INTERVAL, which is the whole point: the waiter must
		// never observe the open state directly.
		resumed, err := db.Resume(context.WithoutCancel(ctx), state.ResumeRequest{
			Expect: sealed.Generation, Clears: state.ProvenanceOperator, Actor: "someone else",
		})
		if err != nil {
			t.Errorf("Resume: %v", err)

			return
		}

		if _, err := db.Seal(context.WithoutCancel(ctx), state.SealRequest{
			Expect: resumed.Generation, Provenance: state.ProvenanceOperator,
			Actor: "someone else", Reason: "their own maintenance",
		}); err != nil {
			t.Errorf("Seal: %v", err)
		}
	}()

	var got error

	capture(t, func() { got = cmdDrain(ctx, []string{"--config", cfgPath, "--wait"}) })

	<-moved

	if got == nil {
		t.Fatal("the wait reported the deployment drained after somebody else took admission " +
			"away from it and gave it back")
	}
	if errors.Is(got, errWaitInterrupted) {
		t.Fatal("the wait carried on until its outer bound, so it never noticed admission " +
			"being reopened and resealed underneath it")
	}
	if !strings.Contains(got.Error(), "admission moved") {
		t.Errorf("the diagnostic does not name the generation moving: %v", got)
	}
}

// A BROKEN LEDGER IS A FAILURE, NOT A TIMEOUT.
//
// Both arrive at the same place in the wait, and folding them together tells a
// monitor "still draining, all is well" when the state directory is unreadable.
// The exit status is the difference: 1 is billet failing, 2 is an answer.
func TestAWaitThatCannotReadTheLedgerFailsRatherThanReportingATimeout(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	outstandingLease(t, db, cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	broken := make(chan struct{})

	go func() {
		defer close(broken)

		waitUntilSealed(t, db, 20*time.Second)

		// ONLY THE BARRIER'S OWN READ IS BROKEN, mid-wait, the way a migration or a
		// hand-edited ledger could.
		if err := db.Tx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.WithoutCancel(ctx), `DROP TABLE admission`)

			return err
		}); err != nil {
			t.Errorf("drop admission: %v", err)
		}
	}()

	var got error

	capture(t, func() {
		got = cmdDrain(ctx, []string{"--config", cfgPath, "--wait", "--timeout", "25s"})
	})

	<-broken

	if got == nil {
		t.Fatal("a wait against an unreadable ledger reported the deployment drained")
	}
	if errors.Is(got, errStillDraining) || errors.Is(got, errWaitInterrupted) {
		t.Fatalf("a broken ledger was reported as an answer rather than a failure: %v", got)
	}
	if code := exitStatus(got); code != 1 {
		t.Errorf("a broken ledger exits %d, want 1; 2 is reserved for answers a monitor "+
			"acts on differently", code)
	}
}
