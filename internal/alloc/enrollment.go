package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Enrollment states.
const (
	EnrollPending  = "pending"
	EnrollApproved = "approved"
	EnrollDenied   = "denied"
)

// ErrEnrollmentConflict means a different key already claimed this node name.
//
// A NAME IS CLAIMED BY THE FIRST KEY TO ASK, and a second one is refused rather
// than overwriting it. The alternative loses the property approval exists for: an
// operator who compared a fingerprint yesterday would be approving a different
// machine today, under a name they already trust.
var ErrEnrollmentConflict = errors.New("alloc: another key has already requested this node name")

// Enrollment is a machine asking to join, and what was decided.
type Enrollment struct {
	Name        string
	Fingerprint string
	CSRPEM      string
	CertPEM     string
	State       string
	// Source is how this machine got in: `enrolled` (it asked and an operator
	// approved a fingerprint) or `issued` (an operator handed it a bundle).
	Source      string
	RequestedAt string
	DecidedAt   string
}

// RequestEnrollment records a node asking to join, or returns what it was
// already told.
//
// IDEMPOTENT FOR THE SAME KEY, because the node polls this until it is approved.
// A second request from the same fingerprint is the same request; one from a
// different fingerprint is a different machine claiming a taken name.
func (a *Allocator) RequestEnrollment(ctx context.Context, name, fingerprint, csrPEM string) (Enrollment, error) {
	var out Enrollment

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		existing, err := readEnrollment(ctx, tx, name)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			now := ts(a.now().UTC())

			if _, err := tx.ExecContext(ctx,
				`INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
				 VALUES (?, ?, ?, ?, ?)`,
				name, fingerprint, csrPEM, EnrollPending, now); err != nil {
				return fmt.Errorf("alloc: record the enrollment of %s: %w", name, err)
			}

			out = Enrollment{
				Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM,
				State: EnrollPending, RequestedAt: now,
			}

			return nil
		case err != nil:
			return err
		}

		if existing.Fingerprint != fingerprint {
			return fmt.Errorf("%w: %s is held by %s", ErrEnrollmentConflict, name, existing.Fingerprint)
		}

		out = existing

		return nil
	})

	return out, err
}

// LookupEnrollment reads a request without creating one.
//
// SEPARATE FROM RequestEnrollment, which INSERTS. Reusing that to ask "does this
// already exist" would record a row as a side effect of the question — and with
// no CSR on it, because a question does not carry one, leaving an enrollment
// that can never be approved.
func (a *Allocator) LookupEnrollment(ctx context.Context, name string) (Enrollment, bool, error) {
	var (
		out   Enrollment
		found bool
	)

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		e, err := readEnrollment(ctx, tx, name)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			found = false

			return nil
		case err != nil:
			return err
		}

		out, found = e, true

		return nil
	})

	return out, found, err
}

// DecideEnrollment approves or denies a pending request.
//
// THE FINGERPRINT IS PART OF THE DECISION, not just a thing to look at. An
// operator approves the machine whose fingerprint they read off its console, and
// requiring it here is what makes that comparison load-bearing: approving by
// name alone would approve whatever is currently holding the name.
func (a *Allocator) DecideEnrollment(ctx context.Context, name, fingerprint, state, certPEM string) error {
	if state != EnrollApproved && state != EnrollDenied {
		return fmt.Errorf("alloc: %q is not a decision", state)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		existing, err := readEnrollment(ctx, tx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: no node called %s has asked to join", name)
		} else if err != nil {
			return err
		}

		if !fingerprintMatches(existing.Fingerprint, fingerprint) {
			return fmt.Errorf(
				"alloc: %s is asking to join with fingerprint %s, not %s; check the value the "+
					"node printed, and do not approve one you cannot account for",
				name, existing.Fingerprint, fingerprint)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE node_enrollments SET state = ?, cert_pem = ?, decided_at = ? WHERE name = ?`,
			state, certPEM, ts(a.now().UTC()), name); err != nil {
			return fmt.Errorf("alloc: record the decision on %s: %w", name, err)
		}

		return nil
	})
}

// fingerprintMatches compares a supplied fingerprint against the recorded one.
// An empty supplied value never matches: approving without naming what you are
// approving is the thing this check exists to prevent.
func fingerprintMatches(recorded, supplied string) bool {
	return supplied != "" && recorded == supplied
}

// RecordIssued writes down a certificate handed out directly, so both ways into
// a deployment leave the same trail.
//
// `billet ca issue` is the older path and the right one for a machine being
// provisioned anyway — cloud-init can drop a bundle on it, and no human is
// standing there to compare a fingerprint. It recorded NOTHING, so there was no
// single answer to "what has been admitted here, and when": a fleet built that
// way was invisible to the same list that shows what is waiting.
//
// Marked as its own source, because the two are not the same fact. One was
// approved by somebody comparing a fingerprint; this one was issued.
func (a *Allocator) RecordIssued(ctx context.Context, name, fingerprint, certPEM string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		now := ts(a.now().UTC())

		_, err := tx.ExecContext(ctx,
			`INSERT INTO node_enrollments
			   (name, fingerprint, csr_pem, cert_pem, state, source, requested_at, decided_at)
			 VALUES (?, ?, '', ?, ?, 'issued', ?, ?)
			 ON CONFLICT (name) DO UPDATE SET
			   fingerprint = excluded.fingerprint,
			   cert_pem    = excluded.cert_pem,
			   state       = excluded.state,
			   source      = excluded.source,
			   decided_at  = excluded.decided_at`,
			name, fingerprint, certPEM, EnrollApproved, now, now)
		if err != nil {
			return fmt.Errorf("alloc: record the certificate issued to %s: %w", name, err)
		}

		return nil
	})
}

// Enrollments lists what has asked to join.
func (a *Allocator) Enrollments(ctx context.Context, state string) ([]Enrollment, error) {
	var out []Enrollment

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		out = nil

		query := `SELECT name, fingerprint, csr_pem, cert_pem, state, source, requested_at, decided_at
		            FROM node_enrollments`
		args := []any{}

		if state != "" {
			query += ` WHERE state = ?`

			args = append(args, state)
		}

		query += ` ORDER BY requested_at`

		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("alloc: list enrollments: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var e Enrollment
			if err := rows.Scan(&e.Name, &e.Fingerprint, &e.CSRPEM, &e.CertPEM,
				&e.State, &e.Source, &e.RequestedAt, &e.DecidedAt); err != nil {
				return fmt.Errorf("alloc: scan an enrollment: %w", err)
			}

			out = append(out, e)
		}

		return rows.Err()
	})

	return out, err
}

func readEnrollment(ctx context.Context, tx *sql.Tx, name string) (Enrollment, error) {
	var e Enrollment

	err := tx.QueryRowContext(ctx,
		`SELECT name, fingerprint, csr_pem, cert_pem, state, source, requested_at, decided_at
		   FROM node_enrollments WHERE name = ?`, name).
		Scan(&e.Name, &e.Fingerprint, &e.CSRPEM, &e.CertPEM, &e.State, &e.Source,
			&e.RequestedAt, &e.DecidedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("alloc: read the enrollment of %s: %w", name, err)
	}

	return e, err
}
