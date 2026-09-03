package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
)

// barrierHost registers a host in the ledger and returns its allocator, so a
// test can stage the fleet these commands are about.
//
// AT THE BARRIER WIRE VERSION, because a host below it can never answer and
// would block for that reason instead of the one under test.
func barrierHost(t *testing.T, db *state.DB, cfg *config.Config, name string) *alloc.Allocator {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: name, Provider: config.ProviderDocker,
		VCPU: 1 << 20, Memory: 1 << 20 * config.GiB,
		WireMin: 12, WireMax: alloc.BarrierWireVersion, WireVersion: alloc.BarrierWireVersion,
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	return a
}

// atClock is an allocator over the same ledger with a clock a test chooses.
//
// The command under test reads through its OWN allocator at the real clock; this
// is only how a test writes rows that are dated where it needs them.
func atClock(t *testing.T, db *state.DB, cfg *config.Config, at time.Time) *alloc.Allocator {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Nodes: cfg.NodePolicies(),
	}, cfg.Tiers, alloc.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	return a
}

// proveHost plays the control plane's barrier loop for one host.
//
// TWO OBSERVATIONS SPANNING THE GRACE, both dated in the PAST. A proof is an
// empty answer taken at or after the grace elapsed, so one answer would leave
// the host settling forever however long the test then waited — and dating them
// in the past is what lets the command under test, reading at the real clock,
// find a run that has already completed without anything sleeping for five
// minutes.
//
// A NON-EMPTY ANSWER NEEDS ONLY ONE, because it ends the run rather than
// building it.
func proveHost(t *testing.T, db *state.DB, cfg *config.Config, name string, empty bool) {
	t.Helper()

	now := time.Now().UTC()

	a := atClock(t, db, cfg, now)

	barrier, found, err := a.ComputeBarrierInForce(t.Context())
	if err != nil {
		t.Fatalf("ComputeBarrierInForce: %v", err)
	}
	if !found {
		t.Fatal("no barrier is in force, so there is nothing to answer")
	}

	fence, found, err := a.NodeFenceOf(t.Context(), name)
	if err != nil {
		t.Fatalf("NodeFenceOf(%s): %v", name, err)
	}
	if !found {
		t.Fatalf("%s is not in the ledger", name)
	}

	obs := alloc.BarrierObservation{
		Node: name, BarrierID: barrier.ID, Fence: fence, Empty: empty,
	}

	// An hour back, so both ends of the run are comfortably in the past.
	started := now.Add(-time.Hour)

	if err := atClock(t, db, cfg, started).
		RecordBarrierObservation(t.Context(), obs); err != nil {
		t.Fatalf("RecordBarrierObservation: %v", err)
	}

	if !empty {
		return
	}

	// Thirty minutes later — comfortably past the five-minute grace, and still
	// half an hour before the command under test reads any of it.
	if err := atClock(t, db, cfg, started.Add(30*time.Minute)).
		RecordBarrierObservation(t.Context(), obs); err != nil {
		t.Fatalf("RecordBarrierObservation: %v", err)
	}
}

// answerBarrierWhenAsked runs alongside a waiting command: as soon as a barrier
// appears, it answers for the named host.
//
// THE COMMAND WRITES THE REQUEST, so nothing can be staged in advance — the
// barrier id does not exist until the drain reaches its second stage.
func answerBarrierWhenAsked(
	t *testing.T, db *state.DB, cfg *config.Config, name string, empty bool,
) chan struct{} {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		a, err := alloc.New(db, alloc.Limits{
			MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
			Nodes: cfg.NodePolicies(),
		}, cfg.Tiers)
		if err != nil {
			t.Errorf("allocator: %v", err)

			return
		}

		deadline := time.Now().Add(30 * time.Second)

		for time.Now().Before(deadline) {
			_, found, err := a.ComputeBarrierInForce(context.WithoutCancel(t.Context()))
			if err != nil {
				t.Errorf("ComputeBarrierInForce: %v", err)

				return
			}

			if found {
				proveHost(t, db, cfg, name, empty)

				return
			}

			time.Sleep(20 * time.Millisecond)
		}

		t.Error("no barrier was ever requested, so the second stage never ran")
	}()

	return done
}

// A DRAIN DOES NOT STOP AT THE LEDGER. Once the ledger holds nothing it asks
// every host what it is actually running, which is the only way to see compute
// whose lease has already gone.
func TestDrainWaitsForEveryHostToSayItIsRunningNothing(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "drain-host")

	answered := answerBarrierWhenAsked(t, db, cfg, "drain-host", true)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out := capture(t, func() {
		if err := cmdDrain(ctx, []string{"--config", cfgPath, "--wait"}); err != nil {
			t.Errorf("drain --wait: %v", err)
		}
	})

	<-answered

	if !strings.Contains(out, "asking each host what it is actually running") {
		t.Errorf("the drain never reached the second stage:\n%s", out)
	}

	if !strings.Contains(out, "says it is running no compute") {
		t.Errorf("the drain did not report what was proved:\n%s", out)
	}
}

// AND IT DOES NOT FINISH WHILE A HOST SAYS IT IS RUNNING SOMETHING.
//
// This is the whole class the ledger cannot see: the lease is gone, so
// Quiescence is quiet, and the host still has the compute.
func TestDrainDoesNotFinishWhileAHostSaysItIsRunningWork(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "drain-host")

	answered := answerBarrierWhenAsked(t, db, cfg, "drain-host", false)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out := capture(t, func() {
		err := cmdDrain(ctx, []string{"--config", cfgPath, "--wait", "--timeout", "3s"})
		if err == nil {
			t.Error("a drain finished while a host said it was running compute")
		}
	})

	<-answered

	if strings.Contains(out, "says it is running no compute") {
		t.Errorf("the drain claimed the fleet was proved idle:\n%s", out)
	}

	if !strings.Contains(out, "waiting on 1 host") {
		t.Errorf("the drain did not name what it was waiting on:\n%s", out)
	}
}

// THE DRAIN RE-READS ADMISSION WHILE IT IS PROVING THE FLEET, not only while it
// is waiting on the ledger.
//
// The control plane drops a barrier whose admission generation has moved, but
// that is asynchronous cleanup rather than a fence — and in this test there is
// no control plane running at all, which is precisely the window. Every host's
// run is still stored and still fenced, so without this check the drain reports
// the fleet proved idle and exits 0 against a deployment that was reopened, took
// work, and was closed again.
func TestDrainStopsIfAdmissionMovesWhileProvingTheFleet(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "drain-host")

	moved := make(chan struct{})

	go func() {
		defer close(moved)

		a, err := alloc.New(db, alloc.Limits{
			MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
			Nodes: cfg.NodePolicies(),
		}, cfg.Tiers)
		if err != nil {
			t.Errorf("allocator: %v", err)

			return
		}

		ctx := context.WithoutCancel(t.Context())
		deadline := time.Now().Add(30 * time.Second)

		for time.Now().Before(deadline) {
			barrier, found, err := a.ComputeBarrierInForce(ctx)
			if err != nil {
				t.Errorf("ComputeBarrierInForce: %v", err)

				return
			}

			if found {
				// The host answers and is proved, and only THEN does somebody
				// reopen the deployment and close it again. Nothing touches the
				// host, so its epoch and dispatch fences both still match.
				proveHost(t, db, cfg, "drain-host", true)

				resumed, err := db.Resume(ctx, state.ResumeRequest{
					Expect: barrier.Generation, Clears: state.ProvenanceOperator,
					Actor: "someone else",
				})
				if err != nil {
					t.Errorf("Resume: %v", err)

					return
				}

				if _, err := db.Seal(ctx, state.SealRequest{
					Expect: resumed.Generation, Provenance: state.ProvenanceOperator,
					Actor: "someone else",
				}); err != nil {
					t.Errorf("Seal: %v", err)
				}

				return
			}

			time.Sleep(20 * time.Millisecond)
		}

		t.Error("no barrier was ever requested, so the second stage never ran")
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out := capture(t, func() {
		err := cmdDrain(ctx, []string{"--config", cfgPath, "--wait"})
		if err == nil {
			t.Error("a drain reported the fleet proved idle after admission moved underneath it")

			return
		}

		if !strings.Contains(err.Error(), "admission moved") {
			t.Errorf("the diagnostic does not say what changed: %v", err)
		}
	})

	<-moved

	if strings.Contains(out, "says it is running no compute") {
		t.Errorf("the drain printed the proved conclusion anyway:\n%s", out)
	}
}

// THE ESCAPE IS NAMED AND IT PRINTS A DIFFERENT CONCLUSION. An operator who
// waives the proof must not be told the same thing as one who obtained it.
func TestDrainWithoutComputeProofSaysWhatItDidNotEstablish(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "drain-host")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out := capture(t, func() {
		if err := cmdDrain(ctx, []string{
			"--config", cfgPath, "--wait", "--without-compute-proof",
		}); err != nil {
			t.Errorf("drain --wait --without-compute-proof: %v", err)
		}
	})

	if !strings.Contains(out, "No host was asked") {
		t.Errorf("a waived proof did not say so:\n%s", out)
	}

	if strings.Contains(out, "says it is running no compute") {
		t.Errorf("a waived proof printed the conclusion of one that was obtained:\n%s", out)
	}
}

// THE ESCAPE IS REFUSED WHERE IT WOULD MEAN NOTHING. Without --wait this command
// proves nothing either way, so accepting the flag would leave somebody
// believing they had waived something.
func TestDrainRefusesTheEscapeWithoutAWait(t *testing.T) {
	_, cfgPath := drainFixture(t)

	err := cmdDrain(t.Context(), []string{"--config", cfgPath, "--without-compute-proof"})
	if err == nil {
		t.Fatal("--without-compute-proof was accepted on a drain that waits for nothing")
	}

	if !strings.Contains(err.Error(), "only applies to --wait") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// `billet local down` DOES NOT STOP A SERVICE WHILE A HOST SAYS IT IS RUNNING
// WORK.
//
// THE ASSERTION IS THAT NOTHING WAS STOPPED, never merely that an error came
// back. An error value is the cheapest thing a function produces: the command
// could return one and stop the units anyway, which is exactly the failure this
// whole change exists to prevent.
func TestDownStopsNothingWhileAHostSaysItIsRunningCompute(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "probe-node")

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	answered := answerBarrierWhenAsked(t, db, cfg, "probe-node", false)

	err := runLocalDown(t.Context(), downOptions{configPath: cfgPath, timeout: 3 * time.Second})

	<-answered

	if err == nil {
		t.Fatal("a host that said it was running compute was taken down anyway")
	}

	for _, step := range f.trace {
		if strings.HasPrefix(step, "stop ") || strings.HasPrefix(step, "disable ") {
			t.Errorf("a host said it was running compute and down did %q; that fails "+
				"somebody's build", step)
		}
	}

	// The seal STAYS. The operator's intent has not changed and the compute is
	// still there, so reopening on the way out would invite work onto a host
	// somebody is trying to take down.
	if got := admissionNow(t, db); got.Mode != state.AdmissionSealed {
		t.Errorf("admission is %s after an abandoned down, want it still sealed", got.Mode)
	}
}

// AND IT DOES STOP ONCE EVERY HOST HAS SAID IT IS RUNNING NOTHING.
//
// The companion to the test above, and it is not optional: a command that
// refused everything would pass that one and be useless.
func TestDownStopsOnceTheFleetIsProvedIdle(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "probe-node")

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	answered := answerBarrierWhenAsked(t, db, cfg, "probe-node", true)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("a proved-idle host was refused: %v", err)
		}
	})

	<-answered

	if !strings.Contains(strings.Join(f.trace, " "), "stop "+deploy.NodeUnitName) {
		t.Errorf("a proved-idle host was not stopped; down did %v", f.trace)
	}

	if !strings.Contains(out, "says it is running no compute") {
		t.Errorf("the report does not say what was proved:\n%s", out)
	}
}

// THE GAP BETWEEN PROVING AND STOPPING IS RE-READ, NOT ASSUMED.
//
// MUTATION TESTING FOUND THIS MISSING. The test above never reaches
// stopAndDisable at all — its wait times out — so it says nothing about the
// window this guards: the fleet was proved clear, the wait returned, and a
// launch was dispatched before the first unit stopped. That launch moves the
// host's dispatch fence and discards its run, so re-reading is what turns "it
// was proved a moment ago" into "it is still proved now".
//
// Driven through stopAndDisable directly, the way the admission-generation fence
// beside it already is: the window it covers opens after the wait has returned,
// and a test that went through runLocalDown could not stage anything inside it.
func TestDownStopsNothingIfTheFleetStoppedBeingProvedBeforeItActed(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	a := barrierHost(t, db, cfg, "probe-node")

	sealed, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceLocalDown, Reason: "test", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := a.RequestComputeBarrier(t.Context(), sealed.Generation, "ops"); err != nil {
		t.Fatalf("RequestComputeBarrier: %v", err)
	}

	proveHost(t, db, cfg, "probe-node", true)

	req := lifeops.UpRequest{ConfigPath: cfgPath, WantServer: true, WantNode: true}

	// FIRST, THE COMPANION: a fleet that is still proved DOES get stopped. Without
	// this, a guard that refused everything would pass the assertion below and be
	// useless.
	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	if err := stopAndDisable(t.Context(), f, cfg, req, sealed.Generation, true); err != nil {
		t.Fatalf("a fleet that is still proved idle was refused: %v", err)
	}

	if !strings.Contains(strings.Join(f.trace, " "), "stop "+deploy.NodeUnitName) {
		t.Fatalf("a proved fleet was not stopped; down did %v", f.trace)
	}

	// NOW THE WINDOW: a launch is dispatched to that host after the proof and
	// before the stop.
	if _, err := a.BumpDispatch(t.Context(), "probe-node"); err != nil {
		t.Fatalf("BumpDispatch: %v", err)
	}

	after := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	err = stopAndDisable(t.Context(), after, cfg, req, sealed.Generation, true)
	if err == nil {
		t.Fatal("a host whose proof had been discarded by a later launch was stopped anyway")
	}

	if !strings.Contains(err.Error(), "stopped being proved idle") {
		t.Errorf("the diagnostic does not say what changed: %v", err)
	}

	if !strings.Contains(err.Error(), "probe-node") {
		t.Errorf("the diagnostic does not name the host: %v", err)
	}

	for _, step := range after.trace {
		if strings.HasPrefix(step, "stop ") || strings.HasPrefix(step, "disable ") {
			t.Errorf("the proof was gone and down did %q; that fails somebody's build", step)
		}
	}
}

// A HOST WHOSE CONTROL PLANE IS STOPPED CANNOT BE ASKED, and says so rather than
// waiting for silence.
//
// The barrier is issued by the control plane — the request is a durable row and
// the running server is what puts the question to each host — so on a machine
// whose server is already down there is nobody to ask.
func TestDownSaysWhenThereIsNoControlPlaneToAskThroughAndStopsAnyway(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "probe-node")

	// Only the NODE is running. There is no control plane to route the question
	// through, and no answer will ever arrive.
	//
	// STAGED ON `running`, which is what the command actually reads. The fake's
	// default is a live service, so a test that staged only `inspect` would leave
	// the server reported as up and wait out a proof nobody could deliver — which
	// is how this test hung the first time it was written.
	f := stageDown(t, &fakeConverger{
		running: map[string]lifeops.RunningFacts{
			deploy.ServerUnitName: {Active: lifeops.No, IsThisBuild: lifeops.Yes},
			deploy.NodeUnitName:   {Active: lifeops.Yes, IsThisBuild: lifeops.Yes},
		},
	}, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("a host with no control plane to ask through was refused: %v", err)
		}
	})

	if !strings.Contains(out, "the control plane on this host is not running") {
		t.Errorf("the report does not say why nothing was asked:\n%s", out)
	}

	if strings.Contains(out, "says it is running no compute") {
		t.Errorf("a host that asked nobody claimed the fleet was proved idle:\n%s", out)
	}

	if !strings.Contains(strings.Join(f.trace, " "), "stop ") {
		t.Errorf("nothing was stopped on a host that could not obtain a proof; that makes "+
			"the command useless where it is needed most: %v", f.trace)
	}
}

// AND THE WAIVED PATH SAYS WHAT IT DID NOT ESTABLISH.
func TestDownWithoutComputeProofSaysWhatItDidNotEstablish(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	db := openLedger(t, stateDir)
	cfg := loadFixtureConfig(t, cfgPath)

	barrierHost(t, db, cfg, "probe-node")

	stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	out := capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{
			configPath: cfgPath, withoutProof: true,
		}); err != nil {
			t.Errorf("a waived proof refused the command: %v", err)
		}
	})

	if !strings.Contains(out, "NO HOST WAS ASKED") {
		t.Errorf("a waived proof did not say so:\n%s", out)
	}

	if strings.Contains(out, "says it is running no compute") {
		t.Errorf("a waived proof printed the conclusion of one that was obtained:\n%s", out)
	}
}

// A DECOMMISSION WITHOUT PROOF IS RECORDED AS SUCH, and the command says so in
// those words — that is the difference between billet knowing a machine is idle
// and an operator asserting it.
func TestDecommissionWithoutProofSaysTheExclusionIsUnproven(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	a := barrierHost(t, db, cfg, "retired-host")

	fence, _, err := a.NodeFenceOf(t.Context(), "retired-host")
	if err != nil {
		t.Fatalf("NodeFenceOf: %v", err)
	}

	if err := a.NodeGone(t.Context(), "retired-host", fence.Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	// Without --force it is refused, and the refusal names what is missing.
	err = cmdNodes(t.Context(), []string{"decommission", "retired-host", "--config", cfgPath})
	if err == nil {
		t.Fatal("a host nothing had proved idle was decommissioned")
	}
	if !strings.Contains(err.Error(), "nothing has proved") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}

	out := capture(t, func() {
		if err := cmdNodes(t.Context(), []string{
			"decommission", "retired-host", "--config", cfgPath, "--force",
		}); err != nil {
			t.Errorf("a forced decommission was refused: %v", err)
		}
	})

	if !strings.Contains(out, "UNPROVEN") {
		t.Errorf("a forced decommission did not record itself as unproven:\n%s", out)
	}
}

// AND A LATER DRAIN NAMES IT RATHER THAN REPORTING THE FLEET CLEAR.
//
// This is what stops membership laundering uncertainty: if a silent host could
// simply be excluded, the next drain would report "nothing is running" while
// that host ran somebody's job.
func TestADrainNamesAHostExcludedWithoutProof(t *testing.T) {
	db, cfgPath := drainFixture(t)
	cfg := loadFixtureConfig(t, cfgPath)

	a := barrierHost(t, db, cfg, "retired-host")

	fence, _, err := a.NodeFenceOf(t.Context(), "retired-host")
	if err != nil {
		t.Fatalf("NodeFenceOf: %v", err)
	}

	if err := a.NodeGone(t.Context(), "retired-host", fence.Epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if _, err := a.Decommission(t.Context(), alloc.DecommissionRequest{
		Node: "retired-host", Actor: "ops", Force: true,
	}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out := capture(t, func() {
		if err := cmdDrain(ctx, []string{"--config", cfgPath, "--wait"}); err != nil {
			t.Errorf("drain --wait: %v", err)
		}
	})

	if !strings.Contains(out, "EXCLUDED from that set without proof") {
		t.Errorf("the drain did not name the host excluded without proof:\n%s", out)
	}

	if !strings.Contains(out, "retired-host") {
		t.Errorf("the drain did not name WHICH host:\n%s", out)
	}
}

// A HOST THAT ANSWERED AND THEN WENT AWAY HAS STILL ANSWERED.
//
// `anyoneAnswered` decides whether a long wait prints "Nothing has answered yet
// … check that `billet server` is up". That line sends an operator to look at a
// control plane, so it must never appear about a fleet that HAS answered.
//
// It used to switch on the state alone, and a host that gave a fenced empty
// answer and then stopped responding is reported ClearanceUnreachable — so the
// drain would tell an operator nothing had answered and point them at a control
// plane that was fine. The retained run is the evidence: EmptySince is set only
// while a run is still fenced to this barrier and this registration.
func TestAHostThatAnsweredAndThenWentAwayCountsAsAnAnswer(t *testing.T) {
	t.Parallel()

	answered := alloc.ComputeClearance{Nodes: []alloc.NodeClearance{{
		Node:       "gone-1",
		State:      alloc.ClearanceUnreachable,
		EmptySince: "2026-08-31T05:23:23Z",
		ClearAt:    "2026-08-31T05:28:21Z",
	}}}

	if !anyoneAnswered(answered) {
		t.Error("a host with a retained run was counted as never having answered; the " +
			"drain then tells an operator to check a control plane that is fine")
	}

	// AND A HOST THAT GENUINELY NEVER ANSWERED STILL COUNTS AS SILENT, or the
	// diagnostic this protects would never appear at all.
	silent := alloc.ComputeClearance{Nodes: []alloc.NodeClearance{{
		Node: "gone-2", State: alloc.ClearanceUnreachable,
	}}}

	if anyoneAnswered(silent) {
		t.Error("a host with no run at all was counted as having answered")
	}
}
