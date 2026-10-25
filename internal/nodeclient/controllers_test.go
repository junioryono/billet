package nodeclient_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
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
		drop := c.drop
		c.mu.Unlock()

		if drop != nil && drop(r.URL.Path, n) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close() //nolint:errcheck // the point is that nothing answers
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

// deadAddress is an address nothing listens on: bound, then released.
func deadAddress(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	return addr
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

	waitFor(t, func() bool { return len(b.plane.Nodes()) == 1 })

	if got, want := c.Endpoint().String(), endpointOf(t, b.srv.URL); got != want {
		t.Fatalf("the client dials %s, want %s", got, want)
	}
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

// THE NODE'S CONNECTIONS NOTICE A SILENT CONTROLLER IN SECONDS. A controller
// cut off the network sends nothing, not even a reset, and a long poll to it
// waited out the poll window and its slack: the probes are what end it, in
// about Idle + Interval x Count, and a dial that is not answered gives up
// while another controller may be serving. The rehearsal measures the effect
// on a real partition (docs/reference/records/host-rehearsals.md).
func TestTheNodesDialerNoticesASilentController(t *testing.T) {
	t.Parallel()

	d := nodeclient.DialerForTest()

	if !d.KeepAliveConfig.Enable {
		t.Fatal("the node's connections are not probed, so a silent controller holds a poll for its whole window")
	}

	silent := d.KeepAliveConfig.Idle + d.KeepAliveConfig.Interval*time.Duration(d.KeepAliveConfig.Count)
	if silent <= 0 || silent > 15*time.Second {
		t.Fatalf("a silent controller is noticed after %v, want within 15s", silent)
	}

	if d.Timeout <= 0 || d.Timeout > 5*time.Second {
		t.Fatalf("a dial waits %v for an address that does not answer, want a bound within 5s", d.Timeout)
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

		return <-g.release, nil
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

	go func() {
		_, err := c.LaunchedLeaseIDs(t.Context(), "n1")
		late <- err
	}()

	<-toA.held

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
