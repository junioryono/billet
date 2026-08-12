package alloc

import (
	"context"
	"database/sql"
	"fmt"
)

// RevokedCert is a credential that has been withdrawn.
type RevokedCert struct {
	Serial    string
	Node      string
	Reason    string
	RevokedAt string
}

// RevokeCert withdraws one certificate.
//
// KEYED ON SERIAL, not on node name. A name is legitimately re-issued to a
// replacement machine, and revoking the name would refuse the replacement too.
// The serial identifies the one credential being taken back.
//
// Idempotent: revoking twice is not an error, because an operator who is not
// sure whether the first attempt landed must be able to just run it again.
func (a *Allocator) RevokeCert(ctx context.Context, serial, node, reason string) error {
	if serial == "" {
		return fmt.Errorf("alloc: a revocation needs a certificate serial")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO revoked_certs (serial, node, reason, revoked_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (serial) DO NOTHING`,
			serial, node, reason, ts(a.now().UTC()))
		if err != nil {
			return fmt.Errorf("alloc: revoke certificate %s: %w", serial, err)
		}

		return nil
	})
}

// CertRevoked reports whether a certificate has been withdrawn.
//
// FAILS CLOSED IS NOT AN OPTION HERE, and that is worth being explicit about: a
// database error returns the error rather than a verdict, and the caller refuses
// the request. Answering "not revoked" on a failed read would make an unreadable
// ledger equivalent to an empty one, which is the whole check switched off by a
// transient fault.
func (a *Allocator) CertRevoked(ctx context.Context, serial string) (bool, error) {
	var revoked bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var one int

		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM revoked_certs WHERE serial = ?`, serial).Scan(&one); {
		case err == sql.ErrNoRows:
			revoked = false
		case err != nil:
			return fmt.Errorf("alloc: read the revocation list: %w", err)
		default:
			revoked = true
		}

		return nil
	})

	return revoked, err
}

// RevokedCerts lists what has been withdrawn, newest first.
func (a *Allocator) RevokedCerts(ctx context.Context) ([]RevokedCert, error) {
	var out []RevokedCert

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx,
			`SELECT serial, node, reason, revoked_at FROM revoked_certs ORDER BY revoked_at DESC`)
		if err != nil {
			return fmt.Errorf("alloc: list revoked certificates: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var r RevokedCert
			if err := rows.Scan(&r.Serial, &r.Node, &r.Reason, &r.RevokedAt); err != nil {
				return fmt.Errorf("alloc: scan a revoked certificate: %w", err)
			}

			out = append(out, r)
		}

		return rows.Err()
	})

	return out, err
}
