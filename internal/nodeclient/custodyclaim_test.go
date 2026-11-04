package nodeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeending"
)

const (
	custodyRoute = "/v1/nodes/n1/custody"
	pollRoute    = "/v1/nodes/n1/poll"
)

func registerClient(t *testing.T, c *nodeclient.Client) {
	t.Helper()

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func twoControllers(t *testing.T) (*controller, *controller, *nodeclient.Client) {
	t.Helper()

	a, b := newController(t), newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	registerClient(t, c)

	return a, b, c
}

func goAway(a *controller) {
	a.srv.CloseClientConnections()
	a.srv.Close()
}

// A SUPERSEDED PROCESS FOLLOWS THE DEPLOYMENT WITHOUT TAKING THE NAME. It passes over
// the controller that went to the one that answers, and its lease calls go there from
// then on, and it never registers: a registration would take the name back from the
// process that holds it.
func TestACustodyClaimFollowsTheControllerThatAnswersWithoutRegistering(t *testing.T) {
	t.Parallel()

	a, b, c := twoControllers(t)

	wire := c.WireVersion()

	goAway(a)

	if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
		t.Fatalf("ClaimCustody: %v", err)
	}

	if got := c.Endpoint().String(); got != endpointOf(t, b.srv.URL) {
		t.Fatalf("the process is on %s, want B", got)
	}

	if n := b.sent(custodyRoute); n != 1 {
		t.Fatalf("B was asked %d times", n)
	}

	if n := b.sent("/v1/register"); n != 0 {
		t.Fatalf("a superseded process registered with B %d times", n)
	}

	if got := c.WireVersion(); got != wire {
		t.Fatalf("a controller speaking the same moved the wire from %d to %d", wire, got)
	}

	const heartbeat = "/v1/nodes/n1/leases/l1/heartbeat"

	if err := c.Heartbeat(t.Context(), "l1", 1); err == nil {
		t.Log("B answered the heartbeat")
	}

	if n := b.sent(heartbeat); n != 1 {
		t.Fatalf("the next lease call reached B %d times, want once", n)
	}
}

// AN ANSWER THAT IS NOT SUCCESS IS THAT CONTROLLER'S WORD, and only silence moves on.
// A refusal from B leaves the process on A, and A's refusal is not passed over to ask
// B: a controller that answered is the one serving.
func TestACustodyClaimMovesOnlyOnSilenceAndOnSuccess(t *testing.T) {
	t.Parallel()

	t.Run("the next controller refuses", func(t *testing.T) {
		t.Parallel()

		a, b, c := twoControllers(t)

		b.mu.Lock()
		b.unavailable = func(path string, _ int) bool { return path == custodyRoute }
		b.mu.Unlock()

		goAway(a)

		_, err := c.ClaimCustody(t.Context(), []string{"l1"})
		if err == nil || nodeclient.IsNoAnswerForTest(err) || !strings.Contains(err.Error(), "503") {
			t.Fatalf("ClaimCustody = %v, want B's refusal", err)
		}

		if n := b.sent(custodyRoute); n != 1 {
			t.Fatalf("B was asked %d times", n)
		}

		if got := c.Endpoint().String(); got != endpointOf(t, a.srv.URL) {
			t.Fatalf("a refused claim moved the process to %s", got)
		}
	})

	t.Run("the first controller refuses", func(t *testing.T) {
		t.Parallel()

		a, b, c := twoControllers(t)

		a.mu.Lock()
		a.unavailable = func(path string, _ int) bool { return path == custodyRoute }
		a.mu.Unlock()

		_, err := c.ClaimCustody(t.Context(), []string{"l1"})
		if err == nil || nodeclient.IsNoAnswerForTest(err) || !strings.Contains(err.Error(), "503") {
			t.Fatalf("ClaimCustody = %v, want A's refusal", err)
		}

		if n := b.sent(custodyRoute); n != 0 {
			t.Fatalf("a claim A answered went on to B %d times", n)
		}
	})

	t.Run("nobody answers", func(t *testing.T) {
		t.Parallel()

		a, b, c := twoControllers(t)

		goAway(a)
		goAway(b)

		if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); !nodeclient.IsNoAnswerForTest(err) {
			t.Fatalf("ClaimCustody = %v, want no answer", err)
		}

		if got := c.Endpoint().String(); got != endpointOf(t, a.srv.URL) {
			t.Fatalf("an unanswered claim moved the process to %s", got)
		}
	})
}

// A CLAIM STARTS FROM THE CONTROLLER IT LAST REACHED, and goes round from there: asking
// the first one named each time would hold every claim behind a controller that has gone.
func TestACustodyClaimStartsFromTheControllerItLastReached(t *testing.T) {
	t.Parallel()

	a, b, c := twoControllers(t)

	silent := func(path string, _ int) bool { return path == custodyRoute }

	a.mu.Lock()
	a.drop = silent
	a.mu.Unlock()

	if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
		t.Fatalf("the claim that passes over A: %v", err)
	}

	asked := a.sent(custodyRoute)

	a.mu.Lock()
	a.drop = nil
	a.mu.Unlock()

	if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
		t.Fatalf("the next claim: %v", err)
	}

	if n := a.sent(custodyRoute); n != asked {
		t.Fatalf("a claim from B asked A %d more times", n-asked)
	}

	if n := b.sent(custodyRoute); n != 2 {
		t.Fatalf("B was asked %d times, want both", n)
	}

	// AND ROUND AGAIN, past the last one named.
	b.mu.Lock()
	b.drop = silent
	b.mu.Unlock()

	if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
		t.Fatalf("the claim that passes over B: %v", err)
	}

	if got := c.Endpoint().String(); got != endpointOf(t, a.srv.URL) {
		t.Fatalf("the process is on %s, want A", got)
	}
}

// A CLAIM IS SENT WHATEVER WAS NEGOTIATED. That was with another controller: one that
// was older when this process registered may have been upgraded since, and this process
// cannot register to learn so. An older plane's bare 404 is a failed claim and no more.
func TestACustodyClaimIsSentWhateverWasNegotiated(t *testing.T) {
	t.Parallel()

	for _, wire := range []int{nodeapi.MinVersion, nodeapi.VersionCustodyClaim, 0} {
		a := newController(t)

		c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		nodeclient.SetWireForTest(c, wire)

		if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
			t.Fatalf("wire %d: ClaimCustody = %v", wire, err)
		}

		if n := a.sent(custodyRoute); n != 1 {
			t.Fatalf("wire %d: the claim was sent %d times", wire, n)
		}

		if got := c.WireVersion(); got != nodeapi.Version {
			t.Fatalf("wire %d: the answer left the wire at %d", wire, got)
		}
	}
}

// AN OLDER PLANE ANSWERS A BARE 404, with no body the client can read: that is a claim
// that failed, not silence and not "unregistered", so the process stays on this
// controller and keeps the wire it has.
func TestAnOlderPlanesAnswerToAClaimChangesNothing(t *testing.T) {
	t.Parallel()

	a, b, c := twoControllers(t)

	a.mu.Lock()
	a.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != custodyRoute {
			return false
		}

		http.NotFound(w, r)

		return true
	}
	a.mu.Unlock()

	wire := c.WireVersion()

	_, err := c.ClaimCustody(t.Context(), []string{"l1"})
	if err == nil || nodeclient.IsNoAnswerForTest(err) || errors.Is(err, nodeclient.ErrUnregistered) {
		t.Fatalf("ClaimCustody = %v, want a refusal that is neither silence nor unregistered", err)
	}

	if got := c.WireVersion(); got != wire {
		t.Fatalf("the wire moved from %d to %d", wire, got)
	}

	if n := b.sent(custodyRoute); n != 0 {
		t.Fatalf("an answer from A went on to B %d times", n)
	}
}

// THE WIRE MOVES WITH THE CONTROLLER. The process reached this one without registering,
// so the claim's answer is the only thing that can say what it speaks: what it sends a
// controller older than the last is held to that controller's version, and an
// "unregistered" answer's clearing of the wire does not stay for the rest of the wait.
func TestACustodyClaimSettlesTheWireWithTheControllerThatAnswered(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		before, plane int
		want          int
	}{
		{"an older controller", nodeapi.Version, nodeapi.VersionCustodyClaim, nodeapi.VersionCustodyClaim},
		{"a newer controller", nodeapi.VersionCustodyClaim, nodeapi.Version + 5, nodeapi.Version},
		{"a wire an answer cleared", 0, nodeapi.Version, nodeapi.Version},
		{"an answer that does not say", nodeapi.VersionCustodyClaim, 0, nodeapi.VersionCustodyClaim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newController(t)

			a.mu.Lock()
			a.respond = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != custodyRoute {
					return false
				}

				w.Header().Set("Content-Type", "application/json")

				_ = json.NewEncoder(w).Encode(nodeapi.ClaimCustodyResponse{Version: tc.plane}) //nolint:errcheck // the test's own server

				return true
			}
			a.mu.Unlock()

			c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			nodeclient.SetWireForTest(c, tc.before)

			if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err != nil {
				t.Fatalf("ClaimCustody: %v", err)
			}

			if got := c.WireVersion(); got != tc.want {
				t.Fatalf("the wire is %d, want %d", got, tc.want)
			}
		})
	}
}

// followingCompute holds leases a superseded process can claim.
type followingCompute struct {
	*askingCompute

	leases []string
}

func (f *followingCompute) CustodyLeases() []string { return slices.Clone(f.leases) }

func newFollowingCompute(c *nodeclient.Client) *followingCompute {
	return &followingCompute{
		askingCompute: &askingCompute{fakeCompute: &fakeCompute{holding: true}, c: c},
		leases:        []string{"l1"},
	}
}

// claimsHeard answers every claim and keeps what each one named.
type claimsHeard struct {
	mu    sync.Mutex
	named [][]string
}

func (h *claimsHeard) respond(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != custodyRoute {
		return false
	}

	var req nodeapi.ClaimCustodyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return true
	}

	h.mu.Lock()
	h.named = append(h.named, req.LeaseIDs)
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(nodeapi.ClaimCustodyResponse{Version: nodeapi.Version, Held: req.LeaseIDs}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	return true
}

func (h *claimsHeard) first() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.named) == 0 {
		return nil
	}

	return h.named[0]
}

// EVERY WAY A PROCESS LEARNS IT IS SUPERSEDED FOLLOWS ITS CUSTODY. A recovery or a poll in
// the ordinary loop, a recovery before a stop's drain, a poll during the drain and a
// registration during the drain each leave it waiting for what it holds, and that wait
// has to say so to whichever controller serves: one path missed is a process a new
// controller refuses its own leases.
//
// SERIAL, because the controllers' seam and the loop's hooks are the same as those of
// the drain tests beside it, and each case waits on a real loop.
func TestEveryWayOfBeingSupersededFollowsCustody(t *testing.T) {
	const (
		launched = "/v1/nodes/n1/launched"
		register = "/v1/register"
	)

	for _, tc := range []struct {
		name string
		// when is the moment it hears it: the path answers superseded once armed.
		when string
		// armed runs once the process has registered, and arms it.
		armed func(t *testing.T, a *controller, cancel func(), arm func())
		// stopping is whether Run answers the supersession rather than a stop.
		superseded bool
	}{
		{
			name: "a poll in the ordinary loop", when: pollRoute, superseded: true,
			armed: func(_ *testing.T, _ *controller, _ func(), arm func()) { arm() },
		},
		{
			name: "a recovery before the drain", when: launched,
			armed: func(t *testing.T, a *controller, cancel func(), arm func()) {
				t.Helper()

				// THE ORDINARY LOOP'S RECOVERY FAILS UNTIL THE STOP, whose own recovery is
				// the one superseded, which the controller says before it says anything else.
				waitFor(t, func() bool { return a.sent(launched) >= 2 })
				cancel()
				arm()
			},
		},
		{
			name: "a poll during the drain", when: pollRoute,
			armed: func(t *testing.T, a *controller, cancel func(), arm func()) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(pollRoute) >= 1 })
				cancel()
				arm()
			},
		},
		{
			name: "a registration during the drain", when: register,
			armed: func(t *testing.T, a *controller, cancel func(), arm func()) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(pollRoute) >= 1 })
				cancel()
				arm()
				a.plane.ForgetForTest("n1")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newController(t)

			var (
				armed atomic.Bool
				heard claimsHeard
			)

			a.mu.Lock()
			a.respond = heard.respond
			a.supersede = func(path string, _ int) bool { return path == tc.when && armed.Load() }
			a.mu.Unlock()

			c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			compute := newFollowingCompute(c)

			if tc.when == launched {
				a.mu.Lock()
				a.unavailable = func(path string, _ int) bool { return path == launched }
				a.mu.Unlock()
			}

			cancel, done, _ := runDraining(t, c, compute)

			waitFor(t, func() bool { return a.sent(register) >= 1 })
			tc.armed(t, a, cancel, func() { armed.Store(true) })

			waitFor(t, func() bool { return slices.Equal(heard.first(), []string{"l1"}) })

			if n := compute.supersededCount(); n != 1 {
				t.Fatalf("what runs here was handed to custody %d times, want once", n)
			}

			compute.setHolding(false)

			select {
			case err := <-done:
				if tc.superseded && !errors.Is(err, nodeclient.ErrSuperseded) {
					t.Fatalf("the process ended with %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the process did not stop once it held nothing")
			}
		})
	}
}

// A RECOVERY THAT HEARS IT WAS SUPERSEDED DOES NOT GO ROUND AGAIN. Another process
// registered while this one was recovering, and treating that as an outage registers
// again, which takes the name back from the process that holds it, and that process
// then does the same. What it recovered before hearing is still answered, so Ready is
// called for the wait, as a stop's drain calls it.
func TestARecoverySupersededInTheOrdinaryLoopNeverRegistersAgain(t *testing.T) {
	a := newController(t)

	const launched = "/v1/nodes/n1/launched"

	var heard claimsHeard

	a.mu.Lock()
	a.respond = heard.respond
	a.supersede = func(path string, _ int) bool { return path == launched }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := newFollowingCompute(c)

	var ready atomic.Bool

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	var running sync.WaitGroup

	running.Go(func() {
		done <- nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond, SweepEvery: 10 * time.Millisecond,
			DrainTimeout: time.Hour, Ready: func() { ready.Store(true) },
		})
	})
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})

	waitFor(t, func() bool { return len(heard.first()) > 0 })

	if !ready.Load() {
		t.Fatal("the wait for what it recovered began with its guests unanswered")
	}

	if n := a.sent("/v1/register"); n != 1 {
		t.Fatalf("the process registered %d times, want once", n)
	}

	if n := a.sent(launched); n != 1 {
		t.Fatalf("the process asked %d times after it heard, want once", n)
	}

	compute.setHolding(false)

	select {
	case err := <-done:
		if !errors.Is(err, nodeclient.ErrSuperseded) {
			t.Fatalf("the process ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not stop once it held nothing")
	}
}

// A PROCESS NOBODY SUPERSEDED CLAIMS NOTHING. Its drain's completions still come to
// it, and what it holds is the plane's record already; a claim from it would be noise
// on every stop.
func TestADrainNobodySupersededClaimsNothing(t *testing.T) {
	a := newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := newFollowingCompute(c)

	cancel, done, endings := runDraining(t, c, compute)

	waitFor(t, func() bool { return a.sent(pollRoute) >= 1 })
	cancel()

	// A FEW OF ITS OWN POLLS INTO THE DRAIN, which a claim on the drain's cadence would
	// have come well within.
	polled := a.sent(pollRoute)
	waitFor(t, func() bool { return a.sent(pollRoute) >= polled+3 })

	if n := a.sent(custodyRoute); n != 0 {
		t.Fatalf("a process nobody superseded claimed %d times", n)
	}

	compute.setHolding(false)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the drain did not end once nothing was held")
	}

	if got := endings(); slices.Contains(got, nodeending.Superseded) {
		t.Fatalf("the stop recorded %v", got)
	}
}

// A CLAIM THAT FAILS DOES NOT END THE WAIT, and one that worked is asked again: the
// controller that answers today is not the one that may answer tomorrow.
func TestASupersededProcessGoesOnClaimingUntilItHoldsNothing(t *testing.T) {
	a := newController(t)

	var failing atomic.Bool

	failing.Store(true)

	a.mu.Lock()
	a.supersede = func(path string, _ int) bool { return path == pollRoute }
	a.unavailable = func(path string, _ int) bool { return path == custodyRoute && failing.Load() }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := newFollowingCompute(c)

	_, done, _ := runDraining(t, c, compute)

	waitFor(t, func() bool { return a.sent(custodyRoute) >= 3 })

	failing.Store(false)

	sent := a.sent(custodyRoute)
	waitFor(t, func() bool { return a.sent(custodyRoute) >= sent+3 })

	select {
	case err := <-done:
		t.Fatalf("the process stopped with %v while it still held compute", err)
	default:
	}

	compute.setHolding(false)

	select {
	case err := <-done:
		if !errors.Is(err, nodeclient.ErrSuperseded) {
			t.Fatalf("the process ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not stop once it held nothing")
	}
}

// pausedFollower holds a process inside asking what it holds.
type pausedFollower struct {
	*followingCompute

	entered, gate chan struct{}
	once          sync.Once
}

func (g *pausedFollower) CustodyLeases() []string {
	g.once.Do(func() { close(g.entered) })
	<-g.gate

	return g.followingCompute.CustodyLeases()
}

// THE FOLLOWING IS JOINED, from a superseded loop and from a stop's drain: Run does not
// return with a claim still to be sent, which would be a process that has stopped still
// speaking for what it held.
func TestRunJoinsTheFollowingOfCustody(t *testing.T) {
	const launched = "/v1/nodes/n1/launched"

	for _, tc := range []struct {
		name string
		// stage leads to the supersession, with the running stop's cancel.
		before func(a *controller, stopped *atomic.Bool)
		stage  func(t *testing.T, a *controller, cancel func(), stopped *atomic.Bool)
	}{
		{
			name: "a superseded loop",
			before: func(a *controller, _ *atomic.Bool) {
				a.supersede = func(path string, _ int) bool { return path == pollRoute }
			},
			stage: func(*testing.T, *controller, func(), *atomic.Bool) {},
		},
		{
			name: "a stop's drain",
			before: func(a *controller, stopped *atomic.Bool) {
				a.unavailable = func(path string, _ int) bool { return path == launched }
				a.supersede = func(path string, _ int) bool { return path == launched && stopped.Load() }
			},
			stage: func(t *testing.T, a *controller, cancel func(), stopped *atomic.Bool) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(launched) >= 2 })
				cancel()
				stopped.Store(true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newController(t)

			var stopped atomic.Bool

			a.mu.Lock()
			tc.before(a, &stopped)
			a.mu.Unlock()

			c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			compute := &pausedFollower{
				followingCompute: newFollowingCompute(c),
				entered:          make(chan struct{}),
				gate:             make(chan struct{}),
			}

			var release sync.Once

			t.Cleanup(func() { release.Do(func() { close(compute.gate) }) })

			cancel, done, _ := runDraining(t, c, compute)

			tc.stage(t, a, cancel, &stopped)

			select {
			case <-compute.entered:
			case <-time.After(15 * time.Second):
				t.Fatal("the process never asked what it holds")
			}

			compute.setHolding(false)

			// THE WAIT ENDS WITHIN ITS SWEEP, 10 ms here, and only the join can hold Run
			// past it.
			select {
			case err := <-done:
				t.Fatalf("Run returned with %v while the following was still running", err)
			case <-time.After(500 * time.Millisecond):
			}

			release.Do(func() { close(compute.gate) })

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("Run did not return once the following could end")
			}
		})
	}
}

// countingFollower holds nothing it can name.
type countingFollower struct {
	*followingCompute

	asked atomic.Int32
}

func (f *countingFollower) CustodyLeases() []string {
	f.asked.Add(1)

	return nil
}

// A PROCESS NAMING NOTHING SENDS NOTHING: its claim would be a ledger's reads of nothing,
// and an answer that could move it off the controller its lease calls are on.
func TestAProcessNamingNothingClaimsNothing(t *testing.T) {
	a := newController(t)

	a.mu.Lock()
	a.supersede = func(path string, _ int) bool { return path == pollRoute }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := &countingFollower{followingCompute: newFollowingCompute(c)}

	_, done, _ := runDraining(t, c, compute)

	waitFor(t, func() bool { return compute.asked.Load() >= 3 })

	if n := a.sent(custodyRoute); n != 0 {
		t.Fatalf("a process naming nothing claimed %d times", n)
	}

	compute.setHolding(false)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not stop once it held nothing")
	}
}
