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

// IssuedCert is a credential this deployment handed out.
type IssuedCert struct {
	Serial   string
	Node     string
	Source   string
	NotAfter string
	IssuedAt string
}

// Sources a certificate can come from.
const (
	CertEnrolled = "enrolled"
	CertIssued   = "issued"
	CertRenewed  = "renewed"
)

// RecordIssuedCert writes down a credential at the moment it is handed out.
//
// WITHOUT THIS, REVOCATION CANNOT REACH A RENEWAL. Revocation names one serial,
// which is the right granularity — a node name is legitimately re-issued to a
// replacement machine — but it only works on serials billet knows about.
// Renewal mints a fresh key and serial over the wire, so a node that has renewed
// once is presenting a credential that exists nowhere but on that node. An
// operator revoking the bundle they originally issued takes back a serial nobody
// holds, and the host carries on.
func (a *Allocator) RecordIssuedCert(ctx context.Context, c IssuedCert) error {
	if c.Serial == "" {
		return fmt.Errorf("alloc: an issued certificate needs a serial")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO issued_certs (serial, node, source, not_after, issued_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (serial) DO NOTHING`,
			c.Serial, c.Node, c.Source, c.NotAfter, ts(a.now().UTC()))
		if err != nil {
			return fmt.Errorf("alloc: record the certificate issued to %s: %w", c.Node, err)
		}

		return nil
	})
}

// LiveCertsFor lists the credentials a node holds that are neither expired nor
// already revoked.
func (a *Allocator) LiveCertsFor(ctx context.Context, node string) ([]IssuedCert, error) {
	var out []IssuedCert

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx,
			`SELECT i.serial, i.node, i.source, i.not_after, i.issued_at
			   FROM issued_certs i
			   LEFT JOIN revoked_certs r ON r.serial = i.serial
			  WHERE i.node = ? AND r.serial IS NULL AND i.not_after > ?
			  ORDER BY i.issued_at DESC`, node, ts(a.now().UTC()))
		if err != nil {
			return fmt.Errorf("alloc: list the certificates held by %s: %w", node, err)
		}
		defer rows.Close()

		for rows.Next() {
			var c IssuedCert
			if err := rows.Scan(&c.Serial, &c.Node, &c.Source, &c.NotAfter, &c.IssuedAt); err != nil {
				return fmt.Errorf("alloc: scan a certificate: %w", err)
			}

			out = append(out, c)
		}

		return rows.Err()
	})

	return out, err
}

// RevokeNode withdraws every credential a node currently holds, and reports what
// it took back.
//
// THE HANDLE AN OPERATOR ACTUALLY HAS. Responding to a compromised machine means
// taking back everything that machine can present, and after a renewal that is
// not the bundle in their hands — it is a serial they have never seen. Revoking
// by serial from a file silently leaves the live credential working.
//
// A replacement machine under the same name is unaffected: this revokes the
// serials outstanding right now, and a certificate issued afterwards is not one
// of them.
func (a *Allocator) RevokeNode(ctx context.Context, node, reason string) ([]IssuedCert, error) {
	live, err := a.LiveCertsFor(ctx, node)
	if err != nil {
		return nil, err
	}

	for i := range live {
		if err := a.RevokeCert(ctx, live[i].Serial, node, reason); err != nil {
			return nil, err
		}
	}

	return live, nil
}
