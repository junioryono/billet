package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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

	err := a.db.View(ctx, func(tx querier) error {
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

	err := a.db.View(ctx, func(tx querier) error {
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

// ErrParentRevoked means a renewal was signed by a certificate that has since
// been taken back.
var ErrParentRevoked = errors.New("alloc: the certificate being renewed has been revoked")

// RecordRenewedCert records a renewal, refusing one whose parent was revoked.
//
// THE RACE THIS CLOSES. Revocation checks the presented certificate at the start
// of a request, and a renewal signed a new serial some milliseconds later; a
// revocation committing in between took back a credential the machine had
// already stopped presenting, and reported success. Recording the child in the
// same transaction that asks about the parent makes the order decide: either the
// renewal lands first and RevokeNode sees its serial, or the revocation lands
// first and this refuses.
//
// The wire refuses the renewal when this does, so the node keeps the certificate
// it has — which is the revoked one, and will be turned away on its next
// request. That is the intended outcome.
func (a *Allocator) RecordRenewedCert(
	ctx context.Context, cert IssuedCert, parent string, parentIssuedAt time.Time,
) error {
	if cert.Serial == "" {
		return fmt.Errorf("alloc: an issued certificate needs a serial")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		var one int

		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM revoked_certs WHERE serial = ?`, parent).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("alloc: read the revocation list: %w", err)
		default:
			return fmt.Errorf("%w: %s", ErrParentRevoked, parent)
		}

		// AND THE NODE CUTOFF, which is the half a serial cannot answer.
		//
		// The cutoff exists precisely for credentials billet never wrote down, so
		// checking only the serial leaves them a way out: an unrecorded certificate
		// opens a renewal, the revocation commits while its request body is still
		// arriving — nothing bounds that but the read-header timeout — and the
		// child it is handed is minted AFTER the cutoff and therefore accepted. The
		// node renews its way out of a revocation it was never named in.
		var cutoff string

		switch err := tx.QueryRowContext(ctx,
			`SELECT revoked_before FROM node_revocations WHERE node = ?`, cert.Node).Scan(&cutoff); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("alloc: read the revocation cutoff for %s: %w", cert.Node, err)
		default:
			if ts(parentIssuedAt.UTC().Truncate(time.Second)) < cutoff {
				return fmt.Errorf("%w: %s was issued before %s was revoked",
					ErrParentRevoked, parent, cert.Node)
			}
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO issued_certs (serial, node, source, not_after, issued_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (serial) DO NOTHING`,
			cert.Serial, cert.Node, cert.Source, cert.NotAfter, ts(a.now().UTC()))
		if err != nil {
			return fmt.Errorf("alloc: record the certificate issued to %s: %w", cert.Node, err)
		}

		return nil
	})
}

// LiveCertsFor lists the credentials a node holds that are neither expired nor
// already revoked.
func (a *Allocator) LiveCertsFor(ctx context.Context, node string) ([]IssuedCert, error) {
	var out []IssuedCert

	err := a.db.View(ctx, func(tx querier) error {
		var err error

		out, err = liveCertsForTx(ctx, tx, node, ts(a.now().UTC()))

		return err
	})

	return out, err
}

// liveCertsForTx is the query, inside a caller's transaction so a revocation can
// be atomic with the read that decided what to revoke.
func liveCertsForTx(ctx context.Context, tx querier, node, now string) ([]IssuedCert, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT i.serial, i.node, i.source, i.not_after, i.issued_at
		   FROM issued_certs i
		   LEFT JOIN revoked_certs r ON r.serial = i.serial
		  WHERE i.node = ? AND r.serial IS NULL AND i.not_after > ?
		  ORDER BY i.issued_at DESC`, node, now)
	if err != nil {
		return nil, fmt.Errorf("alloc: list the certificates held by %s: %w", node, err)
	}

	defer func() { _ = rows.Close() }()

	var out []IssuedCert

	for rows.Next() {
		var c IssuedCert
		if err := rows.Scan(&c.Serial, &c.Node, &c.Source, &c.NotAfter, &c.IssuedAt); err != nil {
			return nil, fmt.Errorf("alloc: scan a certificate: %w", err)
		}

		out = append(out, c)
	}

	return out, rows.Err()
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
//
// THE CUTOFF COMPARES TWO CLOCKS, and that is a real if narrow residual. The
// timestamps come from the certificate's own issuer and from this process, so a
// CA whose clock runs ahead can mint a credential shortly before a revocation
// that dates itself after the cutoff and escapes it. It only reaches
// certificates whose serials were never recorded — everything issued since this
// release is revoked by serial, where no clock is involved — so it is bounded to
// the legacy set the cutoff exists for, and a deployment that cannot enumerate
// its old certificates and does not trust its clocks should rotate the authority
// instead.
func (a *Allocator) RevokeNode(ctx context.Context, node, reason string) ([]IssuedCert, error) {
	var revoked []IssuedCert

	// ONE TRANSACTION, because a renewal is racing this.
	//
	// Reading the live certificates and then revoking them one at a time left a
	// window exactly where it hurts: a compromised host authenticates with the
	// certificate being taken back and asks to renew, the read misses the new
	// serial because it does not exist yet, the renewal records it, and the
	// revocation reports success over a credential the machine is no longer
	// presenting. Reading and revoking together makes the order decide: either
	// the renewal lands first and its serial is in the list, or this commits
	// first and the renewal is refused for renewing a revoked certificate.
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		revoked = nil
		now := ts(a.now().UTC())

		live, err := liveCertsForTx(ctx, tx, node, now)
		if err != nil {
			return err
		}

		for i := range live {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO revoked_certs (serial, node, reason, revoked_at)
				 VALUES (?, ?, ?, ?)
				 ON CONFLICT (serial) DO NOTHING`,
				live[i].Serial, node, reason, now); err != nil {
				return fmt.Errorf("alloc: revoke certificate %s: %w", live[i].Serial, err)
			}
		}

		// AND A CUTOFF, which is what reaches the credentials billet cannot name.
		//
		// Two ways for one to exist: a deployment upgraded from a version that did
		// not record serials, and a name issued more than once before it did — the
		// admission trail keeps one row per node and overwrites it, so an earlier
		// certificate is unrecoverable. Recording the moment refuses every
		// certificate for this name minted before it, seen or not, while a
		// replacement issued afterwards still works.
		// ROUNDED UP TO THE NEXT SECOND, because a certificate cannot express
		// anything finer. X.509 stores validity at one-second resolution, so the
		// issuance moment recovered from a certificate is always truncated — and a
		// cutoff carrying nanoseconds would sit AFTER a replacement minted in the
		// same second, refusing it. Rounding up puts the boundary between whole
		// seconds, where both sides can express it.
		//
		// The ambiguity inside the revocation's own second resolves toward
		// REFUSING: a credential minted in that second is treated as predating the
		// revocation. Re-issuing a second later is a second's inconvenience;
		// accepting a compromised certificate is not recoverable.
		cutoff := ts(a.now().UTC().Truncate(time.Second).Add(time.Second))

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_revocations (node, revoked_before, reason, revoked_at)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (node) DO UPDATE SET
			   revoked_before = excluded.revoked_before,
			   reason         = excluded.reason,
			   revoked_at     = excluded.revoked_at`,
			node, cutoff, reason, now); err != nil {
			return fmt.Errorf("alloc: record the revocation cutoff for %s: %w", node, err)
		}

		revoked = live

		return nil
	})

	return revoked, err
}

// CertRevokedFor reports whether a certificate has been withdrawn, by serial or
// by the cutoff its node carries.
//
// issuedAt IS WHEN THE CERTIFICATE WAS MINTED, which is NOT its NotBefore.
//
// Every certificate billet issues is valid from an hour before it was created,
// so that a node whose clock is behind the control plane's does not reject what
// it was just handed. Reading NotBefore as the issuance moment therefore places
// every certificate an hour earlier than it really is — and a replacement issued
// within an hour of a revocation would fall before the cutoff and be refused,
// which turns a revocation into a permanent ban on the node name. The caller
// adds wirecert.ClockSkew back before calling.
func (a *Allocator) CertRevokedFor(
	ctx context.Context, node, serial string, issuedAt time.Time,
) (bool, error) {
	var revoked bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		revoked = false

		var one int

		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM revoked_certs WHERE serial = ?`, serial).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("alloc: read the revocation list: %w", err)
		default:
			revoked = true

			return nil
		}

		var cutoff string

		switch err := tx.QueryRowContext(ctx,
			`SELECT revoked_before FROM node_revocations WHERE node = ?`, node).Scan(&cutoff); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the revocation cutoff for %s: %w", node, err)
		}

		// TRUNCATED TO THE SECOND on this side too, so the comparison is between
		// two values a certificate can actually carry.
		revoked = ts(issuedAt.UTC().Truncate(time.Second)) < cutoff

		return nil
	})

	return revoked, err
}
