package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
	"github.com/junioryono/billet/internal/version"
)

// ErrReleaseBehind means this binary is older than the release that last served
// the ledger it is opening.
//
// ITS OWN ERROR because the remedy is specific: install the release the ledger
// names or newer, restore the archive that matches this binary, or downgrade on
// purpose with `billet host-upgrade --allow-downgrade`. A bare failure to open
// would send an operator to the database.
var ErrReleaseBehind = errors.New("state: this billet is older than the release that " +
	"last served this ledger")

// OpenOption configures one open of the ledger.
type OpenOption func(*openMode)

// WithRunningRelease names the billet that is opening the ledger, so the open can
// refuse a proved downgrade and the control plane can record a proved upgrade.
//
// PASSED IN RATHER THAN READ HERE, for the reason releasesource.Current gives: a
// package that read version.Version() itself could only ever be tested against
// the build running the test. cmd/billet passes it on every open, and a
// structural test there proves no open site forgets; a caller that passes
// nothing gets no check and no record, which is what every test that opens a
// throwaway ledger wants.
func WithRunningRelease(release string) OpenOption {
	return func(m *openMode) { m.release = release }
}

// releaseWatermarkMigration is the version that created the table.
//
// CONSULTED BEFORE THE TABLE IS READ, because two of the handles that check the
// mark may legitimately be looking at a schema that predates it: a standby
// tolerates a ledger behind its own binary, and a probe opens what it inherited.
// Reading a table that is not there would answer "could not open", which is not
// what an unmigrated ledger means.
const releaseWatermarkMigration = 48

// enforceReleaseWatermark refuses a proved downgrade and, for the one handle
// entitled to, records a proved upgrade.
//
// THREE ANSWERS, AND ONLY TWO OF THEM ACT. The running release is provably older
// than the mark: refused, whoever is asking. Provably newer: recorded, but only by
// the control plane that holds the exclusion — an operator command run from a
// newer binary records nothing, or a `billet check` from a laptop would fence the
// running server out of its own next restart. Equal, or not comparable at all (a
// development build, a snapshot, an unstamped binary against a release, or no
// running release named): nothing, said at debug level, because "could not tell"
// must not become "refuse" in either direction.
//
// AFTER THE SCHEMA IS SETTLED, never before: the table this reads arrived in a
// migration, and the migrate or verify that precedes this is what makes reading
// it a question with an answer.
func (db *DB) enforceReleaseWatermark(ctx context.Context, running string, record bool) error {
	if running == "" {
		return nil
	}

	applied, err := db.releaseWatermarkApplied(ctx)
	if err != nil || !applied {
		return err
	}

	recorded, recordedAt, err := db.ReleaseWatermark(ctx)
	if err != nil {
		return err
	}

	if recorded == "" {
		// ONLY A RELEASE TAG IS EVER RECORDED, on a fresh mark as much as on a
		// raise: "(devel)" written here would make every later open "could not
		// tell" for as long as the row lived.
		if record && version.IsRelease(running) {
			return db.writeReleaseWatermark(ctx, running)
		}

		return nil
	}

	order, ok := version.Compare(running, recorded)
	if !ok {
		slog.Debug("the running release and the ledger's release watermark cannot be ordered, "+
			"so neither is refused nor recorded", "running", running, "recorded", recorded)

		return nil
	}

	switch {
	case order < 0:
		return fmt.Errorf("%w: it was last served by %s (recorded %s) and this binary is %s. "+
			"Install %s or newer, or restore the archive that matches this binary. To run %s "+
			"here on purpose: `billet host-upgrade --version %s --allow-downgrade`",
			ErrReleaseBehind, recorded, recordedAt, running, recorded, running, running)
	case order > 0 && record:
		return db.writeReleaseWatermark(ctx, running)
	}

	return nil
}

// raiseReleaseWatermarkIn records the running release inside the controller
// claim's transaction: a proved newer release is written, an equal or
// uncomparable one leaves the row alone, and a proved older one refuses the
// claim, since a newer control plane recorded itself between this handle's open
// and its claim.
//
// THE TABLE MAY NOT BE THERE: a standby promoting onto a ledger the leader never
// migrated past 47 has just applied 48 itself, but a control plane on an older
// binary claiming a ledger nobody migrated has no table to read, and that is an
// ordinary state rather than a fault.
func (db *DB) raiseReleaseWatermarkIn(ctx context.Context, tx *sql.Tx) error {
	running := db.runningRelease
	if running == "" {
		return nil
	}

	seen, err := readAppliedMigrations(ctx, ReadQueries(tx))
	if err != nil {
		return fmt.Errorf("state: read whether the ledger records a release watermark: %w", err)
	}

	if _, applied := seen[releaseWatermarkMigration]; !applied {
		return nil
	}

	row, err := ReadQueries(tx).ReadReleaseWatermark(ctx)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !version.IsRelease(running) {
			return nil
		}
	case err != nil:
		return fmt.Errorf("state: read the ledger's release watermark: %w", err)
	default:
		order, ok := version.Compare(running, row.Release)
		if !ok {
			slog.Debug("the running release and the ledger's release watermark cannot be "+
				"ordered, so neither is refused nor recorded", "running", running,
				"recorded", row.Release)

			return nil
		}

		if order < 0 {
			return fmt.Errorf("%w: %s recorded itself as serving this ledger (%s) after this %s "+
				"handle opened, so the claim is refused", ErrReleaseBehind, row.Release,
				row.RecordedAt, running)
		}

		if order == 0 {
			return nil
		}
	}

	if err := WriteQueries(tx).SetReleaseWatermark(ctx, ledgerdb.SetReleaseWatermarkParams{
		Release:    running,
		RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return fmt.Errorf("state: record %s as the release serving this ledger: %w", running, err)
	}

	return nil
}

// checkReleaseWatermarkIn refuses, inside a revalidated write transaction, a
// running release that the mark has moved past since this handle opened.
//
// THE TABLE IS THERE TO READ: a revalidated handle has just verified the schema
// is the one this binary carries, and this binary carries migration 48. What can
// have changed is the row, by a newer control plane claiming in the meantime.
func (db *DB) checkReleaseWatermarkIn(ctx context.Context, q Querier) error {
	if db.runningRelease == "" {
		return nil
	}

	row, err := ReadQueries(q).ReadReleaseWatermark(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("state: re-read the ledger's release watermark: %w", err)
	}

	if order, ok := version.Compare(db.runningRelease, row.Release); ok && order < 0 {
		return fmt.Errorf("%w: %s recorded itself as serving this ledger (%s) after this %s "+
			"handle opened, so the write is refused", ErrReleaseBehind, row.Release,
			row.RecordedAt, db.runningRelease)
	}

	return nil
}

// releaseWatermarkApplied reports whether the ledger's schema carries the table.
func (db *DB) releaseWatermarkApplied(ctx context.Context) (bool, error) {
	var applied bool

	err := db.View(ctx, func(q Querier) error {
		seen, err := readAppliedMigrations(ctx, ReadQueries(q))
		if err != nil {
			return err
		}

		_, applied = seen[releaseWatermarkMigration]

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("state: read whether the ledger records a release watermark: %w",
			err)
	}

	return applied, nil
}

// ReleaseWatermark reports the newest release that has served this ledger and
// when it was recorded, or empty strings for a ledger nothing has recorded on.
//
// FOR A DIAGNOSTIC AND FOR THE OPEN-TIME CHECK, never for a scheduling decision.
// An absent row is an ordinary state — every ledger upgrading through the release
// that adds the table — and is reported as empty rather than as an error.
func (db *DB) ReleaseWatermark(ctx context.Context) (string, string, error) {
	var (
		release    string
		recordedAt string
	)

	err := db.View(ctx, func(q Querier) error {
		row, err := ReadQueries(q).ReadReleaseWatermark(ctx)
		if err != nil {
			return err
		}

		release, recordedAt = row.Release, row.RecordedAt

		return nil
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("state: read the ledger's release watermark: %w", err)
	}

	return release, recordedAt, nil
}

// SetReleaseWatermark moves the mark to a release an operator chose, in either
// direction.
//
// THE ONE WRITE THAT MAY LOWER IT, and it is allowed through the maintenance
// handle — the typed entry the host-upgrade transaction opens after it has
// snapshotted the ledger, so the higher mark survives in the snapshot a rollback
// restores — and, on an external ledger, through the operator handle, because
// there the maintenance open is a probe that refuses every write and there is no
// snapshot for the mark to survive in. Every other writer moves the mark
// forwards or not at all.
func (db *DB) SetReleaseWatermark(ctx context.Context, release string) error {
	external := db.admin && db.backend.engine() != "sqlite"
	if !db.maintenanceBypass && !external {
		return errors.New("state: the release watermark is lowered only through the host " +
			"upgrade's maintenance handle; nothing else may move it backwards")
	}

	if !version.IsRelease(release) {
		return fmt.Errorf("state: %q is not a release tag, so it cannot be recorded as the "+
			"release serving this ledger", release)
	}

	applied, err := db.releaseWatermarkApplied(ctx)
	if err != nil {
		return err
	}

	if !applied {
		// NOTHING TO LOWER. A ledger that predates the table carries no mark, so
		// an older binary opening it is refused by nothing here anyway.
		return nil
	}

	return db.writeReleaseWatermark(ctx, release)
}

// writeReleaseWatermark records one release as the one serving this ledger.
func (db *DB) writeReleaseWatermark(ctx context.Context, release string) error {
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).SetReleaseWatermark(ctx, ledgerdb.SetReleaseWatermarkParams{
			Release:    release,
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
	if err != nil {
		return fmt.Errorf("state: record %s as the release serving this ledger: %w", release, err)
	}

	return nil
}
