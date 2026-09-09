package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrInspect means this handle was opened for a report and may not write.
var ErrInspect = errors.New("state: this handle was opened to inspect the ledger and may not write")

// ErrNoLedger means there is no ledger at the state directory to inspect, said
// positively: the directory or the file is absent, which an inspection never
// repairs by creating either.
var ErrNoLedger = errors.New("state: no ledger to inspect")

// OpenInspect opens an EXISTING SQLite ledger for a report and nothing else.
//
// A CONSTRUCTION PATH OF ITS OWN, beside OpenAdmin and never a flag on it,
// because an operator command's open is allowed to repair and an inspection is
// not: OpenAdmin creates the state directory, tightens its mode, takes the
// directory lock when it is free, migrates a ledger nothing holds and records
// nothing only because a command never claims. A report read by a converge's
// check job must leave the host as it found it, or the check would hand the
// converge a ledger the check itself had migrated, locked or minted. So this
// keeps the VALIDATION of an open and none of its MUTATION: the migration set
// must load; the state directory and the ledger file must already exist
// (ErrNoLedger otherwise, and the driver is never given the chance to create
// one); the maintenance fence is honoured; no directory lock, no controller
// claim, no migration and no watermark write; the connection is opened
// read-only (mode=ro on SQLite, which still reads a live WAL correctly; never
// immutable=1, which does not); the schema is verified to be EXACTLY this
// binary's, migrations and checksums included, so a ledger behind it is
// ErrSchemaBehind naming the control plane's restart rather than a migration
// the report performed; the release watermark is checked and never raised; and
// every read transaction keeps the revalidation an unlocked handle gets, since
// a control plane may migrate underneath a report. Tx is refused with
// ErrInspect.
//
// The integrity scan is the control plane's and is not run here, for the
// reason OpenAdmin gives: a whole-file read in front of a report is a cost
// with no decision behind it.
func OpenInspect(ctx context.Context, stateDir string, opts ...OpenOption) (*DB, error) {
	if err := requireLedgerFile(stateDir, LedgerPath(stateDir)); err != nil {
		return nil, err
	}

	return openDir(ctx, stateDir, newSQLiteBackend(stateDir), openMode{inspect: true}.with(opts))
}

// OpenPostgresInspect is OpenInspect for a ledger in PostgreSQL: the state
// directory must exist (it holds the identity and the fence), nothing local is
// created or locked, the connection's default transaction is read-only, the
// schema is verified exactly and never migrated, and Tx is refused.
func OpenPostgresInspect(ctx context.Context, stateDir, dsn string, opts ...OpenOption) (*DB, error) {
	if err := requireLedgerFile(stateDir, ""); err != nil {
		return nil, err
	}

	return openDir(ctx, stateDir, newPostgresBackend(dsn), openMode{inspect: true}.with(opts))
}

// requireLedgerFile proves the state directory, and the ledger file when one is
// named, already exist as a directory and a regular file, by Lstat and without
// following a symlink at either name. An absent one is ErrNoLedger; a read that
// failed for any other reason is that error, never absence.
func requireLedgerFile(stateDir, ledger string) error {
	info, err := os.Lstat(stateDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s does not exist", ErrNoLedger, stateDir)
	case err != nil:
		return fmt.Errorf("state: inspect %s: %w", stateDir, err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrNoLedger, stateDir)
	}

	if ledger == "" {
		return nil
	}

	info, err = os.Lstat(ledger)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s does not exist", ErrNoLedger, ledger)
	case err != nil:
		return fmt.Errorf("state: inspect %s: %w", ledger, err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrNoLedger, ledger)
	}

	return nil
}
