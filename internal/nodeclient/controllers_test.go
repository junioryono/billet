package nodeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeending"
	"github.com/junioryono/billet/internal/nodeplane"
)

// controller is one control plane a node may register with, served on a real
// loopback listener, and the requests it was sent by path.
type controller struct {
	plane *nodeplane.Plane
	srv   *httptest.Server

	mu       sync.Mutex
	requests map[string]int
	// drop, when it returns true for a request, cuts the connection without an
	// answer, as a controller that has gone does.
	drop func(path string, n int) bool
	// unavailable, when it returns true for a request, answers 503, as a
	// controller whose ledger blinked does.
	unavailable func(path string, n int) bool
	// supersede, when it returns true for a request, answers that another
	// process has registered as this node.
	supersede func(path string, n int) bool
}

func newController(t *testing.T) *controller {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithCommandTimeout(5*time.Second),
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))
	p.SetPollWindowForTest(60 * time.Millisecond)

	c := &controller{plane: p, requests: map[string]int{}}
	inner := nodeplane.Handler(log, p, stubStore{}, stubJIT{})

	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests[r.URL.Path]++
		n := c.requests[r.URL.Path]
		drop, unavailable, supersede := c.drop, c.unavailable, c.supersede
		c.mu.Unlock()

		if supersede != nil && supersede(r.URL.Path, n) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)

			_ = json.NewEncoder(w).Encode(nodeapi.ErrorResponse{ //nolint:errcheck // the test's own server
				Code: nodeapi.CodeSuperseded, Message: "another process is this node",
			})

			return
		}

		if unavailable != nil && unavailable(r.URL.Path, n) {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		if drop != nil && drop(r.URL.Path, n) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("the test server's writer cannot be hijacked, so %s was answered", r.URL.Path)
				http.Error(w, "cannot cut this connection", http.StatusInternalServerError)

				return
			}

			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}

			return
		}

		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(c.srv.Close)

	return c
}

func (c *controller) sent(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.requests[path]
}

// endpointOf is the canonical endpoint of a base URL.
func endpointOf(t *testing.T, base string) string {
	t.Helper()

	e, err := endpoint.Parse(base, false)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}

	return e.String()
}

// deadAddress is an address that answers nothing: every connection is closed
// as it opens. HELD FOR THE TEST rather than bound and released, so no other
// test's server can take the port and answer for it.
func deadAddress(t *testing.T) string {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()

		wg.Wait()
	})

	return ln.Addr().String()
}

var registration = nodeclient.Registration{
	Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
	Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
}

// A CONTROLLER THAT DOES NOT ANSWER IS PASSED OVER: the registration goes to
// the next, and every request after it goes there too.
func TestARegistrationPassesOverAControllerThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	b := newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: "http://" + deadAddress(t)}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got, want := c.Endpoint().String(), endpointOf(t, b.srv.URL); got != want {
		t.Fatalf("the client dials %s after registering, want %s, the controller that accepted", got, want)
	}

	if nodes := b.plane.Nodes(); len(nodes) != 1 || nodes[0] != "n1" {
		t.Fatalf("the controller that answered knows %v", nodes)
	}

	// AND THE REQUESTS AFTER IT GO THERE, not to the first controller named.
	if _, err := c.LaunchedLeaseIDs(t.Context(), "n1"); err != nil {
		t.Fatalf("LaunchedLeaseIDs: %v", err)
	}

	if n := b.sent("/v1/nodes/n1/launched"); n != 1 {
		t.Fatalf("the controller that accepted was asked %d times, want once", n)
	}
}

// A REFUSAL IS THE CONTROLLER'S VERDICT, and stops the round where it is given:
// the next controller is not asked.
func TestARefusalStopsTheRound(t *testing.T) {
	t.Parallel()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)

		_ = json.NewEncoder(w).Encode(nodeapi.ErrorResponse{ //nolint:errcheck // the test's own server
			Code: nodeapi.CodeRefused, Message: "this deployment is not yours",
		})
	}))
	t.Cleanup(refusing.Close)

	b := newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: refusing.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Register(t.Context(), registration); !errors.Is(err, nodeclient.ErrRefused) {
		t.Fatalf("Register = %v, want the refusal", err)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("the next controller was asked %d times after a refusal", n)
	}
}

// A CONTROLLER LOST WHILE SERVING IS LEFT FOR THE ONE THAT ANSWERS. The node is
// registered with A; A stops answering; two polls go unanswered, and the node
// registers again, passing over A to B, which every request then goes to.
func TestANodeLeavesALostControllerForTheOther(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runLoop(t, c, &fakeCompute{})

	waitFor(t, func() bool { return len(a.plane.Nodes()) == 1 })

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("B was asked to register the node %d times while A served it", n)
	}

	// A GOES, without a word: every connection cut, and nothing listens after.
	a.srv.CloseClientConnections()
	a.srv.Close()

	// THE CLIENT'S SIDE, not B's: B records the registration before the
	// client has read its answer and made B the controller it dials.
	want := endpointOf(t, b.srv.URL)
	waitFor(t, func() bool { return c.Endpoint().String() == want })

	if nodes := b.plane.Nodes(); len(nodes) != 1 || nodes[0] != "n1" {
		t.Fatalf("B knows %v after the node moved there", nodes)
	}

	// AND IT POLLS B, where its commands now come from.
	waitFor(t, func() bool { return b.sent("/v1/nodes/n1/poll") >= 1 })
}

// A BLINK IS NOT A LOSS. One poll the controller does not answer, then answers,
// then another it does not: the node asks again each time and never registers
// anywhere else, because only polls unanswered in a row mean it has gone.
func TestABlinkDoesNotSendTheNodeElsewhere(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	poll := "/v1/nodes/n1/poll"

	a.mu.Lock()
	a.drop = func(path string, n int) bool { return path == poll && (n == 2 || n == 4) }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runLoop(t, c, &fakeCompute{})

	// PAST BOTH DROPPED POLLS, and the answered ones beside them.
	waitFor(t, func() bool { return a.sent(poll) >= 7 })

	if n := a.sent("/v1/register"); n != 1 {
		t.Fatalf("the node registered with A %d times across two separate blinks, want once", n)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("a blink sent the node to B %d times", n)
	}
}

// AN ANSWER BETWEEN TWO SILENCES IS AN ANSWER. A poll the controller does not
// answer, one it answers with a 503, and another it does not answer are not
// two in a row: the controller said it was there, and the node stays.
func TestAnAnsweredErrorBetweenTwoSilencesIsNotALoss(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	poll := "/v1/nodes/n1/poll"

	a.mu.Lock()
	a.drop = func(path string, n int) bool { return path == poll && (n == 2 || n == 4) }
	a.unavailable = func(path string, n int) bool { return path == poll && n == 3 }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runLoop(t, c, &fakeCompute{})

	waitFor(t, func() bool { return a.sent(poll) >= 7 })

	if n := a.sent("/v1/register"); n != 1 {
		t.Fatalf("the node registered with A %d times when A answered between two silences, want once", n)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("an answered error between two silences sent the node to B %d times", n)
	}
}

// A NODE WITH SEVERAL CONTROLLERS ASKS AGAIN EVERY SECOND, because every round
// it waits after a promotion is a round no node is registered; with one it
// keeps the pace it always had, and a pace the operator set is kept either way.
func TestANodeWithSeveralControllersRetriesSooner(t *testing.T) {
	t.Parallel()

	one, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: "http://" + deadAddress(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	two, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: "http://" + deadAddress(t)}, {Base: "http://" + deadAddress(t)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name    string
		c       *nodeclient.Client
		backoff time.Duration
		want    time.Duration
	}{
		{"one controller", one, 0, 5 * time.Second},
		{"two controllers", two, 0, time.Second},
		{"two controllers, the operator's pace", two, 7 * time.Second, 7 * time.Second},
	} {
		if got := nodeclient.BackoffForTest(nodeclient.LoopOptions{Backoff: tc.backoff}, tc.c); got != tc.want {
			t.Errorf("%s: the node waits %v after a failed registration, want %v", tc.name, got, tc.want)
		}
	}
}

// AND THE LOOP KEEPS THAT PACE: two controllers that both turn the
// registration away, and the node's second round at the first comes about a
// second after its first, not five. The bound is a failure guard well clear of
// both paces.
func TestTheLoopRetriesEverySecondWithSeveralControllers(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		rounds []time.Time
	)

	unavailable := func(record bool) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if record && r.URL.Path == "/v1/register" {
				mu.Lock()
				rounds = append(rounds, time.Now())
				mu.Unlock()
			}

			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		return srv
	}

	a, b := unavailable(true), unavailable(false)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.URL}, {Base: b.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		// THE DEFAULT PACE IS WHAT IS UNDER TEST, so Backoff is left zero.
		err := nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
			GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler),
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	})
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)

	for {
		mu.Lock()
		n := len(rounds)
		mu.Unlock()

		if n >= 2 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the node asked the first controller %d times in 15s", n)
		}

		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	gap := rounds[1].Sub(rounds[0])
	mu.Unlock()

	// AT LEAST THE SECOND, which load can only lengthen, so a loop that does not
	// wait at all fails here; the upper bound is the guard against five.
	if gap < time.Second || gap > 3*time.Second {
		t.Fatalf("the node waited %v between rounds of two controllers, want about a second", gap)
	}
}

// runDraining runs the loop with a drain requested at every stop, and returns
// the cancellation that begins the stop, the channel Run's error arrives on and
// the endings the stop recorded. A DRAIN OUTLIVES ITS CALLER'S CONTEXT by
// design, so the test's cleanup also hurries it and joins it, before the
// controllers it talks to are closed: a test that fails mid-drain leaves
// nothing running.
func runDraining(
	t *testing.T, c *nodeclient.Client, compute nodeclient.Compute,
) (context.CancelFunc, <-chan error, func() []nodeending.Ending) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	hurry := make(chan struct{})
	done := make(chan error, 1)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		endings []nodeending.Ending
	)

	wg.Go(func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU:           testNodeVCPU,
			Memory:         testNodeMemory,
			Provider:       config.ProviderDocker,
			Deployment:     deployment,
			Log:            slog.New(slog.DiscardHandler),
			Backoff:        20 * time.Millisecond,
			SweepEvery:     10 * time.Millisecond,
			DrainTimeout:   time.Hour,
			DrainRequested: func() bool { return true },
			Hurry:          hurry,
			Ended: func(ending nodeending.Ending, _ bool) {
				mu.Lock()
				endings = append(endings, ending)
				mu.Unlock()
			},
		})
	})
	t.Cleanup(func() {
		cancel()
		close(hurry)
		wg.Wait()
	})

	return cancel, done, func() []nodeending.Ending {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(endings)
	}
}

// askingCompute recovers the way the node runtime does, by asking the control
// plane which leases this node launched, so a controller that gives no answer
// fails the recovery with the client's own error.
type askingCompute struct {
	*fakeCompute

	c *nodeclient.Client
}

func (a *askingCompute) Recover(ctx context.Context) error {
	if _, err := a.c.LaunchedLeaseIDs(ctx, "n1"); err != nil {
		return fmt.Errorf("list the leases this node launched: %w", err)
	}

	return a.fakeCompute.Recover(ctx)
}

// A DRAIN THAT RECOVERS ACROSS A FAILOVER RECOVERS FROM THE OTHER CONTROLLER.
// The process registered with A and stopped before its first recovery, and A
// then went: the recovery a requested drain insists on registers again, which
// passes over A to B, rather than asking A for as long as the drain lasts.
func TestADrainRecoveringAcrossAFailoverRegistersWithTheOther(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	const launched = "/v1/nodes/n1/launched"

	// A ACCEPTS THE REGISTRATION AND NEVER ANSWERS THE RECOVERY, so the process
	// is registered with A and has never recovered; then, once gone is set,
	// A answers nothing at all.
	var gone atomic.Bool

	a.mu.Lock()
	a.drop = func(path string, _ int) bool { return gone.Load() || path == launched }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cancel, done, _ := runDraining(t, c, &askingCompute{fakeCompute: &fakeCompute{}, c: c})

	waitFor(t, func() bool { return a.sent(launched) >= 2 })

	// STOPPED BEFORE A GOES, so the stop is what recovers: had A gone first, the
	// ordinary loop would have registered with B and recovered there.
	cancel()
	gone.Store(true)

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("the drain ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the drain was still recovering after 15s; it asked A %d times and B %d",
			a.sent(launched), b.sent(launched))
	}

	if b.sent("/v1/register") == 0 || b.sent(launched) == 0 {
		t.Fatalf("the drain recovered without B: B was asked to register %d times and to list leases %d",
			b.sent("/v1/register"), b.sent(launched))
	}
}

// A RECOVERY SUPERSEDED BEFORE A DRAIN STOPS ASKING. Another process took this
// node's name while this one was still recovering, and the recovery a
// requested drain insists on stops at that verdict: asking again would be
// refused for as long as the drain lasted, and registering again would take
// the name back from the host that holds it.
func TestARecoverySupersededBeforeADrainStopsAsking(t *testing.T) {
	t.Parallel()

	a := newController(t)

	const launched = "/v1/nodes/n1/launched"

	a.mu.Lock()
	a.supersede = func(path string, _ int) bool { return path == launched }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cancel, done, endings := runDraining(t, c, &askingCompute{fakeCompute: &fakeCompute{}, c: c})

	waitFor(t, func() bool { return a.sent(launched) >= 2 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the stop ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the stop was still recovering 10s after it was superseded; it asked %d times", a.sent(launched))
	}

	assertSupersededStop(t, a, endings())
}

// assertSupersededStop holds a stop superseded before its drain to what it
// may and may not have done: no withdrawal, since the name is another
// process's and the plane places on it, and one ending, superseded.
func assertSupersededStop(t *testing.T, a *controller, endings []nodeending.Ending) {
	t.Helper()

	if n := a.sent("/v1/nodes/n1/withdraw"); n != 0 {
		t.Fatalf("a superseded stop asked %d times to withdraw a name another process holds", n)
	}

	if !slices.Equal(endings, []nodeending.Ending{nodeending.Superseded}) {
		t.Fatalf("the stop recorded %v, want one superseded ending", endings)
	}
}

// AND A STOP SUPERSEDED THERE STAYS SUPERSEDED. The process holds compute when
// its recovery is superseded, so it waits for what runs here. It hands what it
// runs to custody once, and its drain serves nothing: a worker that polled
// would, after a failover, register with the next controller and take the
// name back from the process that holds it.
//
// SERIAL, because the drain worker's completion hook is the package's.
func TestAStopSupersededBeforeItsDrainNeverRegistersAgain(t *testing.T) {
	a, b := newController(t), newController(t)

	const launched = "/v1/nodes/n1/launched"

	a.mu.Lock()
	a.supersede = func(path string, _ int) bool { return path == launched }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	served := make(chan struct{})
	t.Cleanup(nodeclient.SetDrainServingDoneForTest(sync.OnceFunc(func() { close(served) })))

	compute := &askingCompute{fakeCompute: &fakeCompute{holding: true}, c: c}
	cancel, done, endings := runDraining(t, c, compute)

	waitFor(t, func() bool { return a.sent(launched) >= 2 })

	polls := a.sent("/v1/nodes/n1/poll")
	cancel()

	// THE DRAIN'S WORKER RETURNS WHILE WHAT RUNS HERE IS STILL HELD, which it
	// does only when it serves nothing: one that served would return only when
	// the drain ended.
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain's worker was still serving 10s into a stop superseded before it")
	}

	if n := a.sent("/v1/nodes/n1/poll") - polls; n != 0 {
		t.Fatalf("a superseded stop polled %d times", n)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("a superseded stop registered with B %d times", n)
	}

	if n := compute.supersededCount(); n != 1 {
		t.Fatalf("what runs here was handed to custody %d times, want once", n)
	}

	compute.setHolding(false)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the stop ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stop did not end once nothing was held")
	}

	assertSupersededStop(t, a, endings())
}

// A DRAIN THAT LOSES ITS CONTROLLER IS SERVED BY THE OTHER. The node is
// draining compute that is still running, which only a destroy from the control
// plane ends, so it keeps polling; A goes, and the drain registers with B and
// polls there, where the destroy will come from, asking B again when its first
// registration fails rather than going back to A.
func TestADrainThatLosesItsControllerIsServedByTheOther(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	var gone atomic.Bool

	a.mu.Lock()
	a.drop = func(string, int) bool { return gone.Load() }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := &fakeCompute{holding: true}

	cancel, done, _ := runDraining(t, c, compute)

	poll := "/v1/nodes/n1/poll"

	waitFor(t, func() bool { return a.sent(poll) >= 1 })

	// THE DRAIN BEGINS, and polls A while what runs here keeps running.
	cancel()

	polled := a.sent(poll)
	waitFor(t, func() bool { return a.sent(poll) > polled })

	// B TURNS THE FIRST ROUND AWAY, so the drain has a failed registration to
	// answer: it asks again rather than going back to polling A, which would
	// take two more unanswered polls to give up on a second time.
	b.mu.Lock()
	b.unavailable = func(path string, n int) bool { return path == "/v1/register" && n == 1 }
	b.mu.Unlock()

	atGone := a.sent(poll)
	gone.Store(true)

	waitFor(t, func() bool { return b.sent("/v1/register") >= 2 && b.sent(poll) >= 1 })

	// THE POLL IN FLIGHT WHEN A WENT, AND THE TWO THAT FOUND IT GONE.
	if n := a.sent(poll) - atGone; n > 3 {
		t.Fatalf("the drain polled A %d times after it went, so a failed registration sent it back to A", n)
	}

	compute.mu.Lock()
	compute.holding = false
	compute.mu.Unlock()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the drain did not end once nothing was held")
	}
}

// THE NODE'S CONNECTIONS NOTICE A SILENT CONTROLLER IN SECONDS. A controller
// cut off the network sends nothing, not even a reset, and a long poll to it
// waited out the poll window and its slack: the probes are what end it, in
// about Idle + Interval x Count, and a dial that is not answered gives up
// while another controller may be serving. The rehearsal measures the effect
// on a real partition (docs/reference/records/host-rehearsals.md).
func TestTheNodesDialerNoticesASilentController(t *testing.T) {
	t.Parallel()

	b := newController(t)

	addrs := []string{deadAddress(t), strings.TrimPrefix(b.srv.URL, "http://")}

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: "http://" + addrs[0]}, {Base: "http://" + addrs[1]},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// WHERE EACH CONTROLLER'S DIALER DIALLED, by the controller's index.
	var (
		mu      sync.Mutex
		dialled = map[int][]string{}
	)

	// THE DIALER EACH CONTROLLER'S TRANSPORT DIALS WITH, as the client built it.
	for i := range c.Controllers() {
		d := nodeclient.DialerForTest(c, i)
		if d == nil {
			t.Fatalf("controller %d's transport dials with no dialer of the client's", i)
		}

		k := d.KeepAliveConfig
		if !k.Enable {
			t.Fatal("the node's connections are not probed, so a silent controller holds a poll for its whole window")
		}

		// EACH VALUE SET, because zero is the system's default (15s, 15s and 9),
		// which would read here as a bound it is not.
		if k.Idle <= 0 || k.Interval <= 0 || k.Count <= 0 {
			t.Fatalf("the probes are %+v; a zero is the system's default, not this bound", k)
		}

		if silent := k.Idle + k.Interval*time.Duration(k.Count); silent > 15*time.Second {
			t.Fatalf("a silent controller is noticed after %v, want within 15s", silent)
		}

		if d.Timeout <= 0 || d.Timeout > 5*time.Second {
			t.Fatalf("a dial waits %v for an address that does not answer, want a bound within 5s", d.Timeout)
		}

		// MARKED, so the requests below can show this is the dialer they use:
		// the transport holds a method of this same dialer, not a copy of it.
		d.Control = func(_, address string, _ syscall.RawConn) error {
			mu.Lock()
			dialled[i] = append(dialled[i], address)
			mu.Unlock()

			return nil
		}
	}

	// A REGISTRATION DIALS THE FIRST, which does not answer, then the second.
	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for i, addr := range addrs {
		if len(dialled[i]) == 0 {
			t.Fatalf("controller %d was dialled without the dialer inspected above", i)
		}

		for _, got := range dialled[i] {
			if got != addr {
				t.Fatalf("controller %d's dialer dialled %s, want only its own controller %s", i, got, addr)
			}
		}
	}
}

// A NODE WITH ONE CONTROLLER DIALS AS IT ALWAYS DID: ten seconds for the whole
// dial, its name included, and Go's default keepalive. The bounds a failover
// needs are for a node that has somewhere else to go.
func TestANodeWithOneControllerDialsAsItAlwaysDid(t *testing.T) {
	t.Parallel()

	b := newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: b.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	d := nodeclient.DialerForTest(c, 0)
	if d == nil {
		t.Fatal("the client built no dialer of its own")
	}

	if d.Timeout != 10*time.Second || d.KeepAliveConfig != (net.KeepAliveConfig{}) || d.KeepAlive != 0 {
		t.Fatalf("a node with one controller dials with %v and keepalive %+v (%v), want 10s and Go's default",
			d.Timeout, d.KeepAliveConfig, d.KeepAlive)
	}

	// AND IT IS THE DIALER THE TRANSPORT DIALS WITH: the transport holds a
	// method of this same dialer, so a mark on it shows on a real connection.
	var dialled atomic.Int32

	d.Control = func(string, string, syscall.RawConn) error {
		dialled.Add(1)

		return nil
	}

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if dialled.Load() == 0 {
		t.Fatal("the transport dialled without the dialer inspected above")
	}
}

// A NAME SLOW TO LOOK UP IS NOT A CONTROLLER THAT DOES NOT ANSWER. The static
// build's resolver waits five seconds on a nameserver that is down before it
// asks the next, which is past the dial's three; the lookup has a bound of its
// own, so the controller behind it is still reached.
func TestASlowLookupIsNotCutByTheDialBound(t *testing.T) {
	t.Parallel()

	b := newController(t)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", b.srv.URL, err)
	}

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: "http://control-b.example:" + port}, {Base: "http://" + deadAddress(t)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// PAST THE DIAL'S BOUND AND WITHIN THE LOOKUP'S, as a dead first nameserver
	// is.
	nodeclient.SetLookupForTest(c, 0, func(ctx context.Context, _, host string) ([]netip.Addr, error) {
		if host != "control-b.example" {
			return nil, fmt.Errorf("looked up %q", host)
		}

		slow := time.NewTimer(4 * time.Second)
		defer slow.Stop()

		select {
		case <-slow.C:
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register behind a four-second lookup: %v", err)
	}

	if n := b.sent("/v1/register"); n != 1 {
		t.Fatalf("the controller behind the slow lookup was asked %d times, want once", n)
	}
}

// A FAMILY THAT DOES NOT ROUTE IS RACED, NOT WAITED OUT. The name answers an
// IPv6 address first, which never answers, and an IPv4 one that does: the
// IPv4 attempt starts while the IPv6 one is still waiting, as the standard
// dialer's would, and the IPv6 attempt is ended once IPv4 connects, rather
// than every new connection waiting out the dial's bound for each address
// that does not route.
func TestAnAddressFamilyThatDoesNotRouteIsRaced(t *testing.T) {
	t.Parallel()

	b := newController(t)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", b.srv.URL, err)
	}

	c, err := nodeclient.New(nodeclient.Options{
		Node: "n1", RequestTimeout: 5 * time.Second,
		Targets: []nodeclient.Target{{Base: "http://control-b.example:" + port}, {Base: "http://" + deadAddress(t)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	nodeclient.SetLookupForTest(c, 0, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("127.0.0.1")}, nil
	})

	var (
		v6Waiting, raced atomic.Bool
		v6Ended          = make(chan struct{})
	)

	nodeclient.SetConnectForTest(c, 0, func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		if host == "2001:db8::1" {
			v6Waiting.Store(true)

			// A BLACKHOLE: nothing answers until the attempt is called off.
			<-ctx.Done()
			close(v6Ended)

			return nil, ctx.Err()
		}

		// STILL WAITING, not only started: a dial that gave IPv6 a bound of its
		// own and tried IPv4 after it would have ended the IPv6 attempt first.
		select {
		case <-v6Ended:
		default:
			raced.Store(v6Waiting.Load())
		}

		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	})

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register behind an IPv6 address that does not route: %v", err)
	}

	if !raced.Load() {
		t.Fatal("the IPv4 address was tried only after the IPv6 one gave up, not beside it")
	}

	select {
	case <-v6Ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the IPv6 attempt was still waiting after IPv4 connected")
	}
}

// THE ADDRESSES A NAME ANSWERS SHARE ONE BOUND, as the standard dialer's do: a
// name whose three addresses never answer costs the dial's three seconds
// before the next controller is tried, not three seconds an address.
func TestTheAddressesOfANameShareOneBound(t *testing.T) {
	t.Parallel()

	b := newController(t)

	c, err := nodeclient.New(nodeclient.Options{
		Node: "n1", RequestTimeout: 15 * time.Second,
		Targets: []nodeclient.Target{{Base: "http://control-a.example:7717"}, {Base: b.srv.URL}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	nodeclient.SetLookupForTest(c, 0, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.3"),
		}, nil
	})

	var (
		mu        sync.Mutex
		first     time.Time
		deadlines []time.Time
	)

	nodeclient.SetConnectForTest(c, 0, func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()

		mu.Lock()
		if first.IsZero() {
			first = time.Now()
		}

		if ok {
			deadlines = append(deadlines, deadline)
		}
		mu.Unlock()

		// A BLACKHOLE: nothing answers until the attempt's bound.
		<-ctx.Done()

		return nil, ctx.Err()
	})

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register past a name whose addresses never answer: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(deadlines) == 0 {
		t.Fatal("the attempts at the name's addresses had no deadline")
	}

	// THE LAST ATTEMPT ENDS WITHIN THE ONE BOUND of the first's start, with a
	// little for the time between taking the bound and the first attempt.
	limit := first.Add(3*time.Second + 250*time.Millisecond)
	for i, deadline := range deadlines {
		if deadline.After(limit) {
			t.Fatalf("attempt %d of %d was bounded %v after the first began, past the shared 3s",
				i+1, len(deadlines), deadline.Sub(first))
		}
	}
}

// A LINK-LOCAL CONTROLLER KEEPS ITS INTERFACE. A name the hosts file gives an
// IPv6 address with a zone is reached only through that zone, which the
// resolver's netip answer drops; a lookup for one family answers that family.
func TestALookupKeepsZonesAndFamilies(t *testing.T) {
	t.Parallel()

	ips := []net.IPAddr{
		{IP: net.ParseIP("fe80::1234"), Zone: "eth0"},
		{IP: net.ParseIP("10.0.0.1")},
		{IP: net.ParseIP("::ffff:10.0.0.2")},
	}

	for network, want := range map[string][]string{
		"ip":  {"fe80::1234%eth0", "10.0.0.1", "10.0.0.2"},
		"ip4": {"10.0.0.1", "10.0.0.2"},
		"ip6": {"fe80::1234%eth0"},
	} {
		var got []string
		for _, addr := range nodeclient.AddrsInForTest(ips, network) {
			got = append(got, addr.String())
		}

		if !slices.Equal(got, want) {
			t.Errorf("%s: the lookup answered %v, want %v", network, got, want)
		}
	}
}

// gate is a transport that can hold a request to one path and answer it as the
// test chooses, or fail every request as a controller that has gone does.
type gate struct {
	next http.RoundTripper

	mu      sync.Mutex
	down    bool
	holding string
	held    chan struct{}
	release chan *http.Response
}

func (g *gate) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	down, hold := g.down, g.holding != "" && strings.HasSuffix(r.URL.Path, g.holding)
	g.mu.Unlock()

	if hold {
		close(g.held)

		select {
		case res := <-g.release:
			return res, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}

	if down {
		return nil, errors.New("injected: the controller does not answer")
	}

	return g.next.RoundTrip(r)
}

// AN ANSWER FROM THE CONTROLLER A NODE LEFT DOES NOT UNDO ITS REGISTRATION WITH
// THE ONE IT MOVED TO. A request sent to A while the node was registered there
// comes back "unregistered" after the node has registered with B: it is about
// A's registration, which is gone, and B's stands.
func TestALateAnswerFromTheOldControllerLeavesTheNewRegistration(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	toA := &gate{next: http.DefaultTransport, held: make(chan struct{}), release: make(chan *http.Response)}

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL, HTTP: &http.Client{Transport: toA}}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got, want := c.Endpoint().String(), endpointOf(t, a.srv.URL); got != want {
		t.Fatalf("registered with %s, want A (%s)", got, want)
	}

	toA.mu.Lock()
	toA.holding = "/launched"
	toA.mu.Unlock()

	late := make(chan error, 1)

	// ITS OWN CONTEXT, ended when the test is, so a test that fails before it
	// releases the held request does not leave this goroutine waiting on it.
	lateCtx, endLate := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		_, err := c.LaunchedLeaseIDs(lateCtx, "n1")
		late <- err
	})
	t.Cleanup(func() {
		endLate()
		wg.Wait()
	})

	select {
	case <-toA.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the request to A was never held, so there is no late answer to give")
	}

	toA.mu.Lock()
	toA.holding, toA.down = "", true
	toA.mu.Unlock()

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register with A gone: %v", err)
	}

	if got, want := c.Endpoint().String(), endpointOf(t, b.srv.URL); got != want {
		t.Fatalf("registered with %s, want B (%s)", got, want)
	}

	wire := c.WireVersion()

	body, err := json.Marshal(nodeapi.ErrorResponse{Code: nodeapi.CodeUnregistered, Message: "who?"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	toA.release <- &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}

	if err := <-late; !errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("the late request answered %v, want A's unregistered", err)
	}

	if wire == 0 || c.WireVersion() != wire {
		t.Fatalf("B's registration negotiated wire %d and now holds %d after A's late answer", wire, c.WireVersion())
	}
}

// A ROUND STARTS AT THE CONTROLLER THE NODE IS REGISTERED WITH. The node is
// registered with B, the second named, after A turned it away; asked again,
// it asks B first, and A, which would accept now, is not asked.
func TestARoundStartsAtTheCurrentController(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	var refusing atomic.Bool

	refusing.Store(true)

	a.mu.Lock()
	a.unavailable = func(path string, _ int) bool { return refusing.Load() && path == "/v1/register" }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got, want := c.Endpoint().String(), endpointOf(t, b.srv.URL); got != want {
		t.Fatalf("registered with %s, want B (%s)", got, want)
	}

	refusing.Store(false)

	if err := c.Register(t.Context(), registration); err != nil {
		t.Fatalf("Register again: %v", err)
	}

	if n := a.sent("/v1/register"); n != 1 {
		t.Fatalf("A was asked %d times; the second round started there rather than at B", n)
	}

	if got, want := c.Endpoint().String(), endpointOf(t, b.srv.URL); got != want {
		t.Fatalf("registered with %s after the second round, want B (%s)", got, want)
	}
}

// SUPERSESSION IS A VERDICT TOO: another process holds the name, which no other
// controller can change, so the round stops where it is said.
func TestASupersessionStopsTheRound(t *testing.T) {
	t.Parallel()

	superseding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)

		_ = json.NewEncoder(w).Encode(nodeapi.ErrorResponse{ //nolint:errcheck // the test's own server
			Code: nodeapi.CodeSuperseded, Message: "another process is this node",
		})
	}))
	t.Cleanup(superseding.Close)

	b := newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: superseding.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Register(t.Context(), registration); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Fatalf("Register = %v, want the supersession", err)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("the next controller was asked %d times after a supersession", n)
	}
}

// THE CONTROLLERS ARE NAMED ONE WAY, AND EACH ONCE. Targets beside the
// one-controller form, an empty list, and one controller spelled twice are
// each refused, the last by the one representation of an endpoint, which
// sees through spellings the configuration's own check does not.
func TestTheControllersAreNamedOnceAndOneWay(t *testing.T) {
	t.Parallel()

	two := []nodeclient.Target{{Base: "http://control-a.example:7717"}, {Base: "http://control-b.example:7717"}}

	for name, tc := range map[string]struct {
		opts nodeclient.Options
		want string
	}{
		"targets beside a base": {
			nodeclient.Options{Node: "n1", Targets: two, Base: "http://control-a.example:7717"},
			"both Targets and Base, TLS or HTTP",
		},
		"targets beside a client": {
			nodeclient.Options{Node: "n1", Targets: two, HTTP: http.DefaultClient},
			"both Targets and Base, TLS or HTTP",
		},
		"an empty list": {
			nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{}},
			"a control plane address is required",
		},
		"one controller twice": {
			nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
				{Base: "http://control-a.example:7717"}, {Base: "http://CONTROL-A.example:7717"},
			}},
			"is named twice",
		},
	} {
		if _, err := nodeclient.New(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, want an error saying %q", name, err, tc.want)
		}
	}
}

// closeCounter is a transport that counts the times its idle connections were
// closed.
type closeCounter struct {
	http.RoundTripper

	closed atomic.Int32
}

func (c *closeCounter) CloseIdleConnections() { c.closed.Add(1) }

// ONLY SILENCE IS NO ANSWER. A controller that answers with an error answered;
// the caller's own cancellation says nothing about the controller; a
// connection cut with nothing said is no answer, and where the node knows
// another controller the lost one's idle connections are closed with it, so
// the next request dials rather than borrowing one a partition left open.
func TestOnlySilenceIsNoAnswer(t *testing.T) {
	t.Parallel()

	answering := newController(t)

	answering.mu.Lock()
	answering.unavailable = func(path string, _ int) bool { return path == "/v1/nodes/n1/launched" }
	answering.mu.Unlock()

	arrived := make(chan struct{}, 1)

	hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}

		<-r.Context().Done()
	}))
	t.Cleanup(hanging.Close)

	ask := func(t *testing.T, ctx context.Context, targets ...nodeclient.Target) error {
		t.Helper()

		c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: targets})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		_, err = c.LaunchedLeaseIDs(ctx, "n1")
		if err == nil {
			t.Fatal("the request succeeded; each case here is a failure")
		}

		return err
	}

	t.Run("an answered error", func(t *testing.T) {
		t.Parallel()

		err := ask(t, t.Context(), nodeclient.Target{Base: answering.srv.URL}, nodeclient.Target{Base: "http://" + deadAddress(t)})
		if nodeclient.IsNoAnswerForTest(err) {
			t.Fatalf("a 503 counted as no answer: %v", err)
		}
	})

	// A REFUSED REDIRECT is the one error net/http returns with a response:
	// the controller answered, with a redirect the client would not follow.
	t.Run("an error that came with an answer", func(t *testing.T) {
		t.Parallel()

		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		t.Cleanup(redirecting.Close)

		refuses := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("this client follows no redirect")
		}}

		err := ask(t, t.Context(),
			nodeclient.Target{Base: redirecting.URL, HTTP: refuses}, nodeclient.Target{Base: "http://" + deadAddress(t)})
		if nodeclient.IsNoAnswerForTest(err) {
			t.Fatalf("a redirect the client refused counted as no answer: %v", err)
		}
	})

	t.Run("the caller's cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())

		go func() {
			<-arrived
			cancel()
		}()

		err := ask(t, ctx, nodeclient.Target{Base: hanging.URL}, nodeclient.Target{Base: "http://" + deadAddress(t)})
		if nodeclient.IsNoAnswerForTest(err) || !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled request answered %v, want the cancellation and not no answer", err)
		}
	})

	for name, tc := range map[string]struct {
		others int
		closes bool
	}{
		"silence, with another controller": {1, true},
		"silence, with none":               {0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lost := &closeCounter{RoundTripper: http.DefaultTransport}

			targets := []nodeclient.Target{{Base: "http://" + deadAddress(t), HTTP: &http.Client{Transport: lost}}}
			for range tc.others {
				targets = append(targets, nodeclient.Target{Base: "http://" + deadAddress(t)})
			}

			if err := ask(t, t.Context(), targets...); !nodeclient.IsNoAnswerForTest(err) {
				t.Fatalf("a cut connection answered %v, want no answer", err)
			}

			if closed := lost.closed.Load() > 0; closed != tc.closes {
				t.Fatalf("the lost controller's idle connections closed: %v, want %v", closed, tc.closes)
			}
		})
	}
}
