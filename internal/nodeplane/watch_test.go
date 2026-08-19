package nodeplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// ledger is a Registrar that records what the plane told it.
type ledger struct {
	// reconciled records the hosts whose quarantined capacity was reconciled
	// against what they said they were running.
	reconciled      []string
	reportedRunning []string

	mu    sync.Mutex
	epoch int64
	gone  map[string]int64

	// failures is how many NodeGone calls to reject before accepting, so a
	// transient database can be modelled.
	failures int

	// holdFirst, when set, blocks the FIRST registration inside the ledger write
	// until it is closed — after its epoch has been allocated. That is what lets a
	// test stage a registration being overtaken by a later one.
	holdFirst chan struct{}
	holding   bool
}

func newLedger() *ledger { return &ledger{gone: map[string]int64{}} }

func (l *ledger) RegisterNode(_ context.Context, _ alloc.NodeRegistration) (int64, error) {
	l.mu.Lock()
	l.epoch++
	mine := l.epoch

	// The epoch is allocated BEFORE the wait, which is the whole point: this
	// caller committed first and will arrive last.
	hold := l.holdFirst
	if hold != nil {
		l.holdFirst, l.holding = nil, true
	}

	l.mu.Unlock()

	if hold != nil {
		<-hold

		l.mu.Lock()
		l.holding = false
		l.mu.Unlock()
	}

	return mine, nil
}

// held reports whether a registration is parked inside the ledger write.
func (l *ledger) held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.holding
}

func (l *ledger) currentEpoch() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.epoch
}

func (l *ledger) NodeGone(_ context.Context, name string, epoch int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.failures > 0 {
		l.failures--

		return errors.New("ledger unavailable")
	}

	l.gone[name] = epoch

	return nil
}

// ResolveQuarantineFor records what a returning host said it is running.
func (l *ledger) ResolveQuarantineFor(
	_ context.Context, node string, running []string, _ int64,
) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.reconciled = append(l.reconciled, node)
	l.reportedRunning = running

	return 0, nil
}

func (l *ledger) ResolveQuarantineForCompletion(
	context.Context, string, string, int64, int64, alloc.Phase,
) (bool, error) {
	return true, nil
}

func (l *ledger) ForgetEveryNode(context.Context) error { return nil }

// wasToldGone reports whether the ledger has been told this host is gone.
func (l *ledger) wasToldGone(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, ok := l.gone[name]

	return ok
}

// NOTHING ASKED, SO NOTHING NOTICED.
//
// Expiry ran only where its answer was needed — picking a node, listing the
// fleet, broadcasting a destroy — which was enough while that was all it
// decided. It is not enough once liveness decides what a tier ADVERTISES: an
// idle deployment does none of those three things, so a host that crashed on a
// quiet afternoon kept its capacity advertised until somebody happened to launch
// something, and GitHub went on assigning against a machine that was gone.
//
// THE ASSERTION IS THE LEDGER, not the plane's own map. An earlier version
// checked only that the node had been deleted from memory, which the timer does
// on its way past — so it passed while the ledger went on believing in the host
// forever, which is the half that actually matters.
func TestASilentNodeIsRecordedGoneWithNobodyAsking(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	led := newLedger()

	p := testPlane(t, WithClock(clock.now), WithPollTimeout(20*time.Millisecond),
		WithRegistrar(led))
	register(t, p, "n1", config.ProviderDocker)

	go p.Watch(t.Context())

	clock.advancePastSilence()

	waitFor(t, "the ledger to be told the node is gone", func() bool {
		return led.wasToldGone("n1")
	})
}

// A DATABASE THAT BLINKED MUST NOT COST A PERMANENT LIE.
//
// The node is deleted from the plane's map the moment it is expired, so nothing
// can rediscover it — if the one write that records the fact fails, the ledger
// believes in that machine until it registers again or the process restarts, and
// goes on advertising its capacity the whole time. The fact is queued until it
// is actually written.
func TestARecordingThatFailedIsRetried(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	led := newLedger()
	led.failures = 2

	p := testPlane(t, WithClock(clock.now), WithPollTimeout(20*time.Millisecond),
		WithRegistrar(led))
	register(t, p, "n1", config.ProviderDocker)

	go p.Watch(t.Context())

	clock.advancePastSilence()

	waitFor(t, "the retried recording to land", func() bool {
		return led.wasToldGone("n1")
	})
}

// EXPIRY REACHED ANY OTHER WAY IS STILL REMEMBERED.
//
// Most expiry happens incidentally: something picks a node, lists the fleet or
// answers a poll, and prunes the dead on its way past. Those callers hold the
// mutex and cannot write to a database, and once a node is deleted from the map
// nothing can find it again — so a version that handed them the fact to record
// simply lost it, and the ledger believed in the machine forever.
func TestANodeExpiredBySomethingElseIsStillRecorded(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	led := newLedger()

	p := testPlane(t, WithClock(clock.now), WithPollTimeout(20*time.Millisecond),
		WithRegistrar(led))
	register(t, p, "n1", config.ProviderDocker)

	clock.advancePastSilence()

	// Nodes() expires as a side effect of answering, which is exactly the kind of
	// caller that used to drop the fact on the floor.
	if got := p.Nodes(); len(got) != 0 {
		t.Fatalf("the node was not expired by the incidental caller: %v", got)
	}

	// The timer starts only now, so anything it records must have been queued by
	// the caller above rather than rediscovered.
	go p.Watch(t.Context())

	waitFor(t, "the incidental expiry to reach the ledger", func() bool {
		return led.wasToldGone("n1")
	})
}

// TWO REGISTRATIONS FOR ONE NAME MUST SHARE ONE COMMIT ORDER.
//
// The allocator write and plane install are one logical operation. If A can
// pause between them while B commits and installs, A can arrive last and leave
// the two authorities describing different processes. A per-node guard keeps B
// out of the allocator until A has installed, without blocking other node names.
//
// STAGED RATHER THAN REASONED ABOUT. The test holds A after its durable call has
// returned but before its plane install, then observes that B cannot enter the
// ledger until A finishes.
func TestALateRegistrationCannotSupersedeTheNewerProcess(t *testing.T) {
	t.Parallel()

	led := newLedger()
	committed := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	p := testPlane(t, WithRegistrar(led), func(p *Plane) {
		p.afterRegisterNodeForTest = func(ctx context.Context, _ string, _ int64) {
			once.Do(func() {
				close(committed)
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			})
		}
	})

	first := make(chan error, 1)

	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "old", VCPU: 8, Memory: 32 * config.GiB,
		})
		first <- err
	}()

	// A has committed its older epoch but has not installed it in the plane.
	<-committed
	if !registrationGuardHeldForTest(p, "n1") {
		t.Fatal("registration guard was released in the post-commit, pre-install gap")
	}

	second := make(chan error, 1)
	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "n1", Provider: config.ProviderFirecracker,
			Deployment: deployment, Incarnation: "new", VCPU: 8, Memory: 32 * config.GiB,
		})
		second <- err
	}()
	waitFor(t, "the second registration to queue behind the first", func() bool {
		return registrationCountForTest(p, "n1") == 2
	})

	select {
	case err := <-second:
		t.Fatalf("second registration crossed the first one's unfinished install: %v", err)
	default:
	}
	if got := led.currentEpoch(); got != 1 {
		t.Fatalf("second registration reached the ledger before the first installed: epoch=%d", got)
	}

	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Register: %v", err)
	}

	if got := p.epochForTest("n1"); got != 2 {
		t.Errorf("ledger epoch = %d, want 2", got)
	}

	if got := p.incarnationForTest("n1"); got != "new" {
		t.Errorf("incarnation = %q, want the second serialized process", got)
	}

	if got := p.providerForTest("n1"); got != config.ProviderFirecracker {
		t.Errorf("provider = %q, want the second serialized process's", got)
	}
}

func registrationCountForTest(p *Plane, node string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.registering[node]
}

func registrationGuardHeldForTest(p *Plane, node string) bool {
	p.mu.Lock()
	guard := p.registrationGuards[node]
	p.mu.Unlock()
	if guard == nil {
		return false
	}
	if guard.TryLock() {
		guard.Unlock()

		return false
	}

	return true
}

func TestARegistrationDoesNotBlockAnotherNode(t *testing.T) {
	t.Parallel()

	led := newLedger()
	p := testPlane(t, WithRegistrar(led))
	proceed := make(chan struct{})
	led.holdFirst = proceed

	first := make(chan error, 1)
	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "slow", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "slow-1", VCPU: 8, Memory: 32 * config.GiB,
		})
		first <- err
	}()
	waitFor(t, "the slow registration to reach the ledger", led.held)

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "independent", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: "independent-1", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("independent Register: %v", err)
	}
	if got := p.incarnationForTest("independent"); got != "independent-1" {
		t.Errorf("independent incarnation = %q, want registration to install", got)
	}

	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("slow Register: %v", err)
	}
}

// incarnationForTest reports which process the plane believes owns a node.
func (p *Plane) incarnationForTest(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return ""
	}

	return n.incarnation
}

// providerForTest reports the backend the plane believes a node runs.
func (p *Plane) providerForTest(name string) config.ProviderKind {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return ""
	}

	return n.provider
}

// epochForTest reports the ledger epoch the plane holds for a node.
func (p *Plane) epochForTest(name string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return -1
	}

	return n.ledgerEpoch
}

// waitFor polls a condition rather than sleeping a fixed amount.
//
// THE DEADLINE IS FAR LARGER THAN ANY OF THESE WAITS NEEDS, deliberately. What is
// being waited for — a goroutine reaching a poll — takes microseconds when it is
// scheduled promptly, so this number is not a budget for the work. It is a bound
// on how long a STALL is tolerated before the test gives up.
//
// Five seconds looked ample and was not. Under the full suite these run alongside
// every other package, with -race and -covermode=atomic both adding scheduling
// overhead, and a goroutine can go unscheduled for that long on a loaded machine.
// The failure then names the wait rather than anything the test is about, which
// is the most expensive kind of red: it reads as a defect in the code under test.
//
// A generous deadline costs nothing when the condition holds, because the loop
// exits the moment it does. A genuine failure still fails, just later. Slow is a
// better failure mode than false.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}
