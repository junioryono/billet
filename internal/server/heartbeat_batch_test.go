package server

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// commitCounter counts the write transactions a ledger committed.
type commitCounter struct {
	mu      sync.Mutex
	commits int
}

func (c *commitCounter) WriteWaited(time.Duration) {}
func (c *commitCounter) WriteRetried()             {}

func (c *commitCounter) WriteHeld(_ time.Duration, committed bool) {
	if !committed {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.commits++
}

func (c *commitCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.commits
}

// A HEARTBEAT PASS RENEWS ITS LEASES IN ONE COMMIT, and each lease is still
// judged as its own renewal would have been. Forty running jobs, a held lease
// and a running one the listener holds at an epoch the ledger has moved past
// are renewed by one write transaction; the forty stay running, the held one
// stays held, and the fenced one leaves running for the cleanup set, exactly
// as a lost lease does.
func TestAHeartbeatPassRenewsItsLeasesInOneCommit(t *testing.T) {
	t.Parallel()

	db := openState(t)
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 1 << 10, MaxMemory: 1 << 12 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "test-host-firecracker", Provider: config.ProviderFirecracker,
		VCPU: 1 << 20, Memory: 1 << 20 * config.GiB,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	for request := int64(1); request <= 40; request++ {
		holdRunning(t, l, a, tiers[0].Label, request)
	}

	held, err := a.Reserve(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	lost := holdRunning(t, l, a, tiers[0].Label, 41)
	stale := *lost
	stale.Epoch++

	l.mu.Lock()
	l.held = append(l.held, held)
	l.running[41] = &stale
	for _, lease := range append(l.held, l.running[1]) {
		l.confirmed[lease.ID] = time.Now()
	}
	l.mu.Unlock()

	commits := &commitCounter{}
	db.Observe(commits)

	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	l.mu.Unlock()

	if got := commits.count(); got != 1 {
		t.Fatalf("a pass over 42 leases committed %d write transactions, want 1", got)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for request := int64(1); request <= 40; request++ {
		if _, ok := l.running[request]; !ok {
			t.Fatalf("running job %d left the books after its lease was renewed", request)
		}
	}

	if len(l.held) != 1 || l.held[0] != held {
		t.Fatalf("held escrow = %v after a renewal, want the one lease", l.held)
	}

	if _, ok := l.running[41]; ok {
		t.Fatal("a lease the ledger fenced is still counted as running")
	}

	if _, owed := l.cleanup[41]; !owed {
		t.Fatal("a running lease that was lost was not owed a destroy")
	}
}

// A RENEWAL IS CONFIRMED AS OF WHEN IT WAS ASKED FOR. The allocator writes a
// lease's expiry from when its batch ran, and returns after its last batch;
// confirming at the return would date an early batch's renewal after the
// expiry it wrote, and keep a lease advertised past the moment the reaper may
// take it. In a bubble, twenty seconds pass between the asking and the answer,
// and the confirmation must not move with them.
func TestARenewalIsConfirmedAsOfWhenItWasAskedFor(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tiers := []config.Tier{tier("billet-4vcpu-a")}
		a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

		l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))
		lease := holdRunning(t, l, a, tiers[0].Label, 7)

		const answeredAfter = 20 * time.Second

		l.heartbeatRenew = func() { time.Sleep(answeredAfter) }

		asked := time.Now()

		l.mu.Lock()
		l.heartbeatHeld(t.Context())
		confirmed, seen := l.confirmed[lease.ID]
		l.mu.Unlock()

		if answered := time.Since(asked); answered < answeredAfter {
			t.Fatalf("the pass answered %v after it asked, so the delay it is about never happened", answered)
		}

		if !seen || !confirmed.Equal(asked) {
			t.Fatalf("the renewal was confirmed at %v (seen %v), want %v, when it was asked for", confirmed, seen, asked)
		}
	})
}
