package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrForeignLedger means these rows belong to a different deployment than the
// identity directory this process is using.
//
// ITS OWN ERROR because the remedy is specific and nothing else implies it: one
// of the two halves is pointed at the wrong place, and which half is the
// operator's to decide. An ordinary failure to open would send them to the
// database.
var ErrForeignLedger = errors.New("state: this ledger belongs to another deployment")

// bindDeployment records which deployment these rows belong to, or refuses.
//
// INSIDE THE CALLER'S WRITE TRANSACTION, which is what makes the read and the
// insert one decision. Every writer serializes — SQLite takes its lock at BEGIN
// IMMEDIATE, PostgreSQL takes pg_advisory_xact_lock in beginWrite — so nothing
// can bind between finding no row and writing one.
//
// AN ABSENT ROW IS "NOT YET BOUND", NEVER "BOUND TO NOBODY". A ledger migrated
// before migration 45 existed carries no binding, and so does one whose
// controller has not claimed yet; refusing there would refuse every deployment
// upgrading through the release that adds this.
//
// WHAT IT REFUSES is the state that has no safe reading: a ledger that says it
// belongs to one deployment being scheduled against by a process whose identity
// directory says another. On a shared ledger that is two control planes
// admitting nodes against two authorities while charging capacity into one
// record, and the first thing anybody notices is a fleet that will not connect.
func bindDeployment(ctx context.Context, tx *sql.Tx, deployment string) error {
	if deployment == "" {
		// NOT A BINDING. An empty identity is a caller that does not know which
		// deployment it is, and writing one would record a label nothing can ever
		// match — the column's CHECK refuses it anyway, and this says why.
		return errors.New(
			"state: refusing to bind this ledger to an empty deployment identity")
	}

	row, err := ReadQueries(tx).ReadDeploymentBinding(ctx)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WriteQueries(tx).BindDeployment(ctx, ledgerdb.BindDeploymentParams{
			DeploymentID: deployment,
			BoundAt:      time.Now().UTC().Format(time.RFC3339Nano),
		})
	case err != nil:
		// REPORTED AS THE FAULT IT IS. A read error is evidence about ownership in
		// neither direction: answering it by binding would relabel a ledger this
		// process could not read, and answering it by refusing would stop a healthy
		// control plane over a blip.
		return fmt.Errorf("state: read this ledger's deployment binding: %w", err)
	case row.DeploymentID != deployment:
		return foreignLedger(deployment, row)
	}

	return nil
}

// foreignLedger is the refusal, in one place so both callers say the same thing.
//
// IT NAMES BOTH IDENTITIES AND NEITHER REMEDY, deliberately. billet cannot tell
// which half is wrong — an identity directory restored from the wrong backup, or
// a DSN pointing at another deployment's schema — and guessing would send an
// operator to change the half that was right. What it can do is say exactly what
// disagrees and when the ledger was claimed, which is enough to decide.
func foreignLedger(deployment string, row ledgerdb.ReadDeploymentBindingRow) error {
	return fmt.Errorf(
		"%w: this ledger was bound to deployment %s on %s, and this process's identity "+
			"directory says %s. One of the two is pointed at the wrong place — either the "+
			"identity directory is not this deployment's, or the ledger is not. Nothing has "+
			"been written",
		ErrForeignLedger, row.DeploymentID, row.BoundAt, deployment)
}

// DeploymentBinding reports which deployment the ledger says it belongs to, or
// an empty string for one that has never been bound.
//
// FOR A DIAGNOSTIC, never for a decision — the same rule ControllerHolder
// follows. An unbound ledger is an ordinary state and is reported as an empty
// value rather than as an error.
func (db *DB) DeploymentBinding(ctx context.Context) (string, error) {
	var out string

	err := db.View(ctx, func(q Querier) error {
		row, err := ReadQueries(q).ReadDeploymentBinding(ctx)
		if err != nil {
			return err
		}

		out = row.DeploymentID

		return nil
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("state: read this ledger's deployment binding: %w", err)
	}

	return out, nil
}

// VerifyDeploymentBinding refuses a handle whose identity directory disagrees
// with what the ledger records, and writes nothing.
//
// THE READ-ONLY HALF, FOR EVERY PROCESS THAT IS NOT THE CONTROLLER. An operator
// command binds nothing — it is not the authority for what this ledger is — but
// pointing one at another deployment's rows is exactly as wrong as pointing a
// control plane at them, and it is reachable by one wrong DSN. So it asks, and
// an unbound ledger answers yes because that is what every deployment upgrading
// through this release looks like.
func (db *DB) VerifyDeploymentBinding(ctx context.Context, deployment string) error {
	if deployment == "" {
		return nil
	}

	bound, err := db.DeploymentBinding(ctx)
	if err != nil {
		return err
	}

	if bound == "" || bound == deployment {
		return nil
	}

	return foreignLedger(deployment, ledgerdb.ReadDeploymentBindingRow{DeploymentID: bound})
}
