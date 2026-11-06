package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrPoolOrderClosed is a join asked of a pool order that admits no node: it is not
// placed (sealed, released, refused or never existed), or a node has already joined
// from it under another key.
var ErrPoolOrderClosed = errors.New("alloc: this pool order admits no node")

// ErrPoolOrderNotPlaced is a join from an order whose host the source has not named
// yet: its answer is still being read back, so the host is told to ask again.
var ErrPoolOrderNotPlaced = errors.New("alloc: this pool order's host is not yet recorded as placed")

// ErrPoolJoinHeld is a join from an order a person has to look at first (ADR-030).
var ErrPoolJoinHeld = errors.New("alloc: this pool order's join is held for a person")

// PoolNodeName is the name a node that joins from a pool order takes. It is the one
// place the name is derived: the boot that orders the host and the admission that
// names its node both read it here, so a request naming anything else is refused.
func PoolNodeName(order string) string { return poolNodePrefix + order }

// poolNodePrefix begins every pool node's name, which only a pool's own token enrolls.
const poolNodePrefix = "pool-"

// IsPoolNodeName reports whether a name is one a pool order gives its node, which an
// operator's token may not claim.
func IsPoolNodeName(name string) bool { return strings.HasPrefix(name, poolNodePrefix) }

// NewPoolJoinToken mints one single-use join token for an order being ordered or
// placed. Every send of an order mints one, and any live one admits the order's host:
// a resend under the source's idempotency key returns the host the first send booted.
func (a *Allocator) NewPoolJoinToken(ctx context.Context, order string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", errors.New("alloc: a join token needs a positive lifetime")
	}

	token, err := newJoinTokenSecret()
	if err != nil {
		return "", err
	}

	now := a.now().UTC()

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		row, err := q.ReadPoolOrder(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", order, err)
		case row.State != PoolOrdering && row.State != PoolPlaced:
			return fmt.Errorf("%w: pool order %s is %s", ErrPoolOrderClosed, order, row.State)
		}

		return insertJoinTokenTx(ctx, q, token, "pool order "+order, order, 1, now, ttl)
	})

	return token, err
}

// PoolJoinOrder is the pool order a join token was minted for, spent or not, and
// empty for an operator's token or for no token at all: a host keeps polling with the
// token it booted with after its admission spent it. live is whether the token could
// still be spent now.
func (a *Allocator) PoolJoinOrder(ctx context.Context, token string) (string, bool, error) {
	var row ledgerdb.ReadJoinTokenOrderRow

	err := a.db.View(ctx, func(q querier) error {
		var err error

		row, err = state.ReadQueries(q).ReadJoinTokenOrder(ctx, ledgerdb.ReadJoinTokenOrderParams{
			TokenSha256: hashJoinToken(token), Now: ts(a.now().UTC()),
		})
		if errors.Is(err, sql.ErrNoRows) {
			row, err = ledgerdb.ReadJoinTokenOrderRow{}, nil
		}

		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("alloc: read which order a join token is for: %w", err)
	}

	return row.PoolOrder, row.Live == 1, nil
}

// RecordHeldPoolRequest records a request from a host whose order's join is held, under
// its order and pending, WITHOUT SPENDING ANY TOKEN, so a person deciding about the
// hold has the key the host presents to compare and approve (ADR-030). A held order
// admits nothing on its own, so the record grants nothing; without it, a host whose
// token lapsed while the order was held would have no request a person could approve.
// The name is still claimed by the first key to ask: another key's request is answered
// ErrEnrollmentConflict and recorded nowhere, and a denied key is replaced.
func (a *Allocator) RecordHeldPoolRequest(ctx context.Context, order, fingerprint, csrPEM string) (Enrollment, error) {
	name := PoolNodeName(order)

	var out Enrollment

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		row, err := openPoolOrderTx(ctx, q, order)
		if err == nil {
			return fmt.Errorf("%w: pool order %s is not held", ErrPoolJoinNotHeld, order)
		}
		if !errors.Is(err, ErrPoolJoinHeld) || row.JoinHold == "" {
			return err
		}

		existing, err := readEnrollment(ctx, q, name)
		switch {
		case err == nil && existing.Fingerprint == fingerprint && existing.State == EnrollDenied:
			out = existing

			return nil
		case err == nil && existing.State != EnrollDenied && existing.Fingerprint != fingerprint:
			return fmt.Errorf("%w: %s is held by %s", ErrEnrollmentConflict, name, existing.Fingerprint)
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}

		out, err = a.requestEnrollmentTx(ctx, tx, name, fingerprint, csrPEM, order)

		return err
	})

	return out, err
}

// ErrPoolJoinNotHeld is a release of a hold the order does not have.
var ErrPoolJoinNotHeld = errors.New("alloc: this pool order's join is not held")

// ReleasePoolJoin releases an order's held join on a person's word, so its host's next
// request is judged again. Only an order a node may still join from has one to release.
func (a *Allocator) ReleasePoolJoin(ctx context.Context, order string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		n, err := q.ReleasePoolOrderJoin(ctx, ledgerdb.ReleasePoolOrderJoinParams{OrderID: order, UpdatedAt: ts(a.now())})
		if err != nil {
			return fmt.Errorf("alloc: release the held join of pool order %s: %w", order, err)
		}

		if n == 1 {
			return nil
		}

		row, err := q.ReadPoolOrder(ctx, order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", order, err)
		case row.JoinHold == "":
			return fmt.Errorf("%w: %s", ErrPoolJoinNotHeld, order)
		}

		return fmt.Errorf("%w: pool order %s is %s, joined by %q", ErrPoolOrderClosed, order, row.State, row.NodeName)
	})
}

// ReadPoolOrder reads one order.
func (a *Allocator) ReadPoolOrder(ctx context.Context, id string) (PoolOrder, error) {
	var out PoolOrder

	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadPoolOrder(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, id)
		}
		if err != nil {
			return fmt.Errorf("alloc: read pool order %s: %w", id, err)
		}

		out, err = poolOrderFrom(&row)

		return err
	})

	return out, err
}

// HoldPoolJoin holds an order's join for a person, keeping the first reason given.
func (a *Allocator) HoldPoolJoin(ctx context.Context, order, reason string) error {
	if reason == "" {
		return fmt.Errorf("alloc: pool order %s: a hold needs the reason a person reads", order)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		n, err := q.HoldPoolOrderJoin(ctx, ledgerdb.HoldPoolOrderJoinParams{
			OrderID: order, JoinHold: reason, UpdatedAt: ts(a.now()),
		})
		if err != nil {
			return fmt.Errorf("alloc: hold pool order %s: %w", order, err)
		}

		if n == 1 {
			return nil
		}

		// HELD ALREADY, OR CLOSED (a node joined, or it is no longer placed), so there is
		// nothing to hold: only no such order is a failure.
		if _, err := q.ReadPoolOrder(ctx, order); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, order)
		} else if err != nil {
			return fmt.Errorf("alloc: hold pool order %s: %w", order, err)
		}

		return nil
	})
}

// HoldLapsedPoolJoins holds every placed order that no node joined from and that
// nothing can join from any more: no live token left and no request waiting for a
// person. It answers how many it held.
func (a *Allocator) HoldLapsedPoolJoins(ctx context.Context) (int, error) {
	var held int64

	now := ts(a.now().UTC())

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error

		held, err = state.WriteQueries(tx).HoldLapsedPoolJoins(ctx, ledgerdb.HoldLapsedPoolJoinsParams{
			JoinHold: "every join token of this order lapsed and no node joined from it; look at its host " +
				"before abandoning or releasing it",
			UpdatedAt: now,
			Now:       now,
		})

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("alloc: hold the pool orders nothing can join from: %w", err)
	}

	return int(held), nil
}

// PoolAdmission is a pool host's enrollment its source vouched for, and the
// certificate signed for it.
type PoolAdmission struct {
	Order       string
	Fingerprint string
	CSRPEM      string
	Token       string
	CertPEM     string
	Serial      string
	NotAfter    string
}

// AdmitPoolEnrollment admits a pool host on its source's word, in one transaction
// (ADR-030): the order is placed and held for no one, the token is a live one of
// that order (spent here unless the request was already recorded), the enrollment is
// recorded approved under the order's name with the certificate, the certificate's
// serial is recorded for revocation, the order is linked to its node with the key and
// serial, and every other token of the order is retired.
//
// A RETRY WITH THE SAME KEY FINDS ITSELF ADMITTED and is answered what it was handed
// the first time; the certificate signed for the retry is never recorded or returned.
// A different key already holding the name holds the order for a person, because the
// source says this key is the host's and something else claimed its name first.
func (a *Allocator) AdmitPoolEnrollment(ctx context.Context, adm PoolAdmission) (Enrollment, error) {
	switch {
	case adm.Order == "" || adm.Fingerprint == "" || adm.CSRPEM == "":
		return Enrollment{}, errors.New("alloc: a pool admission needs its order, its key's fingerprint and its request")
	case adm.CertPEM == "" || adm.Serial == "" || adm.NotAfter == "":
		return Enrollment{}, errors.New("alloc: a pool admission needs the certificate signed for it and its serial")
	}

	name := PoolNodeName(adm.Order)

	var (
		out      Enrollment
		conflict string
	)

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		now := ts(a.now().UTC())

		order, err := q.ReadPoolOrder(ctx, adm.Order)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, adm.Order)
		case err != nil:
			return fmt.Errorf("alloc: read pool order %s: %w", adm.Order, err)
		case order.NodeName != "":
			if order.NodeName != name || order.NodeFingerprint != adm.Fingerprint {
				return fmt.Errorf("%w: %s joined from pool order %s under another key", ErrPoolOrderClosed,
					order.NodeName, adm.Order)
			}

			out, err = readEnrollment(ctx, q, name)

			return err
		}

		if _, err := openPoolOrderTx(ctx, q, adm.Order); err != nil {
			return err
		}

		existing, err := readEnrollment(ctx, q, name)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := a.spendPoolJoinTokenTx(ctx, tx, adm.Order, adm.Token); err != nil {
				return err
			}

			if err := q.InsertAdmittedEnrollment(ctx, ledgerdb.InsertAdmittedEnrollmentParams{
				Name: name, Fingerprint: adm.Fingerprint, CsrPem: adm.CSRPEM, CertPem: adm.CertPEM,
				RequestedAt: now, DecidedAt: now, PoolOrder: adm.Order,
			}); err != nil {
				return fmt.Errorf("alloc: record the admission of %s: %w", name, err)
			}
		case err != nil:
			return err
		case existing.Fingerprint != adm.Fingerprint && existing.State != EnrollDenied:
			// HELD IN THIS TRANSACTION, which read the order open and the name taken, so
			// nothing can admit or decide between the reading and the hold.
			conflict = existing.Fingerprint

			if _, err := q.HoldPoolOrderJoin(ctx, ledgerdb.HoldPoolOrderJoinParams{
				OrderID: adm.Order, UpdatedAt: now,
				JoinHold: fmt.Sprintf("the source vouched for key %s, and %s already holds the name %s",
					adm.Fingerprint, conflict, name),
			}); err != nil {
				return fmt.Errorf("alloc: hold pool order %s: %w", adm.Order, err)
			}

			return nil
		case existing.Fingerprint == adm.Fingerprint && existing.State == EnrollDenied:
			// A PERSON DENIED THIS KEY, and the source's word does not overrule them.
			out = existing

			return nil
		case existing.Fingerprint == adm.Fingerprint && existing.State == EnrollPending &&
			existing.PoolOrder == adm.Order:
			if err := approvePendingPoolTx(ctx, q, name, adm, now); err != nil {
				return err
			}
		case existing.State == EnrollDenied:
			// A DENIAL HOLDS NOTHING: another key may take the name, and this one pays.
			if err := a.spendPoolJoinTokenTx(ctx, tx, adm.Order, adm.Token); err != nil {
				return err
			}

			if err := q.ReplaceDeniedEnrollment(ctx, ledgerdb.ReplaceDeniedEnrollmentParams{
				Fingerprint: adm.Fingerprint, CsrPem: adm.CSRPEM, State: EnrollPending,
				RequestedAt: now, PoolOrder: adm.Order, Name: name,
			}); err != nil {
				return fmt.Errorf("alloc: record the admission of %s: %w", name, err)
			}

			if err := approvePendingPoolTx(ctx, q, name, adm, now); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s is %s under pool order %q, not a request from pool order %s",
				ErrPoolOrderClosed, name, existing.State, existing.PoolOrder, adm.Order)
		}

		if err := recordIssuedCertTx(ctx, q, IssuedCert{
			Serial: adm.Serial, Node: name, Source: CertPool, NotAfter: adm.NotAfter,
		}, now); err != nil {
			return err
		}

		if err := a.linkPoolNodeTx(ctx, q, adm.Order, name, adm.Fingerprint, adm.Serial, false); err != nil {
			return err
		}

		out, err = readEnrollment(ctx, q, name)

		return err
	})
	if err != nil {
		return Enrollment{}, err
	}

	if conflict != "" {
		return Enrollment{}, fmt.Errorf("%w: %s is held by %s", ErrEnrollmentConflict, name, conflict)
	}

	return out, nil
}

func approvePendingPoolTx(ctx context.Context, q state.WriteOps, name string, adm PoolAdmission, now string) error {
	n, err := q.AdmitPendingEnrollment(ctx, ledgerdb.AdmitPendingEnrollmentParams{
		CertPem: adm.CertPEM, DecidedAt: now, Name: name, Fingerprint: adm.Fingerprint, PoolOrder: adm.Order,
	})
	if err != nil {
		return fmt.Errorf("alloc: approve the admission of %s: %w", name, err)
	}

	if n != 1 {
		return fmt.Errorf("%w: the request of %s moved while it was being admitted", ErrPoolOrderClosed, name)
	}

	return nil
}

// openPoolOrderTx reads an order a node may join from: placed, held for no one, and
// with no node joined from it yet.
func openPoolOrderTx(ctx context.Context, q state.WriteOps, id string) (ledgerdb.PoolOrder, error) {
	row, err := q.ReadPoolOrder(ctx, id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return row, fmt.Errorf("%w: no pool order %s", ErrPoolOrderClosed, id)
	case err != nil:
		return row, fmt.Errorf("alloc: read pool order %s: %w", id, err)
	case row.State == PoolOrdering:
		return row, fmt.Errorf("%w: %s", ErrPoolOrderNotPlaced, id)
	case row.State != PoolPlaced:
		return row, fmt.Errorf("%w: pool order %s is %s", ErrPoolOrderClosed, id, row.State)
	case row.NodeName != "":
		return row, fmt.Errorf("%w: %s already joined from pool order %s", ErrPoolOrderClosed, row.NodeName, id)
	case row.JoinHold != "":
		return row, fmt.Errorf("%w: %s", ErrPoolJoinHeld, row.JoinHold)
	}

	return row, nil
}

// spendPoolJoinTokenTx consumes one use of a live token of this order.
func (a *Allocator) spendPoolJoinTokenTx(ctx context.Context, tx *sql.Tx, order, token string) error {
	if token == "" {
		return ErrBadJoinToken
	}

	res, err := state.WriteQueries(tx).SpendPoolJoinToken(ctx, ledgerdb.SpendPoolJoinTokenParams{
		TokenSha256: hashJoinToken(token), PoolOrder: order, Now: ts(a.now().UTC()),
	})
	if err != nil {
		return fmt.Errorf("alloc: spend a join token: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alloc: spend a join token: %w", err)
	}

	if n == 0 {
		return ErrBadJoinToken
	}

	return nil
}

// linkPoolNodeTx links a placed order to the node that joined from it, with the key
// it was admitted with and its certificate's serial, and retires the order's other
// tokens. heldMay is a person's approval, which a hold waits for.
func (a *Allocator) linkPoolNodeTx(
	ctx context.Context, q state.WriteOps, order, name, fingerprint, serial string, heldMay bool,
) error {
	if serial == "" {
		return fmt.Errorf("alloc: linking %s to pool order %s needs its certificate's serial", name, order)
	}

	if name != PoolNodeName(order) {
		return fmt.Errorf("%w: %s is not the node pool order %s names", ErrPoolOrderClosed, name, order)
	}

	row, err := openPoolOrderTx(ctx, q, order)
	if err != nil && (!heldMay || !errors.Is(err, ErrPoolJoinHeld)) {
		return err
	}

	// THE HOLD AS IT WAS READ, so a hold committed between that read and this write
	// links nothing on either engine.
	n, err := q.LinkPoolOrderNode(ctx, ledgerdb.LinkPoolOrderNodeParams{
		NodeName: name, NodeFingerprint: fingerprint, NodeCertSerial: serial,
		UpdatedAt: ts(a.now()), OrderID: order, JoinHold: row.JoinHold,
	})
	if err != nil {
		return fmt.Errorf("alloc: link %s to pool order %s: %w", name, order, err)
	}

	if n != 1 {
		return fmt.Errorf("%w: pool order %s moved while %s joined from it", ErrPoolOrderClosed, order, name)
	}

	if err := q.RetirePoolJoinTokens(ctx, order); err != nil {
		return fmt.Errorf("alloc: retire the join tokens of pool order %s: %w", order, err)
	}

	return nil
}
