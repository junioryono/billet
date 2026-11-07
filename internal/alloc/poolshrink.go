package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// ErrPoolHostNotIdle is a pool host the scaler proposed for release that, read again in
// the release's own transaction, holds something, has a release asked for already, or
// was not idle long enough. Nothing was written.
var ErrPoolHostNotIdle = errors.New("alloc: the pool host is not idle")

// ErrPoolAtFloor is an automatic release that would leave its architecture fewer serving
// hosts than its floor, counted in the release's own transaction: a person's release
// committed since the scaler read the pool counts. Nothing was written.
var ErrPoolAtFloor = errors.New("alloc: the pool is at its floor")

// PoolIdleHost is a joined pool host that holds nothing, with when it last held
// something: the end of its newest job, or its node's admission if it never ran one.
type PoolIdleHost struct {
	Order string
	Node  string
	Arch  config.Arch
	Since time.Time
}

// PoolIdleHosts lists the joined pool hosts that hold nothing. A host whose idleness
// cannot be read (no admission recorded, a time this binary cannot parse) is left out:
// could-not-tell is never idle.
func (a *Allocator) PoolIdleHosts(ctx context.Context) ([]PoolIdleHost, error) {
	var out []PoolIdleHost

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListPoolIdleHosts(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list the idle pool hosts: %w", err)
		}

		out = out[:0]

		for _, r := range rows {
			since, ok := idleSince(r.LastFinished, r.Joined)
			if !ok {
				continue
			}

			out = append(out, PoolIdleHost{Order: r.OrderID, Node: r.NodeName, Arch: config.Arch(r.Arch), Since: since})
		}

		return nil
	})

	return out, err
}

// ReleaseIdlePoolHost asks for the release of a joined pool host on the pool's word, in
// one transaction that reads it again: it must still hold nothing, have no release asked
// for, and have held nothing since idleBefore, or it answers ErrPoolHostNotIdle; and its
// architecture must keep at least floor serving hosts without it, or it answers
// ErrPoolAtFloor. Either way nothing is written. The release then proves itself as any
// other does (ADR-030): a timer proposes a drain; only the proof releases.
func (a *Allocator) ReleaseIdlePoolHost(
	ctx context.Context, order string, idleBefore time.Time, floor int, reason string,
) error {
	if reason == "" {
		return fmt.Errorf("alloc: releasing idle pool order %s needs a reason", order)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		row, err := q.ReadPoolIdleHost(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: pool order %s holds something, is not placed and joined, or its release "+
				"was asked for", ErrPoolHostNotIdle, order)
		case err != nil:
			return fmt.Errorf("alloc: read whether pool order %s is idle: %w", order, err)
		}

		since, ok := idleSince(row.LastFinished, row.Joined)
		if !ok || since.After(idleBefore) {
			return fmt.Errorf("%w: pool order %s has not been idle since %s", ErrPoolHostNotIdle, order,
				idleBefore.UTC().Format(time.RFC3339))
		}

		serving, err := servingHosts(ctx, q)
		if err != nil {
			return err
		}
		// A HOST WHOSE NODE NEVER REGISTERED SERVES NOTHING, so its release leaves the
		// floor as it was.
		left := serving[config.Arch(row.Arch)]
		if row.Registered == 1 {
			left--
		}
		if left < floor {
			return fmt.Errorf("%w: releasing pool order %s would leave %s %d serving host(s), below its floor of %d",
				ErrPoolAtFloor, order, row.Arch, left, floor)
		}

		return requestJoinedReleaseTx(ctx, q, order, row.NodeName, reason, a.now().UTC())
	})
}

// PoolServingHosts counts each architecture's pool hosts that can take work: placed, a
// node joined from it and registered, no release asked for. A host whose node was
// admitted but never registered is not one, though its order names the node.
func (a *Allocator) PoolServingHosts(ctx context.Context) (map[config.Arch]int, error) {
	var out map[config.Arch]int

	err := a.db.View(ctx, func(q querier) error {
		var err error
		out, err = servingHosts(ctx, state.ReadQueries(q))

		return err
	})

	return out, err
}

func servingHosts(ctx context.Context, q state.ReadOps) (map[config.Arch]int, error) {
	rows, err := q.ListPoolServingHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: count the serving pool hosts: %w", err)
	}

	out := make(map[config.Arch]int, len(rows))
	for _, r := range rows {
		out[config.Arch(r.Arch)] = int(r.Hosts)
	}

	return out, nil
}

// idleSince is the later of a host's newest job's end and its node's admission, and
// whether either could be read. A value this binary cannot parse makes the answer
// unknown rather than older: an unreadable time is never read as idle.
func idleSince(lastFinished, joined string) (time.Time, bool) {
	var since time.Time

	for _, s := range []string{lastFinished, joined} {
		if s == "" {
			continue
		}

		t, err := time.Parse(timestampFormat, s)
		if err != nil {
			return time.Time{}, false
		}

		if t.After(since) {
			since = t
		}
	}

	return since, !since.IsZero()
}
