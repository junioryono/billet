package nodeclient_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/server"
)

// A NODE FINISHES ITS CONTAINERS BEFORE IT STOPS, and it does NOT hand them to
// custody on the way out.
//
// The supersession drain looks like the same thing and is not. It opens with
// compute.Superseded(), which moves running work into custody BECAUSE the
// control plane now routes those completions to whichever process holds the
// name. On a local SIGTERM nobody is taking over: the completions still route
// here, and moving the work to custody would strand the very reports this drain
// is waiting for.
func TestASignalledNodeDrainsWithoutHandingOverCustody(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:         testNodeVCPU,
			Memory:       testNodeMemory,
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 20 * time.Second,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// It keeps tending, which is what advances custody and lets held compute
	// finish. A node that simply returned would leave those leases renewed by
	// nobody.
	waitFor(t, func() bool { return compute.tended() > 0 })

	select {
	case <-done:
		t.Fatal("a signalled node stopped while it was still holding compute")
	case <-time.After(300 * time.Millisecond):
	}

	// AND IT DID NOT HAND OVER CUSTODY. This is the assertion that separates a
	// signal drain from a supersession drain, and the only one that fails if the
	// supersession path is reused wholesale.
	if n := compute.supersededCount(); n != 0 {
		t.Errorf("a signalled node moved %d running job(s) into custody; nobody is "+
			"taking over, so their completions still belong here", n)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case err := <-done:
		// A DRAINED NODE STOPS CLEANLY. This wanted the cancellation back, which
		// was encoding a defect: `systemctl stop billet-node` exited 1 and left the
		// unit failed, so `systemctl is-failed` answered yes after every deliberate
		// drain. The property that matters is that it stopped once the work was
		// done, which the select above already establishes; what this adds is that
		// stopping was not a failure.
		if err != nil {
			t.Errorf("a drained node reported a failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}
}

// A node holding nothing stops at once, AND SAYS NOTHING ABOUT DRAINING.
//
// The early return looks redundant — waitForHolding's own loop condition is
// false immediately, so the node stops either way — and it is not. Without it
// every ordinary restart of an idle node announces "draining: waiting for the
// compute still running here" and then exits, which tells an operator reading
// the journal that work was in flight when none was. A log line that is only
// sometimes true is worse than no log line.
func TestANodeHoldingNothingStopsImmediatelyAndSaysNothingAboutDraining(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: false}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: time.Hour,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case err := <-done:
		// CHECKED, because ignoring it lets any unrelated early exit satisfy this
		// test — a registration that failed, a refused provider — none of which is
		// the behaviour being asserted.
		//
		// NIL IS THE SIGNAL-SHAPED ANSWER. This used to require context.Canceled,
		// which was encoding a defect: returning it made an ordinary `systemctl
		// stop` exit 1 and left the unit failed. A clean stop now returns
		// nothing, and anything non-nil is the unrelated exit this guards
		// against.
		if err != nil {
			t.Errorf("the node stopped for some reason other than the signal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a node with nothing running waited out a drain it did not need")
	}

	logMu.Lock()
	defer logMu.Unlock()

	if strings.Contains(logged.String(), "draining") {
		t.Errorf("an idle node announced a drain it did not perform:\n%s", logged.String())
	}
}

// lockedWriter serialises writes from the janitor goroutine and the loop, which
// both log.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(p)
}

// nodeDrainOverrun is the node saying its wait is running long.
//
// A CONSTANT SO A REWORDED MESSAGE BREAKS THESE TESTS rather than leaving them
// watching for a line that can no longer appear. That is not hypothetical here:
// this exact rename — from "still draining" to wording that names what is being
// waited for — is what caught it, and the server's own suite had two assertions
// go vacuous the same way when a message outlived the constant matching it.
const nodeDrainOverrun = "this will not time out"

// THE NODE'S DRAIN IS NOT BOUNDED EITHER, AND IT SAYS SO WHILE IT WAITS.
//
// This was TestANodeDrainIsBounded, and it was correct: the wait expired and the
// process stopped. What that cost is less obvious than on the control plane,
// because the node's expiry was never destructive — the containers outlive the
// process and Recover re-adopts them. It cost the DELIVERY: a node that has
// stopped answering the control plane can no longer be told to destroy a job that
// has since finished, so that container lingers until the node starts again.
//
// The drain contract asks for the wait to be as long as the work on both
// halves. So the bound is gone, and what is asserted instead is that the drain
// keeps going and keeps reporting itself — a node that looks wedged is one
// somebody kills.
func TestANodeDrainIsNotBoundedAndKeepsReportingItself(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	// Holding, and never stops holding.
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	read := func() string {
		logMu.Lock()
		defer logMu.Unlock()

		return logged.String()
	}

	hurry := make(chan struct{})

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// The threshold this drain sails past. It decides only when the node
			// starts saying the drain is long.
			DrainTimeout: 50 * time.Millisecond,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	waitFor(t, func() bool { return strings.Contains(read(), "draining") })
	waitFor(t, func() bool { return strings.Contains(read(), nodeDrainOverrun) })

	// AND IT SAYS WHAT ENDS THE WAIT. A warning that reports an unbounded wait
	// without naming the lever that stops it tells an operator only that they are
	// stuck, and the two callers of this wait have different levers.
	if !strings.Contains(read(), "stop_waiting") {
		t.Errorf("the overrun warning does not name what ends the wait:\n%s", read())
	}

	if !strings.Contains(read(), "second signal") {
		t.Errorf("the overrun warning does not name the SECOND SIGNAL, which is what "+
			"ends a drain specifically:\n%s", read())
	}

	// AND IT IS STILL GOING. The old bound would have expired many times over.
	select {
	case <-done:
		t.Fatalf("the node drain ended on its own; nothing may end it but the work "+
			"finishing or a second signal:\n%s", read())
	case <-time.After(500 * time.Millisecond):
	}

	// NOTHING WAS DESTROYED BY WAITING, which is the promise the report makes.
	if _, _, gone := compute.snapshot(); len(gone) != 0 {
		t.Errorf("an overrunning node drain destroyed %v", gone)
	}

	close(hurry)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the node's drain")
	}

	if !strings.Contains(read(), "stopped waiting") {
		t.Errorf("the drain ended without reporting that it had stopped waiting:\n%s",
			read())
	}
}

// A TEND THAT WAS STILL RUNNING WHEN THE WAIT ENDED IS NOT A CUSTODY FAILURE.
//
// Tend takes the drain's context, so a second signal arriving while it is inside
// one makes it return context.Canceled. Logged as an error that reads as custody
// having failed — and if the reporting threshold has passed, the loop would go on
// to tell the operator to send a second signal they have just sent. Neither is
// true, and both land at the moment somebody is watching a shutdown.
func TestATendCancelledByTheSecondSignalIsNotReportedAsAFailure(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	tending := make(chan struct{})

	var once sync.Once

	compute := &fakeCompute{holding: true}
	compute.onTend = func(ctx context.Context) error {
		// BLOCKS UNTIL THE WAIT ENDS, which is the state under test: a real Tend
		// can be inside a provider call when the signal lands.
		once.Do(func() { close(tending) })

		<-ctx.Done()

		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	read := func() string {
		logMu.Lock()
		defer logMu.Unlock()

		return logged.String()
	}

	hurry := make(chan struct{})

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// Already passed by the time the signal lands, so the overrun warning
			// is live and would fire on the way out.
			DrainTimeout: time.Nanosecond,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// INSIDE Tend WHEN THE SIGNAL ARRIVES, which is the whole point. Sending it
	// before this races the entry and the test would sometimes cover nothing.
	<-tending

	close(hurry)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the node never stopped after its second signal")
	}

	if strings.Contains(read(), "could not advance custody") {
		t.Errorf("a Tend cancelled by the shutdown was reported as a custody failure, "+
			"which is a broken-looking line at the moment an operator is watching:\n%s",
			read())
	}
}

// AND A NODE DRAIN INSIDE ITS THRESHOLD SAYS NOTHING ABOUT OVERRUNNING.
//
// The mirror of the test above, and it exists because a mutant that removed the
// threshold check walked through the whole suite: every node drain would have
// reported itself long from the first instant. The ordinary drain finishes in
// seconds, so a warning on each one is noise that buries the case the warning is
// for — a drain that has genuinely been waiting for a day.
func TestANodeDrainInsideItsThresholdIsQuiet(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		logMu  sync.Mutex
		logged bytes.Buffer
	)

	read := func() string {
		logMu.Lock()
		defer logMu.Unlock()

		return logged.String()
	}

	hurry := make(chan struct{})

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// An hour, which this test comes nowhere near.
			DrainTimeout: time.Hour,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// THE DRAIN IS GENUINELY UNDER WAY, or the silence below proves nothing: a
	// node that never drained is quiet for the wrong reason.
	waitFor(t, func() bool { return strings.Contains(read(), "draining") })
	waitFor(t, func() bool { return compute.tended() > 0 })

	// The wait loop ticks every SweepEvery, so an unthresholded warning would have
	// been written many times over by now.
	time.Sleep(300 * time.Millisecond)

	if strings.Contains(read(), nodeDrainOverrun) {
		t.Errorf("a node reported an overrunning drain within an hour-long threshold:\n%s",
			read())
	}

	close(hurry)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the node's drain")
	}
}

// A second signal ends the node's wait too. Without it, an operator who does not
// want to wait out a six-hour drain has only the signal that kills the process,
// which is the outcome the drain exists to avoid.
func TestASecondSignalEndsTheNodesDrain(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	// Holding, and never stops holding: only the hurry can end this.
	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	hurry := make(chan struct{})

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
			// An hour, so waiting it out is not something this test could do by
			// accident.
			DrainTimeout: time.Hour,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	waitFor(t, func() bool { return compute.tended() > 0 })

	select {
	case <-done:
		t.Fatal("the node stopped before it was hurried")
	case <-time.After(200 * time.Millisecond):
	}

	close(hurry)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the node's drain")
	}
}

// THE LEASES STAY RENEWED FOR THE WHOLE DRAIN, or the drain is worse than not
// draining at all.
//
// KeepAlive is what renews the leases of compute this node is holding. Its
// context used to be a child of the caller's, so the first signal stopped it at
// the exact moment the wait began — the node would sit holding containers whose
// leases nothing was renewing, the reaper would expire them, and another tier
// could escrow the same capacity while the container was still here.
//
// aliveCount() only counts janitors STARTED, which is why it could not see this.
// aliveReturned() counts the ones that have exited.
func TestTheJanitorKeepsRenewingForTheWholeDrain(t *testing.T) {
	t.Parallel()

	_, c := harness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:         testNodeVCPU,
			Memory:       testNodeMemory,
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 20 * time.Second,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// The drain is under way: custody is being advanced.
	waitFor(t, func() bool { return compute.tended() > 0 })

	if n := compute.aliveReturnedCount(); n != 0 {
		t.Fatalf("the janitor stopped renewing while the node was still draining "+
			"(%d returned); those leases are now expiring under running containers", n)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}

	// And it does stop once Run returns, so nothing heartbeats behind it.
	waitFor(t, func() bool { return compute.aliveReturnedCount() == 1 })
}

// A DRAINING NODE STILL ANSWERS THE CONTROL PLANE, and without that the drain is
// useless in the ordinary case.
//
// Tend advances CUSTODY — work this node adopted or could not account for. A job
// running normally is not custody, and what removes it is a Destroy, which
// arrives over the command poll after the control plane learns from GitHub that
// the job finished. A drain that stopped polling could never receive that, so
// Holding() would stay true until the whole grace expired: a node that always
// waits its maximum, which is the opposite of draining.
//
// It also refuses a Launch, because accepting one would extend the wait it is
// trying to finish.
func TestADrainingNodeAcceptsDestroyAndRefusesLaunch(t *testing.T) {
	t.Parallel()

	// A LONGER COMMAND CEILING THAN THE SHARED HARNESS GIVES, and only here.
	//
	// This test sends a command to a node that is DRAINING, so the answer travels
	// behind whatever tend is in flight — and MEASURED in CI, under -race on a
	// loaded runner, that took longer than the shared five seconds: it reported
	// "node n1 did not answer within 5s" about a node that was answering. The
	// bound is a ceiling on a hang rather than a deadline this test asserts, so a
	// larger one here cannot make it pass for the wrong reason.
	//
	// NOT MOVED FOR THE PACKAGE. Something else here waits on that ceiling
	// EXPIRING — measured: at thirty seconds the package does not finish inside a
	// five-minute test timeout, where at five it takes six and a half. Raising a
	// shared deadline to fix one caller is how a suite stops finishing.
	plane, c := harnessWithCommandTimeout(t, 30*time.Second)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			// What this host contributes; the control plane refuses a node offering none.
			VCPU:         testNodeVCPU,
			Memory:       testNodeMemory,
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 20 * time.Second,
		})
	}()

	waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

	cancel()

	// The drain has to be under way before either command is sent, or this tests
	// the ordinary loop rather than the draining one.
	waitFor(t, func() bool { return compute.tended() > 0 })
	// Tend and the draining poll run concurrently. Wait for the latter too, so a
	// coverage-slow scheduler cannot spend the command's whole timeout tending
	// before the goroutine that accepts control-plane work has parked.
	waitFor(t, func() bool { return plane.WaitersForTest("n1") == 1 })

	// A DESTROY IS ACCEPTED, which is the message that ends a drain in real life:
	// the job finished, GitHub told the control plane, and the control plane is
	// telling this node to tear the container down.
	if err := plane.NewRunner().Destroy(t.Context(), 42); err != nil {
		t.Fatalf("a draining node refused a destroy: %v", err)
	}

	if _, _, destroyed := compute.snapshot(); len(destroyed) == 0 {
		t.Fatal("the destroy never reached the compute; a drain that cannot be told " +
			"to destroy anything can only ever wait out its grace")
	}

	// A LAUNCH IS REFUSED. Accepting one would mean the drain never converges,
	// because each new job extends the wait it is trying to finish.
	lease := &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}

	err := plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	if err == nil {
		t.Error("a draining node accepted a launch")
	} else if !strings.Contains(err.Error(), "draining") {
		t.Errorf("the refusal should say the node is draining, got: %v", err)
	}

	if _, launched, _ := compute.snapshot(); len(launched) != 0 {
		t.Errorf("a draining node launched %v", launched)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}
}

// A POLL AND A SIGNAL CAN WAKE TOGETHER. The command has already crossed the
// wire, so dropping it is not a shutdown policy: a launch must be refused as
// draining, while a destroy must run and produce the result that lets the plane
// release the machine.
func TestACommandDeliveredAtTheShutdownBoundaryUsesDrainSemantics(t *testing.T) {
	t.Parallel()

	compute := &fakeCompute{rejectCanceledDestroy: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lease := &alloc.Lease{ID: "l1"}
	launch := nodeapi.Command{
		ID: "launch", Kind: nodeapi.CommandLaunch, Lease: lease,
		Job: &nodeapi.Job{RequestID: 7},
	}
	res := nodeclient.ExecuteForTest(ctx, compute, launch)
	if res.OK || !strings.Contains(res.Error, "draining") {
		t.Fatalf("launch result at shutdown = %+v, want a draining refusal", res)
	}
	if _, launched, _ := compute.snapshot(); len(launched) != 0 {
		t.Fatalf("shutdown-boundary launch reached compute: %v", launched)
	}

	destroy := nodeapi.Command{ID: "destroy", Kind: nodeapi.CommandDestroy,
		RequestID: 42}
	res = nodeclient.ExecuteForTest(ctx, compute, destroy)
	if !res.OK {
		t.Fatalf("destroy at shutdown = %+v, want success under a fresh context", res)
	}
	if _, _, destroyed := compute.snapshot(); len(destroyed) != 1 || destroyed[0] != 42 {
		t.Fatalf("shutdown-boundary destroy calls = %v, want [42]", destroyed)
	}
}

// A NODE STOPPED WHILE IT IS RE-REGISTERING STILL DRAINS, and only the serve
// path did.
//
// Run reaches stopGracefully from exactly one place: the return out of serve.
// Every other way the loop notices its context has ended — the registration
// call itself, the backoff after a registration that failed, the backoff after
// Recover failed, the backoff after serve returned some other error — returns
// ctx.Err() directly, and the deferred stopJanitor fires on the way out.
//
// A node in that window is not idle. The route into it is the ordinary one: the
// control plane restarts, serve returns ErrUnregistered, the loop goes back to
// Register, and Register fails because the plane has not finished coming up. The
// containers on this host are still running the whole time. Stop the node there
// and nothing renews their leases — the reaper reclaims the capacity at the TTL,
// the control plane sells it to somebody else, and a second job lands on a
// machine still running the first. The containers are never destroyed either,
// because the process that knew about them is gone.
//
// So the assertion is that the node TENDS after being stopped, which is what a
// drain does and what an immediate return cannot.
func TestANodeStoppedWhileReRegisteringStillDrains(t *testing.T) {
	t.Parallel()

	plane, c, breaker := breakableHarness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:         testNodeVCPU,
			Memory:       testNodeMemory,
			Provider:     config.ProviderDocker,
			Deployment:   deployment,
			Log:          slog.New(slog.DiscardHandler),
			Backoff:      20 * time.Millisecond,
			SweepEvery:   10 * time.Millisecond,
			DrainTimeout: 20 * time.Second,
		})
	}()

	waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

	// The control plane restarts: it forgets this node, so the poll in flight
	// comes back unregistered, and registering again does not work yet.
	breaker.failRegister.Store(true)
	plane.ForgetForTest("n1")

	// IN THE REGISTRATION RETRY, not merely on the way to it. Cancelling before
	// the loop gets there would test the serve path, which already drains.
	waitFor(t, func() bool { return breaker.registerAttempts.Load() >= 2 })

	tendedBefore := compute.tended()

	cancel()

	// A DRAIN TENDS. Nothing else in the loop does once serve has returned, so a
	// single call after the stop is the whole proof.
	waitFor(t, func() bool { return compute.tended() > tendedBefore })

	// And it is a real drain rather than one tick: it holds until the compute is
	// gone, then stops.
	select {
	case <-done:
		t.Fatal("the node stopped while it was still holding compute; its leases are now " +
			"renewed by nobody and its containers are unaccounted for")
	case <-time.After(200 * time.Millisecond):
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}
}

// A DELIBERATE STOP IS NOT A FAILURE, WHATEVER THE NODE WAS HOLDING.
//
// The cost of getting it wrong is not cosmetic. Measured on a packaged Ubuntu
// 24.04 host, `systemctl stop billet-node` left the unit Result=exit-code,
// ExecMainStatus=1, ActiveState=failed — so `systemctl is-failed billet-node`
// answered yes after every supported drain, any monitoring watching unit state
// reported a crash on a purpose-built shutdown, and the upgrade transaction in
// the host role had to either tolerate a failed unit or misread it.
//
// BOTH SHAPES, because they return from different places and the early one was
// not covered: a node holding nothing returns before the drain begins, and a
// node that drains reaches the end of stopGracefully.
func TestAStoppedNodeExitsCleanlyWhetherOrNotItWasHoldingWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// holding is what the node has when the signal lands.
		holding bool
		// finishes says whether that work ends while the drain waits. When it does
		// not, the drain has to be TOLD to stop — nothing bounds it — and the
		// compute is left running: the third way out of stopGracefully, and the one
		// a mutation showed was uncovered.
		finishes bool
		// hurried sends the second signal, which is now the only way to reach that
		// third exit. It used to be reached by giving the drain a 200ms budget and
		// waiting, which stopped being possible when the budget stopped ending
		// anything.
		hurried bool
		grace   time.Duration
	}{
		{"holding nothing", false, false, false, 20 * time.Second},
		{"draining work that then finishes", true, true, false, 20 * time.Second},
		{"a drain the operator stops waiting for", true, false, true, 20 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, c := harness(t)

			compute := &fakeCompute{holding: tc.holding}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			hurry := make(chan struct{})

			go func() {
				done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
					VCPU:         testNodeVCPU,
					Memory:       testNodeMemory,
					Provider:     config.ProviderDocker,
					Deployment:   deployment,
					Log:          slog.New(slog.DiscardHandler),
					Backoff:      20 * time.Millisecond,
					SweepEvery:   10 * time.Millisecond,
					DrainTimeout: tc.grace,
					Hurry:        hurry,
				})
			}()

			if tc.holding {
				waitFor(t, func() bool { return compute.aliveCount() == 1 })
			}

			cancel()

			if tc.finishes {
				// The work finishes while the drain waits, which is the ordinary
				// path: the node is not being asked to abandon anything.
				compute.mu.Lock()
				compute.holding = false
				compute.mu.Unlock()
			}

			if tc.hurried {
				// AFTER THE DRAIN HAS BEGUN, so this exercises the exit from a wait
				// rather than racing the entry into one.
				waitFor(t, func() bool { return compute.tended() > 0 })
				close(hurry)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("a deliberate stop returned %v; billet exits with that, so "+
						"systemd records Result=exit-code and marks the unit failed for a "+
						"shutdown that did exactly what it was asked", err)
				}

				// AND THE THIRD CASE REALLY LEFT WORK BEHIND. Without this it passes
				// by finishing early for some other reason, covering the same path as
				// the second.
				if tc.holding && !tc.finishes && !compute.Holding() {
					t.Error("the fixture's work finished after all, so this did not exercise " +
						"the exit that leaves compute running")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the node never stopped")
			}
		})
	}
}
