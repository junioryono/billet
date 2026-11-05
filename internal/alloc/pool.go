package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// Pool order states (migration 73). Every move is one way.
const (
	PoolOrdering  = "ordering"
	PoolPlaced    = "placed"
	PoolUncertain = "uncertain"
	PoolRefused   = "refused"
	PoolSealed    = "sealed"
	PoolReleased  = "released"
	PoolGone      = "gone"
	PoolAbandoned = "abandoned"
)

// ErrPoolOrderMoved is a move asked of an order that is not in the state it moves
// from: another process moved it first, or it never was.
var ErrPoolOrderMoved = errors.New("alloc: the pool order is not in the state that move starts from")

// PoolOrder is one order a pool of rented bare metal placed (ADR-028).
type PoolOrder struct {
	ID     string
	Source string
	// Scope is where the source orders, as it was when the order was placed; a lost
	// order is read back only there.
	Scope    string
	Shape    string
	Location string
	Arch     config.Arch
	State    string
	HostID   string
	NodeName string
	Reason   string
	// FirstAttempt bounds a resend: a source deduplicates an order only for a window
	// from it.
	FirstAttempt time.Time
	Updated      time.Time
}

// RecordPoolOrder commits an order before its source is asked, so an order whose
// answer is lost is read back under its identifier rather than placed twice.
func (a *Allocator) RecordPoolOrder(ctx context.Context, o PoolOrder) error {
	switch {
	case o.ID == "" || o.Source == "" || o.Scope == "" || o.Shape == "" || o.Arch == "":
		return fmt.Errorf("alloc: a pool order needs an id, a source, its scope, a shape and an architecture: %+v", o)
	case !o.Arch.Valid():
		return fmt.Errorf("alloc: pool order %s names architecture %q", o.ID, o.Arch)
	}
	now := ts(a.now())

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		return state.WriteQueries(tx).InsertPoolOrder(ctx, ledgerdb.InsertPoolOrderParams{
			OrderID: o.ID, Source: o.Source, Scope: o.Scope, Shape: o.Shape, Location: o.Location,
			Arch: string(o.Arch), FirstAttemptAt: now, UpdatedAt: now,
		})
	})
}

// PoolOrders reads every order, oldest first.
func (a *Allocator) PoolOrders(ctx context.Context) ([]PoolOrder, error) {
	var out []PoolOrder
	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListPoolOrders(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list pool orders: %w", err)
		}
		out = make([]PoolOrder, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			first, err := time.Parse(timestampFormat, r.FirstAttemptAt)
			if err != nil {
				return fmt.Errorf("alloc: pool order %s: first attempt %q: %w", r.OrderID, r.FirstAttemptAt, err)
			}
			updated, err := time.Parse(timestampFormat, r.UpdatedAt)
			if err != nil {
				return fmt.Errorf("alloc: pool order %s: updated %q: %w", r.OrderID, r.UpdatedAt, err)
			}
			out = append(out, PoolOrder{
				ID: r.OrderID, Source: r.Source, Scope: r.Scope, Shape: r.Shape, Location: r.Location,
				Arch: config.Arch(r.Arch), State: r.State, HostID: r.HostID, NodeName: r.NodeName,
				Reason: r.Reason, FirstAttempt: first, Updated: updated,
			})
		}

		return nil
	})

	return out, err
}

// PoolOrderPlaced records the host a source named for an order still being ordered.
func (a *Allocator) PoolOrderPlaced(ctx context.Context, id, hostID string) error {
	if hostID == "" {
		return fmt.Errorf("alloc: pool order %s: a placed order needs the host the source named", id)
	}

	return a.movePoolOrder(ctx, id, PoolOrdering, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderPlaced(ctx, ledgerdb.MarkPoolOrderPlacedParams{OrderID: id, HostID: hostID, UpdatedAt: now})
	})
}

// PoolOrderUncertain records that an order's answer was lost and its source can no
// longer deduplicate a resend; a person decides.
func (a *Allocator) PoolOrderUncertain(ctx context.Context, id, reason string) error {
	return a.movePoolOrder(ctx, id, PoolOrdering, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderUncertain(ctx, ledgerdb.MarkPoolOrderUncertainParams{OrderID: id, Reason: reason, UpdatedAt: now})
	})
}

// PoolOrderRefused records the source's refusal of an order being ordered.
func (a *Allocator) PoolOrderRefused(ctx context.Context, id, reason string) error {
	return a.movePoolOrder(ctx, id, PoolOrdering, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderRefused(ctx, ledgerdb.MarkPoolOrderRefusedParams{OrderID: id, Reason: reason, UpdatedAt: now})
	})
}

// PoolOrderAbandoned closes an uncertain order on a person's word, after they have
// looked at the source themselves.
func (a *Allocator) PoolOrderAbandoned(ctx context.Context, id, reason string) error {
	if reason == "" {
		return fmt.Errorf("alloc: pool order %s: abandoning an order needs the reason a person gave", id)
	}

	return a.movePoolOrder(ctx, id, PoolUncertain, func(w state.WriteOps, now string) (int64, error) {
		return w.MarkPoolOrderAbandoned(ctx, ledgerdb.MarkPoolOrderAbandonedParams{OrderID: id, Reason: reason, UpdatedAt: now})
	})
}

func (a *Allocator) movePoolOrder(
	ctx context.Context, id, from string, move func(state.WriteOps, string) (int64, error),
) error {
	now := ts(a.now())

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := move(state.WriteQueries(tx), now)
		if err != nil {
			return fmt.Errorf("alloc: move pool order %s: %w", id, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: %s is not %s", ErrPoolOrderMoved, id, from)
		}

		return nil
	})
}
