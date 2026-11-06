package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

// fakeLedger keeps orders the way migration 73 does: one-way moves from one state, and
// a placed order names its host.
type fakeLedger struct {
	mu        sync.Mutex
	now       func() time.Time
	orders    []alloc.PoolOrder
	tokens    []string
	reports   map[string]alloc.TierCapacity
	readErr   error
	tokenErr  error
	listCalls int
}

func (l *fakeLedger) RecordPoolOrder(_ context.Context, o alloc.PoolOrder) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if o.Scope == "" {
		return errors.New("an order without its scope")
	}
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
	l.listCalls++

	return append([]alloc.PoolOrder(nil), l.orders...), nil
}

func (l *fakeLedger) move(id, from, to, host, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.orders {
		if l.orders[i].ID == id && l.orders[i].State == from {
			l.orders[i].State, l.orders[i].Reason, l.orders[i].Updated = to, reason, l.now()
			l.orders[i].HostID = host

			return nil
		}
	}

	return alloc.ErrPoolOrderMoved
}

func (l *fakeLedger) PoolOrderPlaced(_ context.Context, id, host string) error {
	if host == "" {
		return errors.New("a placed order without its host")
	}

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
	if l.tokenErr != nil {
		return "", l.tokenErr
	}
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

func (l *fakeLedger) snapshot() map[string]alloc.PoolOrder {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]alloc.PoolOrder{}
	for i := range l.orders {
		out[l.orders[i].ID] = l.orders[i]
	}

	return out
}

func (l *fakeLedger) states() map[string]string {
	out := map[string]string{}
	for id, o := range l.snapshot() { //nolint:gocritic // a test's handful of orders
		out[id] = o.State
	}

	return out
}

// fakeSource answers orders from a script, and records that each order it was asked
// for was already committed: an order is committed before its source is asked.
type fakeSource struct {
	t          *testing.T
	ledger     *fakeLedger
	scope      string
	scopeErr   error
	shapes     []string
	mu         sync.Mutex
	asked      []hostsource.Request
	found      []string
	answer     func(req hostsource.Request) (hostsource.Host, error)
	find       func(order string) (hostsource.Host, bool, error)
	idempotent time.Duration
}

func (s *fakeSource) Name() string { return "fake" }

func (s *fakeSource) Scope(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.scope, s.scopeErr
}

func (s *fakeSource) Order(_ context.Context, req hostsource.Request) (hostsource.Host, error) {
	if s.ledger.states()[req.Order] != alloc.PoolOrdering {
		s.t.Errorf("order %s was sent before it was committed as ordering", req.Order)
	}
	s.mu.Lock()
	s.asked = append(s.asked, req)
	answer := s.answer
	s.mu.Unlock()
	if answer == nil {
		return hostsource.Host{ID: "host-" + req.Order}, nil
	}

	return answer(req)
}

func (s *fakeSource) Find(_ context.Context, order string) (hostsource.Host, bool, error) {
	s.mu.Lock()
	s.found = append(s.found, order)
	find := s.find
	s.mu.Unlock()
	if find == nil {
		return hostsource.Host{}, false, nil
	}

	return find(order)
}

func (s *fakeSource) Describe(context.Context, string) (hostsource.Host, error) {
	return hostsource.Host{}, nil
}
func (s *fakeSource) Gone(context.Context, string) (hostsource.Presence, error) {
	return hostsource.PresenceUnknown, nil
}
func (s *fakeSource) Release(context.Context, hostsource.ReleaseProof) error { return nil }

func (s *fakeSource) Terms(shape string) (hostsource.Terms, error) {
	for _, known := range s.shapes {
		if known == shape {
			return hostsource.Terms{IdempotentFor: s.idempotent}, nil
		}
	}

	return hostsource.Terms{}, fmt.Errorf("no shape %q", shape)
}

func (s *fakeSource) requests() []hostsource.Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]hostsource.Request(nil), s.asked...)
}

func (s *fakeSource) finds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.found...)
}

type rig struct {
	now    time.Time
	ledger *fakeLedger
	first  *fakeSource
	second *fakeSource
	pool   *Pool
	mu     sync.Mutex
	boots  map[string][]string
}

func newRig(t *testing.T, policy Policy, tiers ...config.Tier) *rig {
	t.Helper()
	r := &rig{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), boots: map[string][]string{}}
	clock := func() time.Time { return r.now }
	r.ledger = &fakeLedger{now: clock, reports: map[string]alloc.TierCapacity{}}
	r.first = &fakeSource{t: t, ledger: r.ledger, scope: "first:us-west-2", shapes: []string{"z1d.metal", "m6id.metal"},
		idempotent: 10 * time.Minute}
	r.second = &fakeSource{t: t, ledger: r.ledger, scope: "second:acct", shapes: []string{"s2.c2.small"},
		idempotent: 10 * time.Minute}
	if policy.Choices == nil {
		policy.Choices = []Choice{{Source: "first", Shape: "z1d.metal"}, {Source: "second", Shape: "s2.c2.small", Location: "PHX"}}
	}
	n := 0
	p, err := New(r.ledger, Config{
		Archs:          map[config.Arch]Policy{config.ArchX64: policy},
		Sources:        map[string]hostsource.Source{"first": r.first, "second": r.second},
		RefusalBackoff: 10 * time.Minute,
		Tiers:          tiers,
		Boot: func(order, token string) ([]byte, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.boots[order] = append(r.boots[order], token)

			return []byte("join " + token), nil
		},
	}, WithClock(clock), WithIDs(func() (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		n++

		return fmt.Sprintf("o-%d", n), nil
	}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r.pool = p

	return r
}

func firecrackerTier(label string) config.Tier {
	return config.Tier{Label: label, Provider: config.ProviderFirecracker, Arch: config.ArchX64}
}

// report sets a tier's capacity report as the listener writes it, observed age ago:
// short is whether more hosts would have bought its waiting work.
func (r *rig) report(label string, short bool, age time.Duration, observationErr string) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.reports[label] = alloc.TierCapacity{
		ObservedAt:       r.now.Add(-age).UTC().Format(time.RFC3339Nano),
		ObservationError: observationErr,
		Listener:         alloc.ListenerCapacity{HostShortage: short},
	}
}

// reportWaiting sets a report of work waiting that more hosts would not have bought: a
// tier at its own cap, or behind a seal.
func (r *rig) reportWaiting(label string, waiting int) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	r.ledger.reports[label] = alloc.TierCapacity{
		ObservedAt: r.now.UTC().Format(time.RFC3339Nano),
		Listener:   alloc.ListenerCapacity{Waiting: waiting, Refusal: "the tier holds its max_concurrent of 2"},
	}
}

func step(t *testing.T, r *rig) {
	t.Helper()
	if err := r.pool.Step(t.Context()); err != nil {
		t.Fatalf("step: %v", err)
	}
}

// THE FLOOR IS KEPT: an order is committed with its source's scope, sent with its own
// single-use join token in its boot, and placed under the host the source named; a kept
// floor orders nothing more.
func TestTheFloorIsKept(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 2, Max: 4})
	step(t, r)
	step(t, r)
	step(t, r)
	orders := r.ledger.snapshot()
	if len(orders) != 2 {
		t.Fatalf("orders = %+v, want two", orders)
	}
	for _, id := range []string{"o-1", "o-2"} {
		o := orders[id]
		if o.State != alloc.PoolPlaced || o.HostID != "host-"+id || o.Scope != "first:us-west-2" {
			t.Errorf("%s = %+v", id, o)
		}
	}
	asked := r.first.requests()
	if len(asked) != 2 || asked[0].Shape != "z1d.metal" || asked[0].Arch != config.ArchX64 {
		t.Fatalf("asked %+v", asked)
	}
	if string(asked[0].Boot.Reveal()) != "join "+r.boots["o-1"][0] || r.boots["o-1"][0] == r.boots["o-2"][0] {
		t.Fatalf("boots = %v, payload %q", r.boots, asked[0].Boot.Reveal())
	}
	if len(r.second.requests()) != 0 {
		t.Fatal("the second choice was asked while the first answered")
	}
}

// A FIRECRACKER TIER SHORT OF HOSTS, BY A FRESH REPORT, IS DEMAND, for one host while
// none is on its way; another provider's tier, one with no architecture, or one whose
// waiting work more hosts would not buy (its own cap), is not. No host is known to join
// until the join lands, so demand grows the pool by one at most.
func TestDemandOrdersOneHostWhileNoneIsOnItsWay(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 0, Max: 3},
		firecrackerTier("fc-x64"),
		firecrackerTier("fc-capped"),
		config.Tier{Label: "ec2", Provider: config.ProviderEC2, Arch: config.ArchX64},
		config.Tier{Label: "fc-any", Provider: config.ProviderFirecracker})
	r.report("fc-x64", false, 0, "")
	r.reportWaiting("fc-capped", 5)
	r.report("ec2", true, 0, "")
	r.report("fc-any", true, 0, "")
	step(t, r)
	if n := len(r.ledger.states()); n != 0 {
		t.Fatalf("%d orders for demand the pool does not serve", n)
	}
	r.report("fc-x64", true, 0, "")
	step(t, r)
	step(t, r)
	step(t, r)
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolPlaced {
		t.Fatalf("orders = %v, want one placed and no more while it has not joined", got)
	}
}

// A TIER THAT NAMES FIRECRACKER AMONG SEVERAL PROVIDERS IS THE POOL'S DEMAND TOO.
func TestATierOfSeveralProvidersIncludingFirecrackerIsDemand(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 0, Max: 2}, config.Tier{
		Label: "mixed", Providers: []config.ProviderKind{config.ProviderEC2, config.ProviderFirecracker}, Arch: config.ArchX64,
	})
	r.report("mixed", true, 0, "")
	step(t, r)
	if n := len(r.ledger.states()); n != 1 {
		t.Fatalf("%d orders, want 1", n)
	}
}

// THE MAXIMUM HOLDS whatever the floor or the demand.
func TestTheMaximumHolds(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 3, Max: 2})
	for range 4 {
		step(t, r)
	}
	if n := len(r.ledger.states()); n != 2 {
		t.Fatalf("%d orders past the maximum of 2", n)
	}
}

// DEMAND THAT CANNOT BE READ IS NOT NO DEMAND, AND NOT DEMAND: a missing, stale or
// failed report, or a ledger that cannot be read, is reported, the floor is still kept,
// and nothing is ordered for demand.
func TestUnreadableDemandKeepsTheFloorAndOrdersNothingForDemand(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*rig){
		"no report":       func(*rig) {},
		"a stale report":  func(r *rig) { r.report("fc", true, 6*time.Minute, "") },
		"a failed report": func(r *rig) { r.report("fc", true, 0, "malformed snapshot") },
		"an unreadable ledger": func(r *rig) {
			r.report("fc", true, 0, "")
			r.ledger.readErr = errors.New("database is locked")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, Policy{Floor: 0, Max: 3}, firecrackerTier("fc"))
			set(r)
			if err := r.pool.Step(t.Context()); !errors.Is(err, ErrDemandUnknown) {
				t.Fatalf("step = %v, want the demand unknown", err)
			}
			if n := len(r.ledger.states()); n != 0 {
				t.Fatalf("%d orders for demand that could not be read", n)
			}

			r = newRig(t, Policy{Floor: 1, Max: 3}, firecrackerTier("fc"))
			set(r)
			if err := r.pool.Step(t.Context()); !errors.Is(err, ErrDemandUnknown) {
				t.Fatalf("step = %v", err)
			}
			if n := len(r.ledger.states()); n != 1 {
				t.Fatalf("%d orders, want the floor's one", n)
			}
		})
	}
}

// A REFUSAL IS AN ANSWER on the first attempt: the order is refused, the next choice is
// asked, and the refused one is passed over until the backoff passes. An order that
// never reached the source is the same.
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

// A REFUSAL PASSES OVER ONLY ITS OWN CHOICE: another shape at the same source is asked.
func TestARefusalPassesOverOnlyItsChoice(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 1, Choices: []Choice{
		{Source: "first", Shape: "z1d.metal"}, {Source: "first", Shape: "m6id.metal"},
	}})
	r.first.answer = func(req hostsource.Request) (hostsource.Host, error) {
		if req.Shape == "z1d.metal" {
			return hostsource.Host{}, hostsource.ErrNoStock
		}

		return hostsource.Host{ID: "host-" + req.Order}, nil
	}
	step(t, r)
	step(t, r)
	if got := r.ledger.snapshot(); got["o-2"].Shape != "m6id.metal" || got["o-2"].State != alloc.PoolPlaced {
		t.Fatalf("orders = %+v", got)
	}
}

// NOTHING IS ORDERED WHILE AN ORDER'S OUTCOME IS UNKNOWN, floor or no floor: an order
// still being ordered, or uncertain, may hold a host already.
func TestNothingIsOrderedWhileAnOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 2, Max: 3})
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	r.first.find = func(string) (hostsource.Host, bool, error) { return hostsource.Host{}, false, errors.New("timeout") }
	step(t, r)
	for range 3 {
		if err := r.pool.Step(t.Context()); err == nil {
			t.Fatal("a failed read back was not reported")
		}
	}
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolOrdering {
		t.Fatalf("orders while one is unknown: %v", got)
	}
	r.first.find = nil
	r.now = r.now.Add(11 * time.Minute)
	step(t, r)
	step(t, r)
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolUncertain {
		t.Fatalf("orders while one is uncertain: %v", got)
	}
}

// AN ANSWER THAT IS NOT KNOWN leaves the order being ordered, and the next step reads
// it back before anything else: found, it is placed under the found host; listed as
// absent inside the source's window, it is sent again under the same order with a fresh
// token; past the window, it is uncertain.
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
	if got := r.ledger.snapshot(); len(got) != 1 || got["o-1"].State != alloc.PoolPlaced || got["o-1"].HostID != "found-o-1" {
		t.Fatalf("found: %+v", got)
	}
	if finds := r.first.finds(); len(finds) != 1 || finds[0] != "o-1" {
		t.Fatalf("read back %v, want o-1", finds)
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
	if tokens := r.boots["o-1"]; len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("a resend's token: %v", tokens)
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
	if err := r.pool.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "read back order o-1") {
		t.Fatalf("a failed read back answered %v", err)
	}
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolOrdering || len(r.first.requests()) != 1 {
		t.Fatalf("a failed read back decided something: %v, asked %d times", got, len(r.first.requests()))
	}
}

// A RESEND THE SOURCE REFUSES SAYS NOTHING ABOUT THE FIRST ATTEMPT, which may have
// placed a host: the order is uncertain, not refused, and still counts.
func TestARefusedResendIsUncertainNotRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	step(t, r)
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, hostsource.ErrRefused }
	step(t, r)
	got := r.ledger.snapshot()
	if len(got) != 1 || got["o-1"].State != alloc.PoolUncertain || !strings.Contains(got["o-1"].Reason, "first attempt") {
		t.Fatalf("orders = %+v", got)
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

// AN ORDER IS READ BACK ONLY WHERE IT WAS PLACED: a source whose scope changed since
// is neither asked for it nor sent it again; the order is uncertain, for a person to
// look where it was placed, and nothing more is ordered on its architecture meanwhile.
func TestAnOrderIsReadBackOnlyWhereItWasPlaced(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	step(t, r)
	r.first.mu.Lock()
	r.first.scope = "first:eu-west-1"
	r.first.answer = nil
	r.first.mu.Unlock()
	step(t, r)
	step(t, r)
	got := r.ledger.snapshot()
	if len(got) != 1 || got["o-1"].State != alloc.PoolUncertain ||
		!strings.Contains(got["o-1"].Reason, "first:us-west-2") || !strings.Contains(got["o-1"].Reason, "first:eu-west-1") {
		t.Fatalf("orders = %+v, want o-1 uncertain naming both scopes", got)
	}
	if len(r.first.finds()) != 0 || len(r.first.requests()) != 1 {
		t.Fatalf("read back %v and sent %d times somewhere else", r.first.finds(), len(r.first.requests()))
	}
}

// A SOURCE THAT CANNOT SAY WHERE IT ORDERS ORDERS NOTHING and resolves nothing: the step
// reports it, no order is committed, and a lost order is neither read back nor sent.
func TestASourceThatCannotSayWhereItOrdersOrdersNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 2})
	r.first.scopeErr = errors.New("sts: throttled")
	if err := r.pool.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("step = %v, want the scope's error", err)
	}
	if n := len(r.ledger.states()); n != 0 {
		t.Fatalf("%d orders committed with no scope", n)
	}

	r = newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	step(t, r)
	r.first.mu.Lock()
	r.first.scopeErr = errors.New("sts: throttled")
	r.first.answer = nil
	r.first.mu.Unlock()
	if err := r.pool.Step(t.Context()); err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("a resume whose source could not say where it orders answered %v", err)
	}
	if got := r.ledger.states(); len(got) != 1 || got["o-1"] != alloc.PoolOrdering ||
		len(r.first.finds()) != 0 || len(r.first.requests()) != 1 {
		t.Fatalf("orders = %v, read back %v, sent %d times", got, r.first.finds(), len(r.first.requests()))
	}
}

// AN ORDER WHOSE SOURCE IS NO LONGER CONFIGURED IS UNCERTAIN: nothing can read it back,
// so a person must, and its architecture orders nothing meanwhile.
func TestAnOrderWhoseSourceIsGoneIsUncertain(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 2})
	r.first.answer = func(hostsource.Request) (hostsource.Host, error) { return hostsource.Host{}, errors.New("timeout") }
	step(t, r)
	p, err := New(r.ledger, Config{
		Archs:   map[config.Arch]Policy{config.ArchX64: {Floor: 1, Max: 2, Choices: []Choice{{Source: "second", Shape: "s2.c2.small"}}}},
		Sources: map[string]hostsource.Source{"second": r.second},
		Boot:    func(string, string) ([]byte, error) { return []byte("join"), nil },
	}, WithClock(func() time.Time { return r.now }))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for range 2 {
		if err := p.Step(t.Context()); err != nil {
			t.Fatalf("step: %v", err)
		}
	}
	got := r.ledger.snapshot()
	if len(got) != 1 || got["o-1"].State != alloc.PoolUncertain || !strings.Contains(got["o-1"].Reason, "first is no longer configured") {
		t.Fatalf("orders = %+v, want o-1 uncertain naming its source", got)
	}
	if n := len(r.second.requests()); n != 0 {
		t.Fatalf("another source was asked %d times while an order was unknown", n)
	}
}

// A FIRST ATTEMPT THAT FAILS BEFORE ITS SOURCE IS ASKED SENT NOTHING: a token that could
// not be minted, or a boot that could not be rendered, refuses the order rather than
// leaving it for a read back that could only end uncertain.
func TestAFirstAttemptThatCouldNotBeSentIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t, Policy{Floor: 1, Max: 1})
	r.ledger.tokenErr = errors.New("database is locked")
	if err := r.pool.Step(t.Context()); err == nil {
		t.Fatal("a token that could not be minted was not reported")
	}
	if got := r.ledger.snapshot(); got["o-1"].State != alloc.PoolRefused || !strings.HasPrefix(got["o-1"].Reason, "not sent") ||
		len(r.first.requests()) != 0 {
		t.Fatalf("orders = %+v, asked %d", got, len(r.first.requests()))
	}
}

func TestAChoiceNamingNoSourceIsRefused(t *testing.T) {
	t.Parallel()
	_, err := New(&fakeLedger{}, Config{
		Archs:   map[config.Arch]Policy{config.ArchX64: {Max: 1, Choices: []Choice{{Source: "missing", Shape: "x"}}}},
		Sources: map[string]hostsource.Source{},
		Boot:    func(string, string) ([]byte, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a choice naming no configured source answered %v", err)
	}
}

// RUN STEPS AT EVERY TICK until its context ends, and a failed step does not stop it.
func TestRunStepsAtEveryTickUntilItsContextEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, Policy{Floor: 0, Max: 1})
		r.ledger.readErr = errors.New("unreadable") // every step fails on demand
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			r.pool.Run(ctx, time.Minute)
			close(done)
		}()
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		cancel()
		<-done
		r.ledger.mu.Lock()
		calls := r.ledger.listCalls
		r.ledger.mu.Unlock()
		// Each step reads the orders twice; four steps: at start and at 1, 2 and 3 min.
		if calls != 8 {
			t.Fatalf("the ledger was read %d times, want 8 (four steps)", calls)
		}
	})
}
