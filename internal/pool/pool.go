// Package pool orders Firecracker hosts from rented bare metal ahead of demand
// (ADR-028). The control plane runs it, because it holds the demand.
//
// AN ORDER IS COMMITTED BEFORE ITS SOURCE IS ASKED, and read back with Find before
// anything else is done about it, so an order whose answer was lost is never placed
// twice. A resend happens only inside the source's own deduplication window from the
// order's first attempt; past it, the order is uncertain and a person decides.
//
// GROWTH IS ONE HOST AT A TIME PER ARCHITECTURE while any is on its way: a host takes
// minutes to join, and ordering again for demand it has not yet met would order a
// host per step. The floor is kept regardless.
//
// SHRINKING IS NOT HERE. A host leaves only by drain, on proof it holds nothing
// (ADR-028), and that proof needs the host linked to the node that joined from it,
// which the join brings.
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

// Ledger is what the pool reads and writes of the ledger.
type Ledger interface {
	RecordPoolOrder(ctx context.Context, o alloc.PoolOrder) error
	PoolOrders(ctx context.Context) ([]alloc.PoolOrder, error)
	PoolOrderPlaced(ctx context.Context, id, hostID string) error
	PoolOrderUncertain(ctx context.Context, id, reason string) error
	PoolOrderRefused(ctx context.Context, id, reason string) error
	NewJoinToken(ctx context.Context, ttl time.Duration, uses int, note string) (string, error)
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
	// Boot renders an ordered host's first boot from its order and join token.
	Boot func(order, token string) ([]byte, error)
	// Tiers are the deployment's tiers; a Firecracker tier with jobs waiting is
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

// Step reads every order back, then orders what the floor and the demand call for.
func (p *Pool) Step(ctx context.Context) error {
	orders, err := p.ledger.PoolOrders(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range orders {
		if o := orders[i]; o.State == alloc.PoolOrdering {
			if err := p.resume(ctx, o); err != nil {
				errs = append(errs, err)
			}
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
		active, pending := count(orders, arch)
		want := a.Floor
		if demand[arch] && pending == 0 {
			want = active + 1
		}
		if active >= a.Max || active >= want {
			continue
		}
		if err := p.order(ctx, arch, a, orders); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// count is an architecture's orders that hold or may hold a host, and those of them
// that are on their way: ordered and not yet known to have joined.
func count(orders []alloc.PoolOrder, arch config.Arch) (int, int) {
	active, pending := 0, 0
	for i := range orders {
		o := &orders[i]
		if o.Arch != arch {
			continue
		}
		switch o.State {
		case alloc.PoolOrdering, alloc.PoolUncertain:
			active++
			pending++
		case alloc.PoolPlaced:
			active++
			if o.NodeName == "" {
				pending++
			}
		}
	}

	return active, pending
}

// demand is the architectures a Firecracker tier has jobs waiting on.
func (p *Pool) demand(ctx context.Context) (map[config.Arch]bool, error) {
	reports, err := p.ledger.CapacityReports(ctx)
	if err != nil {
		return nil, fmt.Errorf("pool: read the tiers' demand: %w", err)
	}
	out := map[config.Arch]bool{}
	for i := range p.cfg.Tiers {
		t := &p.cfg.Tiers[i]
		if !servesFirecracker(t) {
			continue
		}
		if r, ok := reports[t.Label]; ok && r.Listener.Waiting > 0 {
			out[t.Arch] = true
		}
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
	id, err := p.newID()
	if err != nil {
		return err
	}
	if err := p.ledger.RecordPoolOrder(ctx, alloc.PoolOrder{
		ID: id, Source: choice.Source, Shape: choice.Shape, Location: choice.Location, Arch: arch,
	}); err != nil {
		return err
	}
	p.log.Info("ordering a pool host", "order", id, "arch", arch, "source", choice.Source, "shape", choice.Shape)

	return p.send(ctx, alloc.PoolOrder{ID: id, Source: choice.Source, Shape: choice.Shape, Location: choice.Location, Arch: arch})
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
// records the answer. Any answer but a host or the source's refusal leaves the order
// being ordered, for the next step to read back.
func (p *Pool) send(ctx context.Context, o alloc.PoolOrder) error {
	src := p.cfg.Sources[o.Source]
	token, err := p.ledger.NewJoinToken(ctx, p.cfg.TokenLife, 1, "pool order "+o.ID)
	if err != nil {
		return err
	}
	boot, err := p.cfg.Boot(o.ID, token)
	if err != nil {
		return fmt.Errorf("pool: render the boot of order %s: %w", o.ID, err)
	}
	host, err := src.Order(ctx, hostsource.Request{
		Order: o.ID, Shape: o.Shape, Location: o.Location, Arch: o.Arch, Boot: hostsource.NewSecret(boot),
	})
	switch {
	case err == nil:
		return p.ledger.PoolOrderPlaced(ctx, o.ID, host.ID)
	case errors.Is(err, hostsource.ErrNoStock), errors.Is(err, hostsource.ErrQuota), errors.Is(err, hostsource.ErrRefused),
		errors.Is(err, hostsource.ErrNotSent):
		p.log.Warn("the source refused a pool order", "order", o.ID, "source", o.Source, "err", err)

		return p.ledger.PoolOrderRefused(ctx, o.ID, err.Error())
	}
	p.log.Warn("a pool order's answer is not known; the next step reads it back", "order", o.ID, "err", err)

	return nil
}

// resume reads an order back. Found, it is placed. Listed as absent, it is sent again
// only inside the source's deduplication window from its first attempt; past it, it
// is uncertain. A failed read decides nothing.
func (p *Pool) resume(ctx context.Context, o alloc.PoolOrder) error {
	src := p.cfg.Sources[o.Source]
	if src == nil {
		p.log.Warn("a pool order names a source no longer configured; it waits", "order", o.ID, "source", o.Source)

		return nil
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
		return fmt.Errorf("pool: the terms of order %s: %w", o.ID, err)
	}
	if terms.IdempotentFor > 0 && p.now().Sub(o.FirstAttempt) < terms.IdempotentFor {
		return p.send(ctx, o)
	}

	return p.ledger.PoolOrderUncertain(ctx, o.ID, fmt.Sprintf(
		"%s lists no host for this order, and a resend %v after the first attempt could place a second one",
		o.Source, p.now().Sub(o.FirstAttempt).Round(time.Second)))
}

func newOrderID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pool: draw an order id: %w", err)
	}

	return "p" + hex.EncodeToString(b), nil
}
