// Package pool orders Firecracker hosts from rented bare metal ahead of demand
// (ADR-030). The control plane runs it, because it holds the demand.
//
// AN ORDER IS COMMITTED BEFORE ITS SOURCE IS ASKED, with the scope its source orders
// in, and read back with Find before anything else is done about it, so an order whose
// answer was lost is never placed twice, nor read back somewhere a changed config
// points. A resend happens only inside the source's own deduplication window from the
// order's first attempt; past it, the order is uncertain and a person decides. A resend
// the source refuses proves nothing about the first attempt, so it is uncertain too.
//
// NOTHING IS ORDERED ON AN ARCHITECTURE WHILE ANY ORDER THERE IS UNKNOWN (being
// ordered, or uncertain): it may hold a host the floor or the demand already counts.
// Growth for demand is one host at a time while any placed host has not joined, because
// a host takes minutes to join. The floor is kept otherwise.
//
// A HOST JOINS FROM ITS ORDER (ADR-030): every send mints a token bound to the order,
// the admission links the order to the node that joined from it, and an order whose
// tokens all lapsed with nothing joined is held for a person, as is one whose join the
// source contradicted. A held order still holds its host and is no longer on its way.
//
// SHRINKING IS NOT HERE. A host leaves only by drain, on proof it holds nothing.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

// DefaultTokenLife is how long an ordered host's join token lives: past any measured
// lead time, short enough that a token nobody used is soon worth nothing.
const DefaultTokenLife = 2 * time.Hour

// DefaultDemandFreshness is the oldest a tier's capacity report may be and still be
// read as its demand: a report a listener has not refreshed is a previous controller's,
// or a listener that stopped, and says nothing about now.
const DefaultDemandFreshness = 5 * time.Minute

// ErrDemandUnknown is demand that could not be read: a tier with no fresh report.
var ErrDemandUnknown = errors.New("pool: a Firecracker tier's demand cannot be read")

// Ledger is what the pool reads and writes of the ledger.
type Ledger interface {
	RecordPoolOrder(ctx context.Context, o alloc.PoolOrder) error
	PoolOrders(ctx context.Context) ([]alloc.PoolOrder, error)
	PoolOrderPlaced(ctx context.Context, id, hostID string) error
	PoolOrderUncertain(ctx context.Context, id, reason string) error
	UncertainPoolOrderFound(ctx context.Context, id, hostID string) error
	PoolOrderRefused(ctx context.Context, id, reason string) error
	NewPoolJoinToken(ctx context.Context, order string, ttl time.Duration) (string, error)
	HoldLapsedPoolJoins(ctx context.Context) (int, error)
	CapacityReports(ctx context.Context) (map[string]alloc.TierCapacity, error)
}

// Choice is one place a host may be ordered from.
type Choice struct {
	Source   string
	Shape    string
	Location string
}

// Policy is one architecture's part of the pool.
type Policy struct {
	Floor   int
	Max     int
	Choices []Choice
}

// Config is what the pool is told.
type Config struct {
	Archs          map[config.Arch]Policy
	Sources        map[string]hostsource.Source
	RefusalBackoff time.Duration
	TokenLife      time.Duration
	// DemandFreshness is the oldest a capacity report may be and still count.
	DemandFreshness time.Duration
	// Boot renders an ordered host's first boot from its order and join token.
	Boot func(order, token string) ([]byte, error)
	// Tiers are the deployment's tiers; a Firecracker tier short of hosts is
	// demand for its architecture.
	Tiers []config.Tier
}

// Pool is the scaler.
type Pool struct {
	ledger Ledger
	cfg    Config
	now    func() time.Time
	newID  func() (string, error)
	log    *slog.Logger
}

// Option configures a Pool.
type Option func(*Pool)

// WithClock replaces the clock, for a test.
func WithClock(now func() time.Time) Option { return func(p *Pool) { p.now = now } }

// WithIDs replaces how order identifiers are drawn, for a test.
func WithIDs(next func() (string, error)) Option { return func(p *Pool) { p.newID = next } }

// WithLogger replaces the logger.
func WithLogger(log *slog.Logger) Option { return func(p *Pool) { p.log = log } }

// New builds a pool. Every choice must name a source it is given.
func New(ledger Ledger, cfg Config, opts ...Option) (*Pool, error) {
	if cfg.Boot == nil {
		return nil, errors.New("pool: a pool needs a boot payload for the hosts it orders")
	}
	for arch, a := range cfg.Archs {
		for _, c := range a.Choices {
			if cfg.Sources[c.Source] == nil {
				return nil, fmt.Errorf("pool: %s orders from %s, which is not configured", arch, c.Source)
			}
		}
	}
	if cfg.TokenLife <= 0 {
		cfg.TokenLife = DefaultTokenLife
	}
	if cfg.DemandFreshness <= 0 {
		cfg.DemandFreshness = DefaultDemandFreshness
	}
	p := &Pool{ledger: ledger, cfg: cfg, now: time.Now, newID: newOrderID, log: slog.Default()}
	for _, o := range opts {
		o(p)
	}

	return p, nil
}

// Run steps the pool every interval until ctx ends. A step that fails is logged and
// the next one starts from the ledger again.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := p.Step(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("the pool's step failed; the next one reads the ledger again", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step reads every order back, holds for a person every placed order nothing can join
// from any more, then orders what the floor and the demand call for.
func (p *Pool) Step(ctx context.Context) error {
	var errs []error
	// A HOST WHOSE TOKENS ALL LAPSED CAN NEVER JOIN, and without a hold it would count
	// on its way forever and stop growth for demand with nothing to say why.
	if held, err := p.ledger.HoldLapsedPoolJoins(ctx); err != nil {
		errs = append(errs, err)
	} else if held > 0 {
		p.log.Warn("pool orders whose hosts can no longer join are held for a person", "held", held)
	}
	orders, err := p.ledger.PoolOrders(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for i := range orders {
		var err error
		switch o := orders[i]; o.State {
		case alloc.PoolOrdering:
			err = p.resume(ctx, o)
		case alloc.PoolUncertain:
			err = p.findUncertain(ctx, o)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if orders, err = p.ledger.PoolOrders(ctx); err != nil {
		return errors.Join(append(errs, err)...)
	}
	demand, err := p.demand(ctx)
	if err != nil {
		// COULD-NOT-TELL IS NOT NO DEMAND, and it is not demand either: the floor is
		// still kept, and growth for demand waits for a reading.
		errs = append(errs, err)
	}
	for arch, a := range p.cfg.Archs {
		c := count(orders, arch)
		if c.unknown > 0 {
			continue
		}
		want := a.Floor
		if demand[arch] && c.onItsWay == 0 {
			want = c.active + 1
		}
		if c.active >= a.Max || c.active >= want {
			continue
		}
		if err := p.order(ctx, arch, a, orders); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// counts are an architecture's orders that hold or may hold a host (active), those
// whose outcome is not known (unknown), and those placed, not yet joined and not held
// for a person (onItsWay).
type counts struct{ active, unknown, onItsWay int }

func count(orders []alloc.PoolOrder, arch config.Arch) counts {
	var c counts
	for i := range orders {
		o := &orders[i]
		if o.Arch != arch {
			continue
		}
		switch o.State {
		case alloc.PoolOrdering, alloc.PoolUncertain:
			c.active++
			c.unknown++
		case alloc.PoolPlaced:
			c.active++
			// A HELD ORDER IS NOT ON ITS WAY: its host joins only once a person acts, and
			// counting it would stop growth for demand until then.
			if o.NodeName == "" && o.JoinHold == "" {
				c.onItsWay++
			}
		}
	}

	return c
}

// demand is the architectures a Firecracker tier is short of hosts on (work it could
// not buy for, and only hosts to blame), by a report
// fresh enough to say so. A Firecracker tier whose report is missing, stale or in error
// makes the whole reading unknown.
func (p *Pool) demand(ctx context.Context) (map[config.Arch]bool, error) {
	reports, err := p.ledger.CapacityReports(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDemandUnknown, err)
	}
	out := map[config.Arch]bool{}
	var unknown []string
	for i := range p.cfg.Tiers {
		t := &p.cfg.Tiers[i]
		// A TIER PINNED TO ONE NODE IS NO POOL'S DEMAND: no host the pool orders is that
		// node, so growing for it would order hosts its work can never run on, up to the
		// maximum, while it waits as before.
		if !servesFirecracker(t) || t.Arch == "" || t.Node != "" {
			continue
		}
		r, ok := reports[t.Label]
		observed, parseErr := time.Parse(time.RFC3339Nano, r.ObservedAt)
		switch {
		case !ok, r.ObservationError != "", parseErr != nil, p.now().Sub(observed) > p.cfg.DemandFreshness:
			unknown = append(unknown, t.Label)

			continue
		}
		if r.Listener.HostShortage {
			out[t.Arch] = true
		}
	}
	if len(unknown) > 0 {
		return out, fmt.Errorf("%w: no fresh report for %v", ErrDemandUnknown, unknown)
	}

	return out, nil
}

func servesFirecracker(t *config.Tier) bool {
	if t.Provider == config.ProviderFirecracker {
		return true
	}
	for _, k := range t.Providers {
		if k == config.ProviderFirecracker {
			return true
		}
	}

	return false
}

// order places one order for an architecture, from its first choice not passed over
// for a recent refusal.
func (p *Pool) order(ctx context.Context, arch config.Arch, a Policy, orders []alloc.PoolOrder) error {
	choice, ok := p.firstChoice(a, orders)
	if !ok {
		p.log.Warn("every choice for this architecture was refused recently; ordering nothing until the backoff passes",
			"arch", arch, "backoff", p.cfg.RefusalBackoff)

		return nil
	}
	scope, err := p.cfg.Sources[choice.Source].Scope(ctx)
	if err != nil {
		return fmt.Errorf("pool: where %s orders: %w", choice.Source, err)
	}
	id, err := p.newID()
	if err != nil {
		return err
	}
	o := alloc.PoolOrder{
		ID: id, Source: choice.Source, Scope: scope,
		Shape: choice.Shape, Location: choice.Location, Arch: arch,
	}
	if err := p.ledger.RecordPoolOrder(ctx, o); err != nil {
		return err
	}
	p.log.Info("ordering a pool host", "order", id, "arch", arch, "source", choice.Source, "shape", choice.Shape)

	return p.send(ctx, o, true)
}

// firstChoice skips a choice whose source refused it inside the backoff.
func (p *Pool) firstChoice(a Policy, orders []alloc.PoolOrder) (Choice, bool) {
	for _, c := range a.Choices {
		refusedRecently := false
		for i := range orders {
			o := &orders[i]
			if o.State == alloc.PoolRefused && o.Source == c.Source && o.Shape == c.Shape &&
				o.Location == c.Location && p.now().Sub(o.Updated) < p.cfg.RefusalBackoff {
				refusedRecently = true

				break
			}
		}
		if !refusedRecently {
			return c, true
		}
	}

	return Choice{}, false
}

// send asks the source for an order already committed, with a fresh join token, and
// records the answer. On the first attempt, a refusal, or a failure before anything
// was sent, is the order's answer. On a resend it is not: it says nothing about the
// first attempt, which may have placed a host, so the order is uncertain. Any other
// answer leaves the order being ordered, for the next step to read back.
func (p *Pool) send(ctx context.Context, o alloc.PoolOrder, first bool) error {
	notSent := func(err error) error {
		if !first {
			return err
		}
		if rerr := p.ledger.PoolOrderRefused(ctx, o.ID, "not sent: "+err.Error()); rerr != nil {
			return errors.Join(err, rerr)
		}

		return err
	}
	token, err := p.ledger.NewPoolJoinToken(ctx, o.ID, p.cfg.TokenLife)
	if err != nil {
		return notSent(fmt.Errorf("pool: mint the join token of order %s: %w", o.ID, err))
	}
	boot, err := p.cfg.Boot(o.ID, token)
	if err != nil {
		return notSent(fmt.Errorf("pool: render the boot of order %s: %w", o.ID, err))
	}
	host, err := p.cfg.Sources[o.Source].Order(ctx, hostsource.Request{
		Order: o.ID, Shape: o.Shape, Location: o.Location, Arch: o.Arch, Boot: hostsource.NewSecret(boot),
	})
	switch {
	case err == nil:
		return p.ledger.PoolOrderPlaced(ctx, o.ID, host.ID)
	case errors.Is(err, hostsource.ErrNoStock), errors.Is(err, hostsource.ErrQuota), errors.Is(err, hostsource.ErrRefused),
		errors.Is(err, hostsource.ErrNotSent):
		if !first {
			p.log.Warn("a resend was refused, and the first attempt may have placed a host; the order is uncertain",
				"order", o.ID, "err", err)

			return p.ledger.PoolOrderUncertain(ctx, o.ID,
				"a resend was refused, and the first attempt may have placed a host: "+err.Error())
		}
		p.log.Warn("the source refused a pool order", "order", o.ID, "source", o.Source, "err", err)

		return p.ledger.PoolOrderRefused(ctx, o.ID, err.Error())
	}
	p.log.Warn("a pool order's answer is not known; the next step reads it back", "order", o.ID, "err", err)

	return nil
}

// resume reads an order back where it was placed. Found, it is placed. Listed as
// absent, it is sent again only inside the source's deduplication window from its
// first attempt; past it, it is uncertain. A failed read, or a source whose scope has
// changed since the order, decides nothing.
func (p *Pool) resume(ctx context.Context, o alloc.PoolOrder) error {
	src := p.cfg.Sources[o.Source]
	if src == nil {
		return p.ledger.PoolOrderUncertain(ctx, o.ID, fmt.Sprintf(
			"its source %s is no longer configured, so it cannot be read back", o.Source))
	}
	scope, err := src.Scope(ctx)
	if err != nil {
		return fmt.Errorf("pool: where order %s's source orders now: %w", o.ID, err)
	}
	if scope != o.Scope {
		// UNCERTAIN, NOT WAITING: a read anywhere else could miss the host it placed,
		// and a person can look where it was placed and close it.
		return p.ledger.PoolOrderUncertain(ctx, o.ID, fmt.Sprintf(
			"it was placed in %s and its source now orders in %s, so it cannot be read back", o.Scope, scope))
	}
	host, found, err := src.Find(ctx, o.ID)
	if err != nil {
		return fmt.Errorf("pool: read back order %s: %w", o.ID, err)
	}
	if found {
		return p.ledger.PoolOrderPlaced(ctx, o.ID, host.ID)
	}
	terms, err := src.Terms(o.Shape)
	if err != nil {
		// A SHAPE ITS SOURCE NO LONGER DECLARES CANNOT BE SENT AGAIN, so the order is
		// uncertain, still read back where it was placed, rather than ordering for good
		// and holding its architecture with it.
		return p.ledger.PoolOrderUncertain(ctx, o.ID, fmt.Sprintf(
			"%s lists no host for this order, and its shape %s cannot be sent again: %v", o.Source, o.Shape, err))
	}
	if terms.IdempotentFor > 0 && p.now().Sub(o.FirstAttempt) < terms.IdempotentFor {
		return p.send(ctx, o, false)
	}

	return p.ledger.PoolOrderUncertain(ctx, o.ID, fmt.Sprintf(
		"%s lists no host for this order, and a resend %v after the first attempt could place a second one",
		o.Source, p.now().Sub(o.FirstAttempt).Round(time.Second)))
}

// findUncertain reads an uncertain order back where it was placed, and only there:
// a host its source now lists is the order's, placed and never sent again, so the
// host whose answer was lost can join. Anywhere else, with its source gone, or listed
// as absent, the order waits for a person as before.
func (p *Pool) findUncertain(ctx context.Context, o alloc.PoolOrder) error {
	src := p.cfg.Sources[o.Source]
	if src == nil {
		return nil
	}
	scope, err := src.Scope(ctx)
	if err != nil {
		return fmt.Errorf("pool: where uncertain order %s's source orders now: %w", o.ID, err)
	}
	if scope != o.Scope {
		return nil
	}
	host, found, err := src.Find(ctx, o.ID)
	if err != nil {
		return fmt.Errorf("pool: read back uncertain order %s: %w", o.ID, err)
	}
	if !found {
		return nil
	}
	p.log.Info("an uncertain pool order's host is listed where it was placed; the order is placed",
		"order", o.ID, "host", host.ID)

	return p.ledger.UncertainPoolOrderFound(ctx, o.ID, host.ID)
}

func newOrderID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pool: draw an order id: %w", err)
	}

	return "p" + hex.EncodeToString(b), nil
}
