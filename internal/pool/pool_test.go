package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

// fakeLedger keeps orders the way migration 73 does: one-way moves from one state.
type fakeLedger struct {
	mu      sync.Mutex
	now     func() time.Time
	orders  []alloc.PoolOrder
	tokens  []string
	reports map[string]alloc.TierCapacity
	readErr error
}

func (l *fakeLedger) RecordPoolOrder(_ context.Context, o alloc.PoolOrder) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.orders {
		if l.orders[i].ID == o.ID {
			return errors.New("duplicate order")
		}
	}
	o.State, o.FirstAttempt, o.Updated = alloc.PoolOrdering, l.now(), l.now()
	l.orders = append(l.orders, o)

	return nil
}

func (l *fakeLedger) PoolOrders(context.Context) ([]alloc.PoolOrder, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]alloc.PoolOrder(nil), l.orders...), nil
}

func (l *fakeLedger) move(id, from, to, host, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.orders {
		if l.orders[i].ID == id && l.orders[i].State == from {
			l.orders[i].State, l.orders[i].Reason, l.orders[i].Updated = to, reason, l.now()
			if host != "" {
				l.orders[i].HostID = host
			}

			return nil
		}
	}

	return alloc.ErrPoolOrderMoved
}

func (l *fakeLedger) PoolOrderPlaced(_ context.Context, id, host string) error {
	return l.move(id, alloc.PoolOrdering, alloc.PoolPlaced, host, "")
}

func (l *fakeLedger) PoolOrderUncertain(_ context.Context, id, reason string) error {
	return l.move(id, alloc.PoolOrdering, alloc.PoolUncertain, "", reason)
}

func (l *fakeLedger) PoolOrderRefused(_ context.Context, id, reason string) error {
	return l.move(id, alloc.PoolOrdering, alloc.PoolRefused, "", reason)
}

func (l *fakeLedger) NewJoinToken(_ context.Context, ttl time.Duration, uses int, note string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if uses != 1 || ttl <= 0 || !strings.HasPrefix(note, "pool order ") {
		return "", fmt.Errorf("token asked with uses %d, ttl %v, note %q", uses, ttl, note)
	}
	tok := fmt.Sprintf("tok-%d", len(l.tokens)+1)
	l.tokens = append(l.tokens, tok)

	return tok, nil
}

func (l *fakeLedger) CapacityReports(context.Context) (map[string]alloc.TierCapacity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.reports, l.readErr
}

func (l *fakeLedger) states() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]string{}
	for i := range l.orders {
		out[l.orders[i].ID] = l.orders[i].State
	}

	return out
}

// fakeSource answers orders from a script, and records that each order it was asked
// for was already in the ledger: an order is committed before its source is asked.
type fakeSource struct {
	t          *testing.T
	ledger     *fakeLedger
	mu         sync.Mutex
	asked      []hostsource.Request
	answer     func(req hostsource.Request) (hostsource.Host, error)
	find       func(order string) (hostsource.Host, bool, error)
	idempotent time.Duration
}

func (s *fakeSource) Name() string { return "fake" }

func (s *fakeSource) Order(_ context.Context, req hostsource.Request) (hostsource.Host, error) {
	if s.ledger.states()[req.Order] != alloc.PoolOrdering {
		s.t.Errorf("order %s was sent before it was committed as ordering", req.Order)
	}
	s.mu.Lock()
	s.asked = append(s.asked, req)
	s.mu.Unlock()
	if s.answer == nil {
		return hostsource.Host{ID: "host-" + req.Order}, nil
	}

	return s.answer(req)
}

func (s *fakeSource) Find(_ context.Context, order string) (hostsource.Host, bool, error) {
	if s.find == nil {
		return hostsource.Host{}, false, nil
	}

	return s.find(order)
}

func (s *fakeSource) Describe(context.Context, string) (hostsource.Host, error) {
	return hostsource.Host{}, nil
}
func (s *fakeSource) Gone(context.Context, string) (hostsource.Presence, error) {
	return hostsource.PresenceUnknown, nil
}
func (s *fakeSource) Release(context.Context, hostsource.ReleaseProof) error { return nil }
func (s *fakeSource) Terms(string) (hostsource.Terms, error) {
	return hostsource.Terms{IdempotentFor: s.idempotent}, nil
}

func (s *fakeSource) requests() []hostsource.Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]hostsource.Request(nil), s.asked...)
}

type rig struct {
	now    time.Time
	ledger *fakeLedger
	first  *fakeSource
	second *fakeSource
	pool   *Pool
	boots  map[string]string
}

func newRig(t *testing.T, policy Policy, tiers ...config.Tier) *rig {
	t.Helper()
	r := &rig{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), boots: map[string]string{}}
	clock := func() time.Time { return r.now }
	r.ledger = &fakeLedger{now: clock, reports: map[string]alloc.TierCapacity{}}
	r.first = &fakeSource{t: t, ledger: r.ledger, idempotent: 10 * time.Minute}
	r.second = &fakeSource{t: t, ledger: r.ledger, idempotent: 10 * time.Minute}
	if policy.Choices == nil {
		policy.Choices = []Choice{{Source: "first", Shape: "z1d.metal"}, {Source: "second", Shape: "s2.c2.small", Location: "PHX"}}
	}
	n := 0
	var mu sync.Mutex
	p, err := New(r.ledger, Config{
		Archs:          map[config.Arch]Policy{config.ArchX64: policy},
		Sources:        map[string]hostsource.Source{"first": r.first, "second": r.second},
		RefusalBackoff: 10 * time.Minute,
		Tiers:          tiers,
		Boot: func(order, token string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			r.boots[order] = token

			return []byte("join " + token), nil
		},
	}, WithClock(clock), WithIDs(func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++

		return fmt.Sprintf("o-%d", n), nil
	}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r.pool = p

	return r
}

func firecrackerTier(label string, arch config.Arch) config.Tier {
	return config.Tier{Label: label, Provider: config.ProviderFirecracker, Arch: arch}
}

func (r *rig) waiting(label string, n int) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.reports[label] = alloc.TierCapacity{Listener: alloc.ListenerCapacity{Waiting: n}}
}

func step(t *testing.T, r *rig) {
	t.Helper()
	if err := r.pool.Step(t.Context()); err != nil {
		t.Fatalf("step: %v", err)
	}
}

// THE FLOOR IS KEPT: an order is committed, sent with its own single-use join token in
// its boot, and placed when the source names a host; a kept floor orders nothing more.
func TestTheFloorIsKept(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 2, Max: 4})
	step(t, r)
	step(t, r)
	step(t, r)
	if got := r.ledger.states(); len(got) != 2 || got["o-1"] != alloc.PoolPlaced || got["o-2"] != alloc.PoolPlaced {
		t.Fatalf("orders = %v, want two placed", got)
	}
	asked := r.first.requests()
	if len(asked) != 2 || asked[0].Shape != "z1d.metal" || asked[0].Arch != config.ArchX64 {
		t.Fatalf("asked %+v", asked)
	}
	if string(asked[0].Boot.Reveal()) != "join "+r.boots["o-1"] || r.boots["o-1"] == r.boots["o-2"] {
		t.Fatalf("boots = %v, payload %q", r.boots, asked[0].Boot.Reveal())
	}
	if len(r.second.requests()) != 0 {
		t.Fatal("the second choice was asked while the first answered")
	}
}

// A FIRECRACKER TIER WITH JOBS WAITING IS DEMAND, one host at a time while any is on
// its way, and never past the maximum; another provider's waiting tier, or one whose
// architecture is not named, is not demand for the pool.
func TestDemandGrowsTheFloorOneHostAtATime(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 0, Max: 2},
		firecrackerTier("fc-x64", config.ArchX64),
		config.Tier{Label: "ec2", Provider: config.ProviderEC2, Arch: config.ArchX64},
		config.Tier{Label: "fc-any", Provider: config.ProviderFirecracker})
	r.waiting("ec2", 5)
	r.waiting("fc-any", 5)
	step(t, r)
	if n := len(r.ledger.states()); n != 0 {
		t.Fatalf("%d orders for demand the pool does not serve", n)
	}
	r.waiting("fc-x64", 3)
	step(t, r)
	step(t, r)
	if n := len(r.ledger.states()); n != 1 {
		t.Fatalf("%d orders while one host was on its way, want 1", n)
	}
	// The host joined: the next is ordered for the demand still waiting.
	r.ledger.mu.Lock()
	r.ledger.orders[0].NodeName = "pool-o-1"
	r.ledger.mu.Unlock()
	step(t, r)
	step(t, r)
	if n := len(r.ledger.states()); n != 2 {
		t.Fatalf("%d orders, want 2", n)
	}
	r.ledger.mu.Lock()
	r.ledger.orders[1].NodeName = "pool-o-2"
	r.ledger.mu.Unlock()
	step(t, r)
	if n := len(r.ledger.states()); n != 2 {
		t.Fatalf("%d orders past the maximum of 2", n)
	}
}

// A REFUSAL IS AN ANSWER: the order is refused, the next choice is asked, and the
// refused one is passed over until the backoff passes. An order that never reached the
// source is the same.
func TestARefusedChoiceIsPassedOverForTheBackoff(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"no stock": hostsource.ErrNoStock, "quota": hostsource.ErrQuota,
		"refused": hostsource.ErrRefused, "not sent": hostsource.ErrNotSent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, Policy{Floor: 1, Max: 1})
			r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, refusal }
			step(t, r)
			step(t, r)
			got := r.ledger.states()
			if got["o-1"] != alloc.PoolRefused || got["o-2"] != alloc.PoolPlaced || len(r.second.requests()) != 1 {
				t.Fatalf("orders = %v; second asked %d times", got, len(r.second.requests()))
			}
			r.ledger.mu.Lock()
			r.ledger.orders[1].State = alloc.PoolReleased // the floor is short again
			r.ledger.mu.Unlock()
			r.now = r.now.Add(11 * time.Minute)
			r.first.answer = nil
			step(t, r)
			if got := r.ledger.states(); got["o-3"] != alloc.PoolPlaced || len(r.first.requests()) != 2 {
				t.Fatalf("after the backoff: %v, first asked %d times", got, len(r.first.requests()))
			}
		})
	}
}

// AN ANSWER THAT IS NOT KNOWN leaves the order being ordered, and the next step reads
// it back before it does anything else: found, it is placed; listed as absent inside
// the source's window, it is sent again under the same order; past the window, it is
// uncertain, and nothing more is ordered for it.
func TestALostAnswerIsReadBackBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	lost := func(hostsource.Request) (hostsource.Host, error) {
		return hostsource.Host{}, errors.New("connection reset")
	}

	r := newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = lost
	step(t, r)
	r.first.find = func(order string) (hostsource.Host, bool, error) {
		return hostsource.Host{ID: "found-" + order}, true, nil
	}
	step(t, r)
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolPlaced {
		t.Fatalf("found: %v", got)
	}

	r = newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = lost
	step(t, r)
	r.first.answer = nil
	r.now = r.now.Add(5 * time.Minute)
	step(t, r)
	asked := r.first.requests()
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolPlaced || len(asked) != 2 || asked[1].Order != "o-1" {
		t.Fatalf("resent inside the window: %v, asked %+v", got, asked)
	}

	r = newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = lost
	step(t, r)
	r.now = r.now.Add(11 * time.Minute)
	step(t, r)
	step(t, r)
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolUncertain || len(r.first.requests()) != 1 {
		t.Fatalf("past the window: %v, asked %d times", got, len(r.first.requests()))
	}

	r = newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = lost
	step(t, r)
	r.first.find = func(string) (hostsource.Host, bool, error) { return hostsource.Host{}, false, errors.New("timeout") }
	if err := r.pool.Step(t.Context()); err == nil {
		t.Fatal("a failed read back was not reported")
	}
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolOrdering || len(r.first.requests()) != 1 {
		t.Fatalf("a failed read back decided something: %v, asked %d times", got, len(r.first.requests()))
	}
}

// A SOURCE THAT CANNOT DEDUPLICATE NEVER HAS AN ORDER SENT TWICE: a lost answer it
// lists no host for is uncertain at once.
func TestASourceWithoutDeduplicationIsNeverResent(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 1})
	r.first.idempotent = 0
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	step(t, r)
	step(t, r)
	if got := r.ledger.states(); got["o-1"] != alloc.PoolUncertain || len(r.first.requests()) != 1 {
		t.Fatalf("%v, asked %d times", got, len(r.first.requests()))
	}
}

// DEMAND THAT CANNOT BE READ IS NOT NO DEMAND: the floor is still kept, growth waits,
// and the step says it could not read.
func TestUnreadableDemandStillKeepsTheFloor(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 3}, firecrackerTier("fc", config.ArchX64))
	r.ledger.readErr = errors.New("database is locked")
	if err := r.pool.Step(t.Context()); err == nil {
		t.Fatal("an unreadable demand was not reported")
	}
	if got := r.ledger.states(); len(got) != 1 {
		t.Fatalf("orders = %v, want the floor's one", got)
	}
}

func TestAChoiceNamingNoSourceIsRefused(t *testing.T) {
	t.Parallel()
	_, err := New(&fakeLedger{}, Config{
		Archs:   map[config.Arch]Policy{config.ArchX64: {Max: 1, Choices: []Choice{{Source: "missing", Shape: "x"}}}},
		Sources: map[string]hostsource.Source{},
		Boot:    func(string, string) ([]byte, error) { return nil, nil },
	})
	if err == nil {
		t.Fatal("a choice naming no configured source was accepted")
	}
}
