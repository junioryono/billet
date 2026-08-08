// Package e2e drives the whole of billet against a fake Actions service and a
// real container backend.
//
// Every other suite tests one seam. This one tests the relationships between
// them, which is where the defects that survived unit tests have all lived: a
// message arrives on the wire, capacity is escrowed, a lease is bound, a
// registration is minted, a container starts, the job completes, the container
// is destroyed and the capacity comes back. Each of those steps is covered
// elsewhere; that they compose is only covered here.
//
// The GitHub side is fake and the docker side is REAL. That split is deliberate:
// billet's relationship with GitHub is a protocol it can be lied to about
// safely, while its relationship with a container runtime is one where the
// interesting failures come from the runtime actually behaving like itself.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider/docker"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// plane is a scripted Actions service: it holds a queue of messages a test can
// push to, and records what billet acquired and acknowledged.
//
// Scripted rather than simulated. It does not model GitHub's assignment
// behaviour — it serves exactly what a test queues, so an assertion is about
// billet's response to a stated situation rather than about a guess at GitHub's.
type plane struct {
	*fakeactions.Server

	t *testing.T

	mu        sync.Mutex
	queued    []map[string]any
	nextMsgID int
	acquired  []int64
	deleted   []int64
	setID     int

	// exists models whether the scale set has been created yet. Without it the
	// service reported the set as already present on the very first GET, so
	// billet correctly ADOPTED it and never issued a create — and a test asserting
	// on the create body was asserting on nothing.
	exists bool
}

const (
	testTier  = "billet-2vcpu-ubuntu-2404"
	testGroup = "billet"

	// busybox, not the runner image. The assertion is that billet starts and
	// stops the right container, and a 2GB pull would turn this into a network
	// test that fails for reasons the code cannot cause.
	testImage = "busybox:latest"
)

func newPlane(t *testing.T) *plane {
	t.Helper()

	p := &plane{t: t, nextMsgID: 1, setID: 7}
	p.Server = fakeactions.New(t, p.route)

	return p
}

// route answers the scale-set API. The auth handshake is answered upstream by
// the shared fake, so everything here is protocol proper.
func (p *plane) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case strings.Contains(path, "runnergroups"):
		fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(
			map[string]any{"id": 1, "name": testGroup}))

	case strings.Contains(path, "acquirejobs"):
		p.acquireJobs(w, r)

	case strings.Contains(path, "generatejitconfig"):
		p.generateJIT(w, r)

	case strings.HasSuffix(path, "/sessions"):
		fakeactions.WriteJSON(p.t, w, fakeactions.SessionJSON(
			"3f8a1c22-0000-4000-8000-000000000001", "billet-test",
			p.scaleSet(), p.URL+"/queue", "queue-token"))

	// Session teardown. The path is /sessions/{uuid}, so it must be matched
	// before the collection above would swallow it — hence HasSuffix there.
	case strings.Contains(path, "/sessions/"):
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.Contains(path, "runnerscalesets"):
		p.mu.Lock()
		exists := p.exists
		p.mu.Unlock()

		if !exists {
			fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON())

			return
		}

		fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(p.scaleSet()))

	case r.Method == http.MethodPost && strings.Contains(path, "runnerscalesets"):
		p.mu.Lock()
		p.exists = true
		p.mu.Unlock()

		fakeactions.WriteJSON(p.t, w, p.scaleSet())

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/queue/"):
		p.deleteMessage(w, path)

	case strings.HasPrefix(path, "/queue"):
		p.getMessage(w)

	default:
		p.t.Errorf("unexpected call %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// scaleSet is what the service reports for this tier.
//
// Exactly the labels billet asks for, and no more. An extra one here is not a
// harmless embellishment: reconciliation refuses to adopt a set whose labels it
// did not set, so a fake that adds "self-hosted" makes billet correctly reject
// the very set the fake just handed it — a failure that reads like billet's bug
// and is entirely the fake's.
func (p *plane) scaleSet() map[string]any {
	return fakeactions.ScaleSetJSON(p.setID, testTier, testGroup, testTier)
}

// getMessage serves the head of the queue, or 202 for "nothing right now".
//
// 202 IS THE ORDINARY ANSWER, not an error: the real service holds the
// connection open for the poll interval and then says nothing happened. A fake
// that returned 200-with-empty instead would let billet mishandle the common
// case undetected.
func (p *plane) getMessage(w http.ResponseWriter) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queued) == 0 {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	msg := p.queued[0]
	p.queued = p.queued[1:]

	fakeactions.WriteJSON(p.t, w, msg)
}

func (p *plane) deleteMessage(w http.ResponseWriter, path string) {
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/queue/"), 10, 64)
	if err != nil {
		p.t.Errorf("acknowledged a message with an unreadable id: %s", path)
	}

	p.mu.Lock()
	p.deleted = append(p.deleted, id)
	p.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// acquireJobs records what billet bid for and grants all of it.
func (p *plane) acquireJobs(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("read acquire body: %v", err)
	}

	var ids []int64

	if err := json.Unmarshal(body, &ids); err != nil {
		p.t.Errorf("acquire body is not a list of ids: %s", body)
	}

	p.mu.Lock()
	p.acquired = append(p.acquired, ids...)
	p.mu.Unlock()

	fakeactions.WriteJSON(p.t, w, map[string]any{"count": len(ids), "value": ids})
}

func (p *plane) generateJIT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		WorkFolder string `json:"workFolder"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.t.Errorf("decode jit request: %v", err)
	}

	fakeactions.WriteJSON(p.t, w, fakeactions.JitConfigJSON(99, req.Name, "encoded-jit-config"))
}

// queue pushes one message envelope containing the given job messages.
func (p *plane) queue(stats map[string]any, jobs ...map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.queued = append(p.queued, fakeactions.MessageJSON(p.t, p.nextMsgID, stats, jobs...))
	p.nextMsgID++
}

func (p *plane) acquiredIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]int64(nil), p.acquired...)
}

// ---------------------------------------------------------------- the stack --

// stack is billet, assembled the way cmd/billet assembles it.
type stack struct {
	plane    *plane
	alloc    *alloc.Allocator
	runner   *node.Runner
	server   *server.Server
	provider *docker.Provider
	node     string
}

func newStack(t *testing.T) *stack {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	p := newPlane(t)

	client, err := scaleset.New(scaleset.Config{
		ConfigURL:      p.URL + "/acme",
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     p.PrivateKeyPEM(),
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	dir := t.TempDir()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tiers := []config.Tier{{
		Label:       testTier,
		Provider:    config.ProviderDocker,
		VCPU:        2,
		Memory:      2 * config.GiB,
		Disk:        10 * config.GiB,
		Image:       testImage,
		RunnerGroup: testGroup,
		GuestOS:     config.GuestLinux,
	}}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "e2e-host"

	if err := a.RegisterNode(t.Context(), host, config.ProviderDocker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// A per-test deployment identity, so two of these running concurrently do not
	// enumerate each other's containers. This is the property state.DeploymentID
	// exists for, exercised here rather than asserted about.
	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	prov := docker.New(deployment)

	// Whatever this test leaves behind goes with it, even on a failure path.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())

		instances, err := prov.List(ctx)
		if err != nil {
			return
		}

		for _, inst := range instances {
			//nolint:errcheck // best-effort sweep; the test is already over
			_ = prov.Destroy(ctx, inst.ID)
		}
	})

	runner := node.New(a, host, jitSource{client}, prov, tiers, testLogger(t))

	srv := server.New(a, provisioner{client}, tiers, "billet-test", testLogger(t),
		server.WithNodeRunner(runner),
		// Fast, because the sweep rides this tick and a test that waits a minute
		// for it is a test nobody runs.
		server.WithReapInterval(200*time.Millisecond))

	return &stack{plane: p, alloc: a, runner: runner, server: srv, provider: prov, node: host}
}

// run starts the control plane and returns a stop function.
//
// A Run that fails is reported IMMEDIATELY rather than at stop. The first
// version of this harness only read the error when the test tore down, so a
// control plane that died during startup showed up as whatever assertion timed
// out first — thirty seconds later, naming the wrong thing. Debugging that
// misdirection cost more than the harness.
func (s *stack) run(t *testing.T) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var runErr error

	// CLOSED, not sent on. A channel that carries the error can only be read
	// once, so the watchdog below and the stop function would compete for it —
	// which is how the first version reported "the control plane did not stop"
	// on top of a failure it had already diagnosed correctly.
	finished := make(chan struct{})

	go func() {
		runErr = s.server.Run(ctx)

		close(finished)
	}()

	stopped := make(chan struct{})

	go func() {
		select {
		case <-finished:
			select {
			case <-stopped:
			default:
				// A Run that returns before it is asked to is a failure whatever
				// the error says, including nil.
				t.Errorf("the control plane stopped on its own: %v", runErr)
			}
		case <-stopped:
		}
	}()

	return func() {
		close(stopped)
		cancel()

		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("the control plane did not stop")
		}
	}
}

// awaitContainer waits for a container named after some lease to exist.
func (s *stack) awaitContainer(t *testing.T, want int) []string {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for {
		instances, err := s.provider.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if len(instances) == want {
			names := make([]string, 0, len(instances))
			for _, inst := range instances {
				names = append(names, inst.Name)
			}

			return names
		}

		if time.Now().After(deadline) {
			t.Fatalf("waited for %d container(s), have %d", want, len(instances))
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// ------------------------------------------------------------- the adapters --

// provisioner and jitSource mirror cmd/billet's adapters. They exist because
// internal/scaleset returns its own types, and pointing the dependency the other
// way would make the package whose job is to keep a preview API at arm's length
// import the control plane.
type provisioner struct{ c *scaleset.Client }

func (p provisioner) EnsureScaleSet(
	ctx context.Context, name, group string, labels []string,
) (*server.ScaleSet, error) {
	set, err := p.c.EnsureScaleSet(ctx, name, group, labels)
	if err != nil {
		return nil, err
	}

	return &server.ScaleSet{ID: set.ID, Group: set.Group}, nil
}

func (p provisioner) Session(ctx context.Context, id int, owner string) (server.Session, error) {
	return p.c.Session(ctx, id, owner)
}

type jitSource struct{ c *scaleset.Client }

func (j jitSource) Describe(ctx context.Context, tier, group string) (*node.Set, []string, error) {
	set, labels, err := j.c.Describe(ctx, tier, group)
	if err != nil {
		return nil, nil, err
	}

	return &node.Set{ID: set.ID, Name: set.Name}, labels, nil
}

func (j jitSource) JITConfig(
	ctx context.Context, setID int, name, work string,
) (node.Registration, error) {
	return j.c.JITConfig(ctx, setID, name, work)
}

// testLogger writes the control plane's diagnostics into the test log, where
// they appear only for a failing test. A discarded logger made the first
// failure here unreadable.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)

	return len(p), nil
}
