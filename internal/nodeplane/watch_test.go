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

// TWO REGISTRATIONS CAN COMMIT IN ONE ORDER AND ARRIVE IN THE OTHER.
//
// The ledger write happens BEFORE the plane's mutex is taken, so a slow
// registration can be overtaken: A commits and is handed epoch 1, B commits and
// is handed epoch 2, B installs 2, and then A finally reaches the lock. Installing
// unconditionally lets the older token win, and the damage lands later and
// somewhere else — expiry presents epoch 1, the fenced write matches nothing,
// and the ledger goes on believing in a host the plane has forgotten, advertising
// capacity for a machine nobody can reach.
//
// STAGED RATHER THAN REASONED ABOUT. The fake holds A inside its ledger write
// until B has finished, which is precisely the interleaving and is otherwise
// only reachable by luck.
func TestALateRegistrationCannotSupersedeTheNewerProcess(t *testing.T) {
	t.Parallel()

	led := newLedger()
	p := testPlane(t, WithRegistrar(led))

	// Released once the second registration has been fully installed.
	proceed := make(chan struct{})
	led.holdFirst = proceed

	first := make(chan struct{})

	go func() {
		defer close(first)

		if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "old", VCPU: 8, Memory: 32 * config.GiB,
		}); err != nil {
			t.Errorf("first Register: %v", err)
		}
	}()

	// A is inside the ledger write holding the older epoch.
	waitFor(t, "the first registration to reach the ledger", func() bool {
		return led.held()
	})

	// B arrives and completes: a different process, on a different backend.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderFirecracker,
		Deployment: deployment, Incarnation: "new", VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("second Register: %v", err)
	}

	close(proceed)
	<-first

	// EVERY FIELD, NOT JUST THE EPOCH. Keeping the epoch monotonic while letting
	// the rest of the registration through was the bug: the older request still
	// tombstoned the newer process's in-flight commands, handed their leases to
	// custody, and overwrote the incarnation and provider — so the ledger and the
	// command plane disagreed about who owned the node and what it ran.
	if got := p.epochForTest("n1"); got != 2 {
		t.Errorf("ledger epoch = %d, want 2", got)
	}

	if got := p.incarnationForTest("n1"); got != "new" {
		t.Errorf("incarnation = %q, want the newer process; a registration that committed "+
			"FIRST but arrived LAST superseded the one that replaced it", got)
	}

	if got := p.providerForTest("n1"); got != config.ProviderFirecracker {
		t.Errorf("provider = %q, want the newer process's; the command plane now disagrees "+
			"with the ledger about what this host runs", got)
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
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}
