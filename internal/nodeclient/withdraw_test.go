package nodeclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// dockerLease is a lease the harness plane could place on n1, and on nothing else.
func dockerLease() *alloc.Lease {
	return &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
	}
}

// withdrawn reports whether the node's withdrawal reached the control plane
// and took it out of placement.
//
// THE ARRIVAL COUNT IS THE ASSERTION, and the plane's state is the consequence.
// Both Nodes and PickForTest run expiry, and the harness plane's silence window
// is 240ms — so a test that asked the plane alone could be satisfied by a slow
// scheduler forgetting the node rather than by the node withdrawing. A request
// counted at the wire cannot be produced by anything but the node sending it,
// which is what the mutation that deletes the withdrawal leaves undone.
func withdrawn(t *testing.T, plane *nodeplane.Plane, b *breaker) bool {
	t.Helper()

	if b.withdrawAttempts.Load() == 0 {
		return false
	}

	if len(plane.Nodes()) != 0 {
		return false
	}

	_, err := plane.PickForTest(dockerLease())

	return errors.Is(err, nodeplane.ErrNoNode)
}

// A STOPPED NODE WITHDRAWS FROM PLACEMENT, AND ONLY ONCE IT HOLDS NOTHING.
//
// This is the withdrawal from the node's side. A node that drained and exited
// said nothing, so the control plane went on assigning work to it for a whole
// silence window. The withdrawal is what shortens that to nothing — and it
// must come AFTER the drain, because a node that withdrew while still holding
// a running job would have the plane answer that job's destroy as
// undeliverable while the node was about to take it.
func TestAStoppedNodeWithdrawsFromPlacementOnlyAfterItHoldsNothing(t *testing.T) {
	t.Parallel()

	plane, c, b := breakableHarness(t)

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

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	// THE DRAIN IS UNDER WAY, and the node has not asked to leave: it holds
	// compute and is still answering the control plane about it.
	waitFor(t, func() bool { return compute.tended() > 0 })

	if n := b.withdrawAttempts.Load(); n != 0 {
		t.Fatalf("the node asked to withdraw %d time(s) while it was still holding compute", n)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a drained node reported a failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a drained node never stopped")
	}

	if !withdrawn(t, plane, b) {
		t.Errorf("the node stopped and the control plane still places on it: %v", plane.Nodes())
	}
}

// AN IDLE NODE WITHDRAWS TOO, on the early return that skips the drain — the
// ordinary restart, and the shape the issue was observed in.
func TestAnIdleNodeWithdrawsWhenItStops(t *testing.T) {
	t.Parallel()

	plane, c, b := breakableHarness(t)

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
		if err != nil {
			t.Fatalf("the node stopped for some reason other than the signal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an idle node did not stop")
	}

	if !withdrawn(t, plane, b) {
		t.Errorf("an idle node stopped and the control plane still places on it: %v",
			plane.Nodes())
	}

	logMu.Lock()
	defer logMu.Unlock()

	if !strings.Contains(logged.String(), "withdrew from placement") {
		t.Errorf("the node did not say it withdrew:\n%s", logged.String())
	}

	// STILL NOTHING ABOUT DRAINING: withdrawing is not a drain, and an idle
	// restart must not read as one.
	if strings.Contains(logged.String(), "draining") {
		t.Errorf("an idle node announced a drain it did not perform:\n%s", logged.String())
	}
}

// A DRAIN THE OPERATOR CUTS SHORT STILL WITHDRAWS. The compute stays running
// and its leases stay charged — that is the documented outcome of a second
// signal — but the process is not going to poll again, so nothing new may be
// aimed at it either.
func TestAHurriedDrainStillWithdraws(t *testing.T) {
	t.Parallel()

	plane, c, b := breakableHarness(t)

	compute := &fakeCompute{holding: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	hurry := make(chan struct{})
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
			DrainTimeout: time.Hour,
			Hurry:        hurry,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	waitFor(t, func() bool { return compute.tended() > 0 })

	close(hurry)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a hurried drain reported a failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the node's drain")
	}

	if !compute.Holding() {
		t.Fatal("the fixture's work finished after all, so this did not exercise the hurried exit")
	}

	if !withdrawn(t, plane, b) {
		t.Errorf("a hurried node stopped and the control plane still places on it: %v",
			plane.Nodes())
	}
}

// A SUPERSEDED PROCESS WITHDRAWS NOTHING, because the name is somebody else's
// now and a withdrawal would take the replacement out of the fleet.
//
// THE ASSERTION IS THAT NO REQUEST WAS SENT. The plane refuses a superseded
// withdrawal on its own, so a node that sent one anyway would be refused and the
// test would pass for the wrong reason; counting arrivals is what makes the
// node's own restraint observable.
func TestASupersededNodeDoesNotWithdrawItsReplacement(t *testing.T) {
	t.Parallel()

	t.Run("superseded while serving", func(t *testing.T) {
		t.Parallel()

		plane, first, b := breakableHarness(t)

		compute := &fakeCompute{holding: false}

		done := make(chan error, 1)

		go func() {
			done <- nodeclient.Run(t.Context(), first, compute, nodeclient.LoopOptions{
				VCPU:       testNodeVCPU,
				Memory:     testNodeMemory,
				Provider:   config.ProviderDocker,
				Deployment: deployment,
				Log:        slog.New(slog.DiscardHandler),
				Backoff:    20 * time.Millisecond,
				SweepEvery: 10 * time.Millisecond,
			})
		}()

		waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

		second := supersede(t, first)

		select {
		case err := <-done:
			if !errors.Is(err, nodeclient.ErrSuperseded) {
				t.Fatalf("the superseded node stopped with %v, want ErrSuperseded", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the superseded node did not stop")
		}

		if n := b.withdrawAttempts.Load(); n != 0 {
			t.Errorf("a superseded process asked to withdraw %d time(s)", n)
		}

		if got := plane.CurrentIncarnationForTest("n1"); got != second.Incarnation() {
			t.Errorf("the name resolves to %q, want the replacement %q", got, second.Incarnation())
		}
	})

	t.Run("superseded while draining", func(t *testing.T) {
		t.Parallel()

		plane, first, b := breakableHarness(t)

		compute := &fakeCompute{holding: true}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		hurry := make(chan struct{})
		done := make(chan error, 1)

		go func() {
			done <- nodeclient.Run(ctx, first, compute, nodeclient.LoopOptions{
				VCPU:         testNodeVCPU,
				Memory:       testNodeMemory,
				Provider:     config.ProviderDocker,
				Deployment:   deployment,
				Log:          slog.New(slog.DiscardHandler),
				Backoff:      20 * time.Millisecond,
				SweepEvery:   10 * time.Millisecond,
				DrainTimeout: time.Hour,
				Hurry:        hurry,
			})
		}()

		waitFor(t, func() bool { return len(plane.Nodes()) == 1 })

		cancel()

		// The draining poll is parked before the replacement arrives, so the
		// supersession is observed by the drain rather than by the loop before it.
		waitFor(t, func() bool { return compute.tended() > 0 })
		waitFor(t, func() bool { return plane.WaitersForTest("n1") == 1 })

		second := supersede(t, first)

		// The drain hands its work to custody and keeps waiting; only the second
		// signal ends it.
		waitFor(t, func() bool { return compute.supersededCount() == 1 })

		close(hurry)

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a hurried drain reported a failure: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the second signal did not end the node's drain")
		}

		if n := b.withdrawAttempts.Load(); n != 0 {
			t.Errorf("a process superseded during its drain asked to withdraw %d time(s)", n)
		}

		if got := plane.CurrentIncarnationForTest("n1"); got != second.Incarnation() {
			t.Errorf("the name resolves to %q, want the replacement %q", got, second.Incarnation())
		}
	})
}

// supersede registers a second process under the first client's name and
// control plane, and returns it.
func supersede(t *testing.T, first *nodeclient.Client) *nodeclient.Client {
	t.Helper()

	second, err := nodeclient.New(nodeclient.Options{Base: first.BaseForTest(), Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("register the replacement: %v", err)
	}

	return second
}

// A WITHDRAWAL THE CONTROL PLANE COULD NOT RECORD IS ASKED AGAIN. A ledger blip
// at the moment a node stops would otherwise cost the whole silence window it
// was about to save, for a request that is nearly free to repeat.
func TestAWithdrawalIsRetriedThroughATransientRefusal(t *testing.T) {
	t.Parallel()

	plane, c, b := breakableHarness(t)
	b.failWithdrawOnce.Store(true)

	compute := &fakeCompute{holding: false}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
		})
	}()

	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the node stopped for some reason other than the signal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node did not stop")
	}

	if n := b.withdrawAttempts.Load(); n != 2 {
		t.Errorf("the node asked to withdraw %d time(s), want 2: once refused, once accepted", n)
	}

	if !withdrawn(t, plane, b) {
		t.Errorf("the retried withdrawal never landed: %v", plane.Nodes())
	}
}

// THE CLIENT FORGETS ITS WIRE THE MOMENT IT IS TOLD IT IS UNKNOWN, on any
// route — not where the loop happens to branch on the answer, which it does
// only after checking for cancellation. A stop landing beside that answer would
// otherwise act on a version a control plane that is gone negotiated.
func TestANodeForgetsItsWireWhenTheControlPlaneDisownsIt(t *testing.T) {
	t.Parallel()

	plane, c := harness(t)

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if c.WireVersion() == 0 {
		t.Fatal("no wire was negotiated, so this proves nothing")
	}

	plane.ForgetForTest("n1")

	if _, _, err := c.Poll(t.Context()); !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("a poll from a forgotten node = %v, want ErrUnregistered", err)
	}

	if got := c.WireVersion(); got != 0 {
		t.Errorf("the client still holds wire %d after the control plane disowned it", got)
	}
}

// parkingPlane is a control plane that registers a node at the current wire,
// then parks the FIRST poll until released and answers it "unregistered" —
// which is what a heartbeat that left before a re-registration and came back
// after it looks like.
type parkingPlane struct {
	t       *testing.T
	release chan struct{}
	parked  chan struct{}
	polls   atomic.Int64
}

func (s *parkingPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/v1/register":
		// A LONG POLL WINDOW, so the parked poll's own deadline cannot expire
		// before the test releases it under load.
		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version: nodeapi.Version, LeaseTTLSeconds: 60, PollSeconds: 120,
		}); err != nil {
			s.t.Errorf("encode the registration answer: %v", err)
		}
	case strings.HasSuffix(r.URL.Path, "/poll"):
		if s.polls.Add(1) == 1 {
			close(s.parked)
			<-s.release
		}

		w.WriteHeader(http.StatusUnauthorized)

		if err := json.NewEncoder(w).Encode(nodeapi.ErrorResponse{
			Code: nodeapi.CodeUnregistered, Message: "who?",
		}); err != nil {
			s.t.Errorf("encode the refusal: %v", err)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A STALE "UNREGISTERED" ANSWER DOES NOT ERASE A NEWER REGISTRATION. A request
// that left under one registration and was answered after the next one landed
// is a fact about the old registration; clearing the wire on it would suppress
// the withdrawal at the next stop and send the node back to being forgotten by
// silence.
func TestAStaleUnregisteredAnswerDoesNotEraseANewerRegistration(t *testing.T) {
	t.Parallel()

	stub := &parkingPlane{t: t, release: make(chan struct{}), parked: make(chan struct{})}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	var releaseOnce sync.Once

	release := func() { releaseOnce.Do(func() { close(stub.release) }) }

	t.Cleanup(release)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	reg := nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}

	if err := c.Register(t.Context(), reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	polled := make(chan error, 1)

	go func() {
		_, _, err := c.Poll(t.Context())
		polled <- err
	}()

	// THE OLD REQUEST IS IN FLIGHT when the new registration lands.
	select {
	case <-stub.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the poll never reached the control plane")
	}

	if err := c.Register(t.Context(), reg); err != nil {
		t.Fatalf("register again: %v", err)
	}

	release()

	select {
	case err := <-polled:
		if !errors.Is(err, nodeclient.ErrUnregistered) {
			t.Fatalf("the parked poll = %v, want ErrUnregistered", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked poll never returned")
	}

	if got := c.WireVersion(); got != nodeapi.Version {
		t.Errorf("a stale unregistered answer left the client at wire %d, want the newer "+
			"registration's %d", got, nodeapi.Version)
	}
}

// A NODE THE CONTROL PLANE HAS FORGOTTEN SENDS NO WITHDRAWAL, because it holds
// no registration to withdraw. The case that makes this matter is a control
// plane replaced by an OLDER build between a registration and a stop: the wire
// version the node remembers was negotiated with a plane that is gone, and
// acting on it would send a request the replacement has no route for.
func TestANodeTheControlPlaneForgotSendsNoWithdrawal(t *testing.T) {
	t.Parallel()

	plane, c, b := breakableHarness(t)

	compute := &fakeCompute{holding: false}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log:        slog.New(slog.DiscardHandler),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
		})
	}()

	// REGISTERED FROM THE CLIENT'S SIDE, which the janitor starting proves: the
	// plane installs the node before the client's Register has returned and
	// stored the wire, so waiting on the plane alone raced that store.
	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	if c.WireVersion() < nodeapi.VersionNodeWithdrawal {
		t.Fatalf("negotiated wire %d, so the node would not have withdrawn anyway", c.WireVersion())
	}

	// The control plane restarts on a build that does not know this node, and
	// registering again does not work yet.
	b.failRegister.Store(true)
	plane.ForgetForTest("n1")

	// IN THE REGISTRATION RETRY, which is where the old registration has been
	// disowned and no new one has answered.
	waitFor(t, func() bool { return b.registerAttempts.Load() >= 2 })

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the node stopped for some reason other than the signal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node did not stop")
	}

	if n := b.withdrawAttempts.Load(); n != 0 {
		t.Errorf("a node the control plane had forgotten asked to withdraw %d time(s)", n)
	}

	if got := c.WireVersion(); got != 0 {
		t.Errorf("the node still holds wire %d from a registration the plane disowned", got)
	}
}

// olderPlane is a control plane from before withdrawals existed: it negotiates
// the previous wire, answers polls with nothing, and has no route to withdraw
// on — but counts arrivals there, so a node that sent one anyway is visible.
type olderPlane struct {
	t           *testing.T
	withdrawals atomic.Int64
}

func (s *olderPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()

	switch {
	case r.URL.Path == "/v1/register":
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version:         nodeapi.VersionNodeWithdrawal - 1,
			LeaseTTLSeconds: 60,
			PollSeconds:     1,
		}); err != nil {
			s.t.Errorf("encode the registration answer: %v", err)
		}
	case strings.HasSuffix(r.URL.Path, "/poll"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/withdraw"):
		s.withdrawals.Add(1)

		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A NODE NEGOTIATED BELOW THE WITHDRAWAL VERSION DOES NOT SEND ONE, and says
// why. An older control plane has no route for it and answers with a bare 404,
// which would otherwise read as a decode failure on every clean stop of every
// node behind that plane.
func TestANodeOnAnOldControlPlaneStopsWithoutWithdrawing(t *testing.T) {
	t.Parallel()

	stub := &olderPlane{t: t}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

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
			VCPU:       testNodeVCPU,
			Memory:     testNodeMemory,
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			Log: slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, w: &logged},
				&slog.HandlerOptions{Level: slog.LevelInfo})),
			Backoff:    20 * time.Millisecond,
			SweepEvery: 10 * time.Millisecond,
		})
	}()

	// Registered, which is what the janitor starting proves.
	waitFor(t, func() bool { return compute.aliveCount() == 1 })

	if got := c.WireVersion(); got != nodeapi.VersionNodeWithdrawal-1 {
		t.Fatalf("negotiated wire %d, want %d: the stub is not older than the withdrawal",
			got, nodeapi.VersionNodeWithdrawal-1)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the node stopped for some reason other than the signal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node did not stop")
	}

	if n := stub.withdrawals.Load(); n != 0 {
		t.Errorf("a node on an older control plane asked to withdraw %d time(s)", n)
	}

	logMu.Lock()
	defer logMu.Unlock()

	if !strings.Contains(logged.String(), "too old to be told") {
		t.Errorf("the node did not say why it could not withdraw:\n%s", logged.String())
	}
}
