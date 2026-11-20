package nodeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// heldCompute holds each destroy, and each launch whose request it names, until the
// test lets it go, and records the order things started and finished in.
type heldCompute struct {
	*fakeCompute

	mu       sync.Mutex
	events   []string
	destroys map[int64]chan struct{}
	launches map[int64]chan struct{}
	releases []func()
	// inLine is each command kind the loop ran in line, as it began to wait.
	inLine []string

	// commands are the test's callers of the plane, joined when it ends.
	commands sync.WaitGroup
}

// ranInLine reports whether the loop has begun to run a command of kind in line.
func (h *heldCompute) ranInLine(kind nodeapi.CommandKind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Contains(h.inLine, string(kind))
}

func newHeldCompute() *heldCompute {
	return &heldCompute{
		fakeCompute: &fakeCompute{},
		destroys:    map[int64]chan struct{}{},
		launches:    map[int64]chan struct{}{},
	}
}

// holdDestroy makes the destroy of request id wait until the returned func runs.
func (h *heldCompute) holdDestroy(id int64) func() {
	return h.hold(h.destroys, id)
}

// holdLaunch makes the launch of request id wait until the returned func runs.
func (h *heldCompute) holdLaunch(id int64) func() {
	return h.hold(h.launches, id)
}

func (h *heldCompute) hold(gates map[int64]chan struct{}, id int64) func() {
	h.mu.Lock()
	defer h.mu.Unlock()

	gate := make(chan struct{})
	gates[id] = gate
	release := sync.OnceFunc(func() { close(gate) })
	h.releases = append(h.releases, release)

	return release
}

// releaseAll lets everything held go.
func (h *heldCompute) releaseAll() {
	h.mu.Lock()
	releases := slices.Clone(h.releases)
	h.mu.Unlock()

	for _, release := range releases {
		release()
	}
}

func (h *heldCompute) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.events = append(h.events, event)
}

func (h *heldCompute) seen(event string) bool {
	return slices.Contains(h.eventsNow(), event)
}

func (h *heldCompute) eventsNow() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.events)
}

// without is the events recorded, less every one named in drop.
func (h *heldCompute) without(drop ...string) []string {
	return slices.DeleteFunc(h.eventsNow(), func(e string) bool { return slices.Contains(drop, e) })
}

func (h *heldCompute) Launch(ctx context.Context, lease *alloc.Lease, tier *nodeapi.TierSpec, job dispatch.Job) error {
	h.mu.Lock()
	gate := h.launches[job.RequestID]
	h.mu.Unlock()

	h.record("launch-start " + lease.ID)

	if gate != nil {
		<-gate
	}

	err := h.fakeCompute.Launch(ctx, lease, tier, job)
	h.record("launch-done " + lease.ID)

	return err
}

func (h *heldCompute) Destroy(ctx context.Context, requestID int64) error {
	h.mu.Lock()
	gate := h.destroys[requestID]
	h.mu.Unlock()

	h.record("destroy-start " + strconv.FormatInt(requestID, 10))

	if gate != nil {
		<-gate
	}

	err := h.fakeCompute.Destroy(ctx, requestID)
	h.record("destroy-done " + strconv.FormatInt(requestID, 10))

	return err
}

func (h *heldCompute) Sweep(ctx context.Context) error {
	h.record("sweep")

	return h.fakeCompute.Sweep(ctx)
}

func (h *heldCompute) Tend(ctx context.Context) error {
	h.record("tend")

	return h.fakeCompute.Tend(ctx)
}

// holdCustody makes a lost report's custody take wait until the returned func runs.
func (h *heldCompute) holdCustody() func() {
	return h.hold(h.launches, -1)
}

func (h *heldCompute) AssumeCustody(ctx context.Context, lease *alloc.Lease, requestID int64) error {
	h.mu.Lock()
	gate := h.launches[-1]
	h.mu.Unlock()

	h.record("custody-start " + lease.ID)
	if gate != nil {
		<-gate
	}

	err := h.fakeCompute.AssumeCustody(ctx, lease, requestID)
	h.record("custody-done " + lease.ID)

	return err
}

// pollCounter counts the polls a plane is asked.
type pollCounter struct{ n atomic.Int64 }

func (p *pollCounter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/poll") {
			p.n.Add(1)
		}
		next.ServeHTTP(w, r)
	})
}

// answerVersion has every registration answered as settled on version: a plane that
// speaks no newer, as the node sees it.
func answerVersion(t *testing.T, version int, next http.Handler) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/register") {
			next.ServeHTTP(w, r)

			return
		}

		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		var body map[string]any
		if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) == nil {
			body["version"] = version
			body["max_version"] = version
		}

		maps.Copy(w.Header(), rec.Header())
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		var err error
		if body != nil {
			err = json.NewEncoder(w).Encode(body)
		} else {
			_, err = w.Write(rec.Body.Bytes())
		}
		if err != nil {
			t.Errorf("answer a registration as version %d: %v", version, err)
		}
	})
}

// heldHarness is a plane and a node whose loop runs compute, joined when the test
// ends AFTER everything compute holds is let go and the test's own callers have
// returned: the fake ignores cancellation, so a loop joined with a destroy still
// held would wait for the package timeout.
func heldHarness(
	t *testing.T, concurrency int, sweepEvery time.Duration,
) (*nodeplane.Plane, *heldCompute, *pollCounter, string) {
	t.Helper()

	return heldHarnessAt(t, concurrency, sweepEvery, 0)
}

// heldHarnessAt is heldHarness with the plane answering registrations as settled
// on version, or as it would when version is zero.
func heldHarnessAt(
	t *testing.T, concurrency int, sweepEvery time.Duration, version int,
) (*nodeplane.Plane, *heldCompute, *pollCounter, string) {
	t.Helper()

	return heldHarnessFor(t, concurrency, sweepEvery, version, 30*time.Second)
}

// heldHarnessFor is heldHarnessAt with the plane's command timeout named.
func heldHarnessFor(
	t *testing.T, concurrency int, sweepEvery time.Duration, version int, commandTimeout time.Duration,
) (*nodeplane.Plane, *heldCompute, *pollCounter, string) {
	t.Helper()

	polls := &pollCounter{}
	wrap := polls.wrap
	if version > 0 {
		wrap = func(h http.Handler) http.Handler { return answerVersion(t, version, polls.wrap(h)) }
	}
	p, c := harnessBehind(t, commandTimeout, wrap)
	compute := newHeldCompute()

	ctx, cancel := context.WithCancel(t.Context())

	opts := nodeclient.LoopOptions{
		VCPU:              testNodeVCPU,
		Memory:            testNodeMemory,
		Provider:          config.ProviderDocker,
		GuestOS:           []config.GuestOS{config.GuestLinux},
		Deployment:        deployment,
		Log:               slog.New(slog.DiscardHandler),
		Backoff:           20 * time.Millisecond,
		LaunchConcurrency: concurrency,
		SweepEvery:        sweepEvery,
	}
	nodeclient.SetInLineHookForTest(&opts, func(kind string) {
		compute.mu.Lock()
		compute.inLine = append(compute.inLine, kind)
		compute.mu.Unlock()
	})
	nodeclient.SetWaitsBehindHookForTest(&opts, func(request int64) {
		compute.record("launch-waits " + strconv.FormatInt(request, 10))
	})

	var wg sync.WaitGroup

	wg.Go(func() {
		err := nodeclient.Run(ctx, c, compute, opts)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	})

	t.Cleanup(func() {
		compute.releaseAll()
		compute.commands.Wait()
		cancel()
		wg.Wait()
	})

	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	return p, compute, polls, p.Nodes()[0]
}

// taken waits until the node has taken n commands it has not answered and none is
// left queued for it.
func taken(t *testing.T, p *nodeplane.Plane, node string, n int) {
	t.Helper()

	waitFor(t, func() bool { return p.InflightForTest(node) == n && p.QueuedForTest(node) == 0 })
}

func wantOrder(t *testing.T, got, want []string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// before reports whether a is recorded, and before b if b is.
func before(events []string, a, b string) bool {
	i, j := slices.Index(events, a), slices.Index(events, b)

	return i >= 0 && (j < 0 || i < j)
}

func launchIn(t *testing.T, wg *sync.WaitGroup, p *nodeplane.Plane, i int, request int64) {
	t.Helper()

	wg.Go(func() {
		if err := p.NewRunner().Launch(t.Context(), testLease(i), dispatch.Job{RequestID: request}); err != nil {
			t.Errorf("Launch %d: %v", request, err)
		}
	})
}

func destroyIn(t *testing.T, wg *sync.WaitGroup, p *nodeplane.Plane, request int64) {
	t.Helper()

	wg.Go(func() {
		if err := p.NewRunner().Destroy(t.Context(), request); err != nil {
			t.Errorf("Destroy %d: %v", request, err)
		}
	})
}

// A LAUNCH IS TAKEN WHILE A TEARDOWN SETTLES. A completion's destroy can wait two
// minutes on a guest's Docker image store, and on the loop a few of those kept a
// launch unanswered past its ten-minute window: a consumer's lint job, 2026-11-20, was
// then never offered again and sat queued for ten hours. Here a destroy is held and
// a launch must still finish; and the poll must name the destroy, or the plane hands
// it back on every poll and never the launch.
func TestALaunchIsTakenWhileADestroySettles(t *testing.T) {
	t.Parallel()

	p, compute, _, _ := heldHarness(t, 4, 0)
	compute.holdDestroy(42)

	commands := &compute.commands
	destroyIn(t, commands, p, 42)
	waitFor(t, func() bool { return compute.seen("destroy-start 42") })

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 101})
	}()

	select {
	case err := <-launched:
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a launch waited behind a destroy that was still settling: %v", compute.eventsNow())
	}

	if compute.seen("destroy-done 42") {
		t.Fatal("the destroy had finished, so the launch overlapped nothing")
	}

	compute.releaseAll()
	commands.Wait()
}

// A DESTROY WAITS FOR THE LAUNCHES IT ARRIVED BEHIND, AND NO OTHERS: it never
// overtakes a launch that was in flight when it was queued, which may be its own
// lease's, and a launch started after it never holds it up.
func TestADestroyWaitsOnlyForTheLaunchesItArrivedBehind(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseFirst := compute.holdLaunch(201)
	releaseLater := compute.holdLaunch(202)

	commands := &compute.commands
	launchIn(t, commands, p, 1, 201)
	waitFor(t, func() bool { return compute.seen("launch-start l1") })

	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 43) }()

	// THE DESTROY IS IN THE NODE'S HANDS BEFORE THE LATER LAUNCH IS QUEUED.
	taken(t, p, node, 2)

	launchIn(t, commands, p, 2, 202)
	waitFor(t, func() bool { return compute.seen("launch-start l2") })

	if compute.seen("destroy-start 43") {
		t.Fatalf("the destroy started before the launch it arrived behind had finished: %v", compute.eventsNow())
	}

	releaseFirst()

	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the destroy waited for a launch that started after it: %v", compute.eventsNow())
	}

	if compute.seen("launch-done l2") {
		t.Fatal("the later launch had finished, so the destroy proved nothing about waiting for it")
	}

	// JOINED HERE, while t.Context is live: a launch still running when the test
	// returns is cancelled with it and reports that as a failure.
	releaseLater()
	commands.Wait()

	wantOrder(t, compute.eventsNow(), []string{
		"launch-start l1", "launch-start l2", "launch-done l1", "destroy-start 43", "destroy-done 43",
		"launch-done l2",
	})
}

// A LAUNCH OF A REQUEST WHOSE DESTROY IS OUT WAITS FOR IT, and nothing else does. A
// destroy names a request, not a machine: one that ran after the launch would tear
// down what the launch started. The plane keeps the launch until the destroy is
// answered, and gives the node the launch of another request meanwhile.
func TestALaunchWaitsForAnEarlierDestroyOfItsRequest(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	compute.holdDestroy(91)

	commands := &compute.commands
	destroyIn(t, commands, p, 91)
	waitFor(t, func() bool { return compute.seen("destroy-start 91") })

	launchIn(t, commands, p, 1, 91)
	waitFor(t, func() bool { return p.QueuedForTest(node) == 1 })

	if err := p.NewRunner().Launch(t.Context(), testLease(2), dispatch.Job{RequestID: 92}); err != nil {
		t.Fatalf("Launch 92: %v", err)
	}

	if p.QueuedForTest(node) != 1 || compute.seen("launch-start l1") {
		t.Fatalf("a launch was given while a destroy of its request was still running: %v", compute.eventsNow())
	}

	compute.releaseAll()
	commands.Wait()

	if events := compute.eventsNow(); !before(events, "destroy-done 91", "launch-start l1") ||
		!slices.Contains(events, "launch-done l1") {
		t.Fatalf("the launch did not follow the destroy of its request: %v", events)
	}
}

// DESTROYS RUN ONE AT A TIME, IN THE ORDER THEY ARRIVED, beside the loop as they did
// on it.
func TestDestroysBesideTheLoopRunInOrderOneAtATime(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseFirst := compute.holdDestroy(51)

	commands := &compute.commands
	destroyIn(t, commands, p, 51)
	waitFor(t, func() bool { return compute.seen("destroy-start 51") })

	destroyIn(t, commands, p, 52)
	taken(t, p, node, 2)

	// THE LOOP HAS GONE ON PAST THE SECOND, so it had its chance to start it.
	if err := p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 501}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if compute.seen("destroy-start 52") {
		t.Fatalf("a second destroy started while the first was still running: %v", compute.eventsNow())
	}

	releaseFirst()
	commands.Wait()

	wantOrder(t, compute.without("launch-start l1", "launch-done l1"),
		[]string{"destroy-start 51", "destroy-done 51", "destroy-start 52", "destroy-done 52"})
}

// THE CONTROL PLANE'S TEND AND SWEEP WAIT FOR A DESTROY IN FLIGHT AND HOLD NO LAUNCH. The
// reaper sends both every thirty seconds; in line, each held the loop until every queued
// teardown had settled, which is the stall that left a consumer's job queued for ten
// hours. Neither may run beside a teardown, and a launch is taken while they wait.
func TestTheReapersTendAndSweepWaitForADestroyAndHoldNoLaunch(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseDestroy := compute.holdDestroy(61)

	commands := &compute.commands
	destroyIn(t, commands, p, 61)
	waitFor(t, func() bool { return compute.seen("destroy-start 61") })

	commands.Go(func() {
		if err := p.NewRunner().Tend(t.Context()); err != nil {
			t.Errorf("Tend: %v", err)
		}
	})
	waitFor(t, func() bool { return p.InflightForTest(node) == 2 && p.QueuedForTest(node) == 0 })
	commands.Go(func() {
		if err := p.NewRunner().Sweep(t.Context()); err != nil {
			t.Errorf("Sweep: %v", err)
		}
	})
	taken(t, p, node, 3)

	if err := p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 601}); err != nil {
		t.Fatalf("a launch waited behind the reaper's tend and sweep: %v", err)
	}

	if compute.seen("tend") || compute.seen("sweep") {
		t.Fatalf("a tend or a sweep ran while a destroy was still running: %v", compute.eventsNow())
	}

	releaseDestroy()
	commands.Wait()

	wantOrder(t, compute.without("launch-start l1", "launch-done l1"),
		[]string{"destroy-start 61", "destroy-done 61", "tend", "sweep"})
}

// FOUR LAUNCHES WAITING ON TEARDOWNS LEAVE A FIFTH ITS SLOT. A launch waits on the node
// for a destroy of its request the plane gave up on; holding a slot while it did, four of
// them left the loop blocked for one, behind every teardown.
func TestLaunchesWaitingOnTeardownsHoldNoSlot(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	for i := range 4 {
		compute.holdDestroy(int64(91 + i))
	}

	type answer struct {
		request int64
		err     error
	}
	// GIVEN UP ON BY THEIR CALLERS once the node has them all, as the command timeout
	// would.
	giveUp, abandon := context.WithCancel(t.Context())
	destroyed := make(chan answer, 4)
	for i := range 4 {
		go func() {
			destroyed <- answer{int64(91 + i), p.NewRunner().Destroy(giveUp, int64(91+i))}
		}()
	}
	taken(t, p, node, 4)
	abandon()
	for range 4 {
		if a := <-destroyed; a.err == nil {
			t.Fatalf("destroy %d, still held on the node, was answered: %v", a.request, compute.eventsNow())
		}
	}

	launched := make(chan error, 4)
	for i := range 4 {
		go func() {
			launched <- p.NewRunner().Launch(t.Context(), testLease(i+1), dispatch.Job{RequestID: int64(91 + i)})
		}()
	}
	waitFor(t, func() bool {
		for i := range 4 {
			if !compute.seen("launch-waits " + strconv.Itoa(91+i)) {
				return false
			}
		}

		return true
	})

	if err := p.NewRunner().Launch(t.Context(), testLease(5), dispatch.Job{RequestID: 95}); err != nil {
		t.Fatalf("a fifth launch was not taken past four waiting on teardowns: %v", err)
	}

	compute.releaseAll()
	for range 4 {
		<-launched
	}
	waitFor(t, func() bool {
		for i := range 4 {
			if !compute.seen("launch-done l" + strconv.Itoa(i+1)) {
				return false
			}
		}

		return true
	})

	events := compute.eventsNow()
	for i := range 4 {
		if !before(events, "destroy-done "+strconv.Itoa(91+i), "launch-start l"+strconv.Itoa(i+1)) {
			t.Fatalf("launch %d did not follow the destroy of its request: %v", i+1, events)
		}
	}
}

// THE NODE'S OWN SWEEP TAKES A TURN BETWEEN DESTROYS. It must not run beside one, and
// waiting for none to be running starves it on a node that is always tearing down:
// custody for compute already gone is renewed and never settled.
func TestThePeriodicSweepTakesATurnBetweenDestroys(t *testing.T) {
	t.Parallel()

	p, compute, polls, node := heldHarness(t, 4, 10*time.Millisecond)
	releaseFirst := compute.holdDestroy(81)

	commands := &compute.commands
	destroyIn(t, commands, p, 81)
	waitFor(t, func() bool { return compute.seen("destroy-start 81") })

	// A WHOLE POLL WINDOW PASSES WITH THE DESTROY RUNNING, so the loop has been
	// round once with a sweep due.
	n := polls.n.Load()
	waitFor(t, func() bool { return polls.n.Load() >= n+2 })

	destroyIn(t, commands, p, 82)
	taken(t, p, node, 2)

	releaseFirst()
	commands.Wait()

	events := compute.eventsNow()
	done := slices.Index(events, "destroy-done 81")
	next := slices.Index(events, "destroy-start 82")
	if done < 0 || next < done || !slices.Contains(events[done:next], "sweep") {
		t.Fatalf("no sweep between a destroy and the one queued after the sweep was due: %v", events)
	}

	if i := slices.Index(events, "destroy-start 81"); slices.Contains(events[i:done], "sweep") {
		t.Fatalf("a sweep ran beside a destroy: %v", events)
	}
}

// A NODE IS GIVEN NO MORE DESTROYS THAN A POLL CAN NAME, and a launch still comes.
// One more held would be left out of every poll and given back as lost on each; and
// a node that took the rest in line would stop polling until every one had settled.
func TestALaunchIsTakenPastAsManyDestroysAsAPollNames(t *testing.T) {
	t.Parallel()

	p, compute, polls, node := heldHarness(t, 4, 0)
	compute.holdDestroy(1000)

	commands := &compute.commands
	destroyIn(t, commands, p, 1000)
	waitFor(t, func() bool { return compute.seen("destroy-start 1000") })

	for i := range nodeapi.MaxDestroysRunning {
		destroyIn(t, commands, p, int64(1001+i))
	}
	waitFor(t, func() bool {
		return p.InflightForTest(node) == nodeapi.MaxDestroysRunning && p.QueuedForTest(node) == 1
	})

	n := polls.n.Load()
	if err := p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 7000}); err != nil {
		t.Fatalf("a launch was not taken past a full line of destroys: %v", err)
	}
	if got := polls.n.Load() - n; got > 10 {
		t.Fatalf("%d polls for one launch: the node is polling as fast as the plane answers", got)
	}
	if p.QueuedForTest(node) != 1 || compute.ranInLine(nodeapi.CommandDestroy) {
		t.Fatal("the destroy past the bound was given to the node")
	}

	compute.releaseAll()
	commands.Wait()

	if got := len(compute.without("launch-start l1", "launch-done l1")); got != 2*(nodeapi.MaxDestroysRunning+1) {
		t.Fatalf("%d events, want a start and a finish for each of %d destroys", got, nodeapi.MaxDestroysRunning+1)
	}
}

// WHERE LAUNCHES DO NOT OVERLAP, A DESTROY STILL HOLDS THE LOOP: every provider but
// Firecracker runs one command at a time, as a node always did.
func TestADestroyHoldsTheLoopWhereLaunchesDoNotOverlap(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 1, 0)
	releaseDestroy := compute.holdDestroy(71)

	commands := &compute.commands
	destroyIn(t, commands, p, 71)
	waitFor(t, func() bool { return compute.ranInLine(nodeapi.CommandDestroy) && compute.seen("destroy-start 71") })

	launchIn(t, commands, p, 1, 301)
	waitFor(t, func() bool { return p.QueuedForTest(node) == 1 })

	releaseDestroy()
	commands.Wait()

	wantOrder(t, compute.eventsNow(),
		[]string{"destroy-start 71", "destroy-done 71", "launch-start l1", "launch-done l1"})
}

// AGAINST A PLANE OLDER THAN VersionDestroysBeside A DESTROY HOLDS THE LOOP, even
// where launches overlap: that plane gives back as lost a destroy a node polled past,
// on every poll, and never the command behind it.
func TestADestroyHoldsTheLoopAgainstAPlaneThatCannotBeToldWhatItHolds(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarnessAt(t, 4, 0, nodeapi.VersionDestroysBeside-1)
	releaseDestroy := compute.holdDestroy(75)

	commands := &compute.commands
	destroyIn(t, commands, p, 75)
	waitFor(t, func() bool { return compute.ranInLine(nodeapi.CommandDestroy) && compute.seen("destroy-start 75") })

	launchIn(t, commands, p, 1, 302)
	waitFor(t, func() bool { return p.QueuedForTest(node) == 1 })

	releaseDestroy()
	commands.Wait()

	wantOrder(t, compute.eventsNow(),
		[]string{"destroy-start 75", "destroy-done 75", "launch-start l1", "launch-done l1"})
}

// A LAUNCH WAITS ON THE NODE FOR A DESTROY OF ITS REQUEST THE PLANE GAVE UP ON. The
// plane keeps a launch back only while the destroy is out; once the destroy outlives
// the command timeout the launch is given, and the node, still holding the destroy,
// runs the launch after it.
func TestALaunchWaitsOnTheNodeForADestroyThePlaneGaveUpOn(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	compute.holdDestroy(91)

	// GIVEN UP ON BY ITS CALLER once the node has it, as the command timeout would.
	giveUp, abandon := context.WithCancel(t.Context())
	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(giveUp, 91) }()
	waitFor(t, func() bool { return compute.seen("destroy-start 91") })
	abandon()
	if err := <-destroyed; err == nil {
		t.Fatal("a destroy still held on the node was answered")
	}

	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 91}) }()

	// THE NODE HAS THE LAUNCH AND IS WAITING with it, on its own word; and a launch of
	// another request taken and finished since gives a launch that did not wait every
	// chance to have started.
	waitFor(t, func() bool { return compute.seen("launch-waits 91") })
	if err := p.NewRunner().Launch(t.Context(), testLease(2), dispatch.Job{RequestID: 92}); err != nil {
		t.Fatalf("Launch 92: %v", err)
	}

	if compute.seen("launch-start l1") {
		t.Fatalf("a launch started on the node while a destroy of its request ran: %v", compute.eventsNow())
	}

	compute.releaseAll()
	if err := <-launched; err != nil {
		t.Fatalf("Launch: %v", err)
	}

	wantOrder(t, compute.without("launch-start l2", "launch-done l2"),
		[]string{"destroy-start 91", "launch-waits 91", "destroy-done 91", "launch-start l1", "launch-done l1"})
	if p.QueuedForTest(node) != 0 {
		t.Fatal("a command was left queued")
	}
}

// THE CONTROL PLANE'S SWEEP NEVER RUNS BESIDE A LAUNCH TAKING CUSTODY OF ITS LOST REPORT.
// In line it could not: every launch had returned first. In the destroys' line a launch
// taken since may be in flight, and a sweep that retired a lease while that launch copied
// the running instance into custody would see its decision overwritten.
func TestTheReapersSweepWaitsForALostReportsCustody(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseDestroy := compute.holdDestroy(1)
	releaseLaunch := compute.holdLaunch(77)
	releaseCustody := compute.holdCustody()

	answers := make(chan error, 3)
	go func() { answers <- p.NewRunner().Destroy(t.Context(), 1) }()
	waitFor(t, func() bool { return compute.seen("destroy-start 1") })
	go func() { answers <- p.NewRunner().Sweep(t.Context()) }()
	taken(t, p, node, 2)

	go func() { answers <- p.NewRunner().Launch(t.Context(), testLease(1), dispatch.Job{RequestID: 77}) }()
	waitFor(t, func() bool { return compute.seen("launch-start l1") })

	// THE REPORT IS LOST: the plane forgets the node, so the launch's result is refused.
	p.ForgetForTest(node)
	releaseLaunch()
	waitFor(t, func() bool { return compute.seen("custody-start l1") })

	releaseDestroy()
	// THE SWEEP HAS REACHED ITS TURN AND IS WAITING FOR CUSTODY, on the node's own word.
	waitFor(t, func() bool { return compute.ranInLine(nodeapi.CommandSweep) })
	if compute.seen("sweep") {
		t.Fatalf("the sweep ran while a launch was taking custody: %v", compute.eventsNow())
	}

	releaseCustody()
	waitFor(t, func() bool { return compute.seen("sweep") })
	if events := compute.eventsNow(); !before(events, "custody-done l1", "sweep") {
		t.Fatalf("the sweep did not wait for the custody take: %v", events)
	}
}

// THE CONTROL PLANE'S SWEEP WAITS FOR THE LAUNCHES IN FLIGHT WHEN IT CAME, as it did in
// line: a sweep reads the host's compute against its leases, and one half started is
// neither.
func TestTheReapersSweepWaitsForTheLaunchesBeforeIt(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseLaunch := compute.holdLaunch(55)

	commands := &compute.commands
	launchIn(t, commands, p, 1, 55)
	waitFor(t, func() bool { return compute.seen("launch-start l1") })
	commands.Go(func() {
		if err := p.NewRunner().Sweep(t.Context()); err != nil {
			t.Errorf("Sweep: %v", err)
		}
	})
	taken(t, p, node, 2)

	if err := p.NewRunner().Launch(t.Context(), testLease(2), dispatch.Job{RequestID: 56}); err != nil {
		t.Fatalf("Launch 56: %v", err)
	}
	if compute.seen("sweep") {
		t.Fatalf("the sweep ran beside a launch in flight when it came: %v", compute.eventsNow())
	}

	releaseLaunch()
	commands.Wait()

	if events := compute.eventsNow(); !before(events, "launch-done l1", "sweep") {
		t.Fatalf("the sweep did not follow the launch before it: %v", events)
	}
}

// A DESTROY QUEUED BEHIND A LAUNCH WAITING ON ITS OWN REQUEST'S DESTROY WAITS FOR THAT
// LAUNCH TOO: the waiting launch is in flight from the moment it is taken, so a destroy of
// its request arriving after it can never overtake it.
func TestADestroyBehindAWaitingLaunchWaitsForIt(t *testing.T) {
	t.Parallel()

	p, compute, _, node := heldHarness(t, 4, 0)
	releaseFirst := compute.holdDestroy(91)
	releaseLaunch := compute.holdLaunch(91)

	giveUp, abandon := context.WithCancel(t.Context())
	destroyed := make(chan error, 1)
	go func() { destroyed <- p.NewRunner().Destroy(giveUp, 91) }()
	waitFor(t, func() bool { return compute.seen("destroy-start 91") })
	abandon()
	if err := <-destroyed; err == nil {
		t.Fatal("a destroy still held on the node was answered")
	}

	commands := &compute.commands
	launchIn(t, commands, p, 1, 91)
	waitFor(t, func() bool { return compute.seen("launch-waits 91") })
	destroyIn(t, commands, p, 91)
	taken(t, p, node, 2)

	releaseFirst()
	waitFor(t, func() bool { return compute.seen("launch-start l1") })
	if err := p.NewRunner().Launch(t.Context(), testLease(2), dispatch.Job{RequestID: 92}); err != nil {
		t.Fatalf("Launch 92: %v", err)
	}
	if n := strings.Count(strings.Join(compute.eventsNow(), "|"), "destroy-start 91"); n != 1 {
		t.Fatalf("the later destroy started while the launch it arrived behind ran: %v", compute.eventsNow())
	}

	releaseLaunch()
	commands.Wait()

	wantOrder(t, compute.without("launch-start l2", "launch-done l2"), []string{
		"destroy-start 91", "launch-waits 91", "destroy-done 91", "launch-start l1", "launch-done l1",
		"destroy-start 91", "destroy-done 91",
	})
}
