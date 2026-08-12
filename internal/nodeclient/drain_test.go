package nodeclient_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
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
		if err == nil || !errors.Is(err, ctx.Err()) {
			t.Errorf("want the cancellation once drained, got %v", err)
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
		if !errors.Is(err, context.Canceled) {
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

// The drain is bounded. A container that never reports must not keep the process
// alive forever.
//
// The assertion is on WHY it stopped, not just that it did within ten seconds:
// an upper bound alone is satisfied by a drain that was never entered, which is
// exactly the regression worth catching.
func TestANodeDrainIsBounded(t *testing.T) {
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
			DrainTimeout: 200 * time.Millisecond,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a node drain with no bound never let the process stop")
	}

	logMu.Lock()
	defer logMu.Unlock()

	if !strings.Contains(logged.String(), "draining") {
		t.Errorf("the node never entered a drain, so its bound proves nothing:\n%s",
			logged.String())
	}

	if !strings.Contains(logged.String(), "stopped waiting") {
		t.Errorf("the drain ended for some reason other than its own bound:\n%s",
			logged.String())
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

	plane, c := harness(t)

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
