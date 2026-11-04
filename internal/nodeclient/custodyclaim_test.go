package nodeclient_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeending"
)

const custodyRoute = "/v1/nodes/n1/custody"

func registerClient(t *testing.T, c *nodeclient.Client) {
	t.Helper()

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// A SUPERSEDED PROCESS FOLLOWS THE DEPLOYMENT WITHOUT TAKING THE NAME. It passes over
// the controller that went to the one that answers, and its lease calls go there from
// then on, and it never registers: a registration would take the name back from the
// process that holds it.
func TestACustodyClaimFollowsTheControllerThatAnswersWithoutRegistering(t *testing.T) {
	t.Parallel()

	a, b := newController(t), newController(t)

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
		{Base: a.srv.URL}, {Base: b.srv.URL},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	registerClient(t, c)

	wire := c.WireVersion()

	a.srv.CloseClientConnections()
	a.srv.Close()

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
		t.Fatalf("the claim moved the wire from %d to %d", wire, got)
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

		a, b := newController(t), newController(t)

		c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
			{Base: a.srv.URL}, {Base: b.srv.URL},
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		registerClient(t, c)

		b.mu.Lock()
		b.unavailable = func(path string, _ int) bool { return path == custodyRoute }
		b.mu.Unlock()

		a.srv.CloseClientConnections()
		a.srv.Close()

		if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err == nil {
			t.Fatal("a controller's refusal was reported as success")
		}

		if got := c.Endpoint().String(); got != endpointOf(t, a.srv.URL) {
			t.Fatalf("a refused claim moved the process to %s", got)
		}
	})

	t.Run("the first controller refuses", func(t *testing.T) {
		t.Parallel()

		a, b := newController(t), newController(t)

		c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
			{Base: a.srv.URL}, {Base: b.srv.URL},
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		registerClient(t, c)

		a.mu.Lock()
		a.unavailable = func(path string, _ int) bool { return path == custodyRoute }
		a.mu.Unlock()

		if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err == nil {
			t.Fatal("a controller's refusal was reported as success")
		}

		if n := b.sent(custodyRoute); n != 0 {
			t.Fatalf("a claim A answered went on to B %d times", n)
		}
	})

	t.Run("nobody answers", func(t *testing.T) {
		t.Parallel()

		a, b := newController(t), newController(t)

		c, err := nodeclient.New(nodeclient.Options{Node: "n1", Targets: []nodeclient.Target{
			{Base: a.srv.URL}, {Base: b.srv.URL},
		}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		registerClient(t, c)

		for _, gone := range []*controller{a, b} {
			gone.srv.CloseClientConnections()
			gone.srv.Close()
		}

		if _, err := c.ClaimCustody(t.Context(), []string{"l1"}); err == nil {
			t.Fatal("silence from every controller was reported as success")
		}

		if got := c.Endpoint().String(); got != endpointOf(t, a.srv.URL) {
			t.Fatalf("an unanswered claim moved the process to %s", got)
		}
	})
}

// BELOW ITS VERSION A CLAIM IS NOT SENT, since the older plane answers a bare 404; a
// wire of zero is a process whose registration was forgotten, which says nothing about
// what the controller speaks, so it asks.
func TestACustodyClaimIsGatedOnTheNegotiatedVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wire int
		want int
	}{
		{"the version before", nodeapi.VersionCustodyClaim - 1, 0},
		{"the version itself", nodeapi.VersionCustodyClaim, 1},
		{"no registration remembered", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newController(t)

			c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			nodeclient.SetWireForTest(c, tc.wire)

			adopted, err := c.ClaimCustody(t.Context(), []string{"l1"})
			if err != nil || len(adopted) != 0 {
				t.Fatalf("ClaimCustody = %v, %v", adopted, err)
			}

			if n := a.sent(custodyRoute); n != tc.want {
				t.Fatalf("the claim was sent %d times, want %d", n, tc.want)
			}
		})
	}
}

// followingCompute holds leases a superseded process can claim, and stops holding when
// it is told to let go of them.
type followingCompute struct {
	*askingCompute

	mu     sync.Mutex
	leases []string
	letGo  []string
}

func (f *followingCompute) CustodyLeases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.leases)
}

func (f *followingCompute) LetGo(ids []string) {
	f.mu.Lock()
	f.letGo = append(f.letGo, ids...)
	f.leases = slices.DeleteFunc(f.leases, func(id string) bool { return slices.Contains(ids, id) })
	empty := len(f.leases) == 0
	f.mu.Unlock()

	if empty {
		f.setHolding(false)
	}
}

func (f *followingCompute) letGoOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.letGo)
}

// answerAdopted makes a controller answer every claim that the node's current process
// has adopted what it names, and counts nothing else differently.
func answerAdopted(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != custodyRoute {
		return false
	}

	var req nodeapi.ClaimCustodyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return true
	}

	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(nodeapi.ClaimCustodyResponse{Adopted: req.LeaseIDs}) //nolint:errcheck // the test's own server

	return true
}

// EVERY WAY A PROCESS LEARNS IT IS SUPERSEDED FOLLOWS ITS CUSTODY. A poll in the
// ordinary loop, a recovery before a stop's drain, a poll during the drain and a
// registration during the drain each leave it waiting for what it holds, and that
// wait has to say so to whichever controller serves: one path missed is a process a
// new controller refuses its own leases.
//
// SERIAL, because the controllers' seam and the loop's hooks are the same as those of
// the drain tests beside it, and each case waits on a real loop.
func TestEveryWayOfBeingSupersededFollowsCustody(t *testing.T) {
	const (
		launched = "/v1/nodes/n1/launched"
		poll     = "/v1/nodes/n1/poll"
		register = "/v1/register"
	)

	for _, tc := range []struct {
		name string
		// stage runs against the controller and the running stop, once it has
		// registered, and leads to supersession.
		before func(a *controller)
		stage  func(t *testing.T, a *controller, cancel func(), when *atomic.Bool)
	}{
		{
			name: "a poll in the ordinary loop",
			stage: func(_ *testing.T, _ *controller, _ func(), when *atomic.Bool) {
				when.Store(true)
			},
			before: func(a *controller) {},
		},
		{
			name: "a recovery before the drain",
			stage: func(t *testing.T, a *controller, cancel func(), _ *atomic.Bool) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(launched) >= 2 })
				cancel()
			},
			before: func(a *controller) {
				a.supersede = func(path string, _ int) bool { return path == launched }
			},
		},
		{
			name: "a poll during the drain",
			stage: func(t *testing.T, a *controller, cancel func(), when *atomic.Bool) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(poll) >= 1 })
				cancel()
				when.Store(true)
			},
			before: func(a *controller) {},
		},
		{
			name: "a registration during the drain",
			stage: func(t *testing.T, a *controller, cancel func(), when *atomic.Bool) {
				t.Helper()
				waitFor(t, func() bool { return a.sent(poll) >= 1 })
				cancel()
				a.plane.ForgetForTest("n1")
				a.mu.Lock()
				a.supersede = func(path string, _ int) bool { return path == register }
				a.mu.Unlock()
			},
			before: func(a *controller) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newController(t)

			var when atomic.Bool

			a.mu.Lock()
			a.respond = answerAdopted
			a.supersede = func(path string, _ int) bool { return path == poll && when.Load() }
			tc.before(a)
			a.mu.Unlock()

			c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			compute := &followingCompute{
				askingCompute: &askingCompute{fakeCompute: &fakeCompute{holding: true}, c: c},
				leases:        []string{"l1"},
			}

			cancel, done, _ := runDraining(t, c, compute)

			waitFor(t, func() bool { return a.sent(register) >= 1 })
			tc.stage(t, a, cancel, &when)

			waitFor(t, func() bool { return len(compute.letGoOf()) > 0 })

			if got := compute.letGoOf(); !slices.Equal(got, []string{"l1"}) {
				t.Fatalf("the process let go of %v", got)
			}

			if n := compute.supersededCount(); n != 1 {
				t.Fatalf("what runs here was handed to custody %d times, want once", n)
			}

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the process did not stop once it held nothing")
			}
		})
	}
}

// A PROCESS NOBODY SUPERSEDED CLAIMS NOTHING. Its drain's completions still come to
// it, and what it holds is the plane's record already; a claim from it would be noise
// on every stop.
func TestADrainNobodySupersededClaimsNothing(t *testing.T) {
	a := newController(t)

	a.mu.Lock()
	a.respond = answerAdopted
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := &followingCompute{
		askingCompute: &askingCompute{fakeCompute: &fakeCompute{holding: true}, c: c},
		leases:        []string{"l1"},
	}

	cancel, done, endings := runDraining(t, c, compute)

	const poll = "/v1/nodes/n1/poll"

	waitFor(t, func() bool { return a.sent(poll) >= 1 })
	cancel()

	// A FEW OF ITS OWN POLLS INTO THE DRAIN, which a claim on the drain's cadence would
	// have come well within.
	polled := a.sent(poll)
	waitFor(t, func() bool { return a.sent(poll) >= polled+3 })

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

// A CLAIM THAT HOLDS IS ASKED AGAIN AND LETS GO OF NOTHING, and a claim that fails does
// not end the wait: the controller that does not answer today is the one that may.
func TestASupersededProcessGoesOnClaimingUntilItHoldsNothing(t *testing.T) {
	a := newController(t)

	const poll = "/v1/nodes/n1/poll"

	var failing atomic.Bool

	failing.Store(true)

	a.mu.Lock()
	a.supersede = func(path string, _ int) bool { return path == poll }
	a.unavailable = func(path string, n int) bool { return path == custodyRoute && failing.Load() }
	a.mu.Unlock()

	c, err := nodeclient.New(nodeclient.Options{Node: "n1", Base: a.srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	compute := &followingCompute{
		askingCompute: &askingCompute{fakeCompute: &fakeCompute{holding: true}, c: c},
		leases:        []string{"l1"},
	}

	_, done, _ := runDraining(t, c, compute)

	waitFor(t, func() bool { return a.sent(custodyRoute) >= 3 })

	failing.Store(false)

	sent := a.sent(custodyRoute)
	waitFor(t, func() bool { return a.sent(custodyRoute) >= sent+3 })

	if got := compute.letGoOf(); len(got) != 0 {
		t.Fatalf("a claim nobody answered as adopted let go of %v", got)
	}

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

	// THE FOLLOWING ENDS WITH THE WAIT, and Run joined it.
	after := a.sent(custodyRoute)

	time.Sleep(100 * time.Millisecond)

	if n := a.sent(custodyRoute); n != after {
		t.Fatalf("the process went on claiming after it stopped: %d then %d", after, n)
	}
}
