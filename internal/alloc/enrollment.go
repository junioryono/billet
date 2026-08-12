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
		var err error

		out, err = a.requestEnrollmentTx(ctx, tx, name, fingerprint, csrPEM)

		return err
	})

	return out, err
}

// RequestEnrollmentWithToken records a request and spends the credential that
// authorised it, in ONE transaction.
//
// SEPARATELY THEY ARE A TRAP. The token is single-use and the decrement is
// atomic on its own, but committing it and then failing to insert the request —
// a crash, a busy ledger — burns the credential with nothing to show for it. The
// machine retries, is treated as new because no row exists, and finds its token
// spent: stranded until an operator mints another and notices why.
//
// THE TOKEN IS SPENT ONLY FOR A REQUEST THAT IS NEW. A node polls this endpoint
// until a human decides, so charging every call would spend a single-use token
// on the second poll and strand the machine it was minted for.
func (a *Allocator) RequestEnrollmentWithToken(
	ctx context.Context, name, fingerprint, csrPEM, token string,
) (Enrollment, error) {
	var out Enrollment

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		known, err := isKnownEnrollmentTx(ctx, tx, name, fingerprint)
		if err != nil {
			return err
		}

		if !known {
			if err := a.spendJoinTokenTx(ctx, tx, token); err != nil {
				return err
			}
		}

		out, err = a.requestEnrollmentTx(ctx, tx, name, fingerprint, csrPEM)

		return err
	})

	return out, err
}

// isKnownEnrollmentTx reports whether this exact request has already been
// recorded, which is what makes a poll free.
func isKnownEnrollmentTx(ctx context.Context, tx *sql.Tx, name, fingerprint string) (bool, error) {
	existing, err := readEnrollment(ctx, tx, name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}

	// THE FINGERPRINT ALONE DECIDES, whatever state the row is in.
	//
	// A node polls until it learns a decision, and "denied" IS a decision — it is
	// what stops the node retrying. Treating the same key asking again as a new
	// request made the poll spend another use of the token that already paid for
	// it, so a single-use token answered 401 instead of "denied" and the operator
	// saw a credential problem where there was a verdict.
	//
	// A DIFFERENT key against a denied row is a genuinely new request, replacing
	// one that no longer holds the name, and it pays.
	return existing.Fingerprint == fingerprint, nil
}

// requestEnrollmentTx is the body of a request, inside a caller's transaction.
func (a *Allocator) requestEnrollmentTx(
	ctx context.Context, tx *sql.Tx, name, fingerprint, csrPEM string,
) (Enrollment, error) {
	existing, err := readEnrollment(ctx, tx, name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := ts(a.now().UTC())

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_enrollments (name, fingerprint, csr_pem, state, requested_at)
			 VALUES (?, ?, ?, ?, ?)`,
			name, fingerprint, csrPEM, EnrollPending, now); err != nil {
			return Enrollment{}, fmt.Errorf("alloc: record the enrollment of %s: %w", name, err)
		}

		return Enrollment{
			Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM,
			State: EnrollPending, RequestedAt: now,
		}, nil
	case err != nil:
		return Enrollment{}, err
	}

	// A DENIED ROW HOLDS NOTHING. Denying is the only tool an operator has for a
	// request that should not proceed, and while it kept the name claimed it was
	// a one-way door: the enrolling process holds its private key in memory while
	// it waits for a human, so a reboot loses the key and leaves the row, and the
	// machine that comes back with a new one is refused forever. There was no way
	// out short of editing the ledger by hand.
	//
	// Pending and approved still hold it, which is the property approval depends
	// on — an operator who compared a fingerprint yesterday must not be approving
	// a different machine today under a name they already trust.
	if existing.State == EnrollDenied && existing.Fingerprint != fingerprint {
		now := ts(a.now().UTC())

		if _, err := tx.ExecContext(ctx,
			`UPDATE node_enrollments
			    SET fingerprint = ?, csr_pem = ?, cert_pem = '', state = ?,
			        source = 'enrolled', requested_at = ?, decided_at = ''
			  WHERE name = ?`,
			fingerprint, csrPEM, EnrollPending, now, name); err != nil {
			return Enrollment{}, fmt.Errorf("alloc: record the enrollment of %s: %w", name, err)
		}

		return Enrollment{
			Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM,
			State: EnrollPending, RequestedAt: now,
		}, nil
	}

	if existing.Fingerprint != fingerprint {
		return Enrollment{}, fmt.Errorf(
			"%w: %s is held by %s", ErrEnrollmentConflict, name, existing.Fingerprint)
	}

	return existing, nil
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

	err := a.db.View(ctx, func(tx querier) error {
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
// It REPORTS WHAT IT DISPLACED, because the wire refuses a second key under a
// name the first one claimed and this path does not. Overwriting is right here —
// an operator issuing a certificate is a deliberate act, and refusing would
// leave a name unusable after a machine was rebuilt — but it must not be silent:
// the fingerprint an operator compared yesterday stops describing anything, and
// nothing else would ever tell them.
func (a *Allocator) RecordIssued(
	ctx context.Context, name, fingerprint, certPEM string,
) (string, error) {
	var displaced string

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		now := ts(a.now().UTC())

		var previous string

		switch scanErr := tx.QueryRowContext(ctx,
			`SELECT fingerprint FROM node_enrollments WHERE name = ?`, name,
		).Scan(&previous); {
		case scanErr == nil:
			if previous != fingerprint {
				displaced = previous
			}
		case errors.Is(scanErr, sql.ErrNoRows):
		default:
			return fmt.Errorf("alloc: read what %s was already admitted as: %w", name, scanErr)
		}

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

	return displaced, err
}

// Enrollments lists what has asked to join.
func (a *Allocator) Enrollments(ctx context.Context, state string) ([]Enrollment, error) {
	var out []Enrollment

	err := a.db.View(ctx, func(tx querier) error {
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

func readEnrollment(ctx context.Context, tx querier, name string) (Enrollment, error) {
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
