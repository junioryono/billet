package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SnapshotInto writes a consistent copy of the ledger to path.
//
// VACUUM INTO IS THE MECHANISM, AND THE CONSTRAINTS ARE MEASURED RATHER THAN
// READ. Three of them decide the shape of this function:
//
//   - It cannot run inside a transaction — "SQL logic error: cannot VACUUM from
//     within a transaction (1)" — so this must not go through DB.Tx.
//   - It is refused on the query-only reader pool — "attempt to write a readonly
//     database (8)" — so it must go through the WRITER connection, which is why
//     this lives here rather than being something a caller could assemble out of
//     Reader().
//   - It REFUSES an existing destination: "SQL logic error: output file already
//     exists (1)". That is a no-clobber install for free, and it is the backstop
//     behind the check below rather than a replacement for it.
//
// A WAL reader includes committed frames up to its snapshot end mark, so the
// result is a consistent copy of a LIVE database and billet.db-wal need not be
// copied beside it. Measured: the snapshot carries no -wal of its own.
//
// IT HOLDS THE SINGLE WRITER SLOT FOR ITS DURATION, which is said here rather
// than discovered. The writer pool is one connection, so nothing else in this
// process commits while this runs, and for this ledger's size that is a short
// pause rather than a stall. Contention with ANOTHER process is retried on the
// same terms as any other write: busy is a race, not a verdict.
//
// The file SQLite creates is mode 0644 under the usual umask (measured), so it
// is tightened here — the caller's directory mode is not the only thing standing
// between a ledger copy and every account on the host.
func (db *DB) SnapshotInto(ctx context.Context, path string) error {
	if err := db.checkMaintenance(); err != nil {
		return err
	}

	// ABSOLUTE, BECAUSE SQLITE RESOLVES A RELATIVE PATH AGAINST THE PROCESS
	// WORKING DIRECTORY, not against the state directory this DB was opened on.
	// A caller passing "backup/billet.db" would get a snapshot somewhere neither
	// it nor the operator named, and would then report the path it asked for.
	if !filepath.IsAbs(path) {
		return fmt.Errorf("state: snapshot destination %s must be an absolute path; SQLite "+
			"resolves a relative one against this process's working directory", path)
	}

	// A COURTESY DIAGNOSTIC, NOT THE SAFETY PROPERTY. VACUUM INTO refuses an
	// occupied destination itself and atomically; this exists so the operator is
	// told which file is in the way rather than reading SQLite's own sentence
	// about an "output file".
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("state: %s already exists and billet will not write a ledger "+
			"snapshot over it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: inspect the snapshot destination %s: %w", path, err)
	}

	if err := db.backend.snapshotInto(ctx, db, path); err != nil {
		return err
	}

	// 0600 EXPLICITLY. The ledger holds join-token digests, certificate serials
	// and the admission record; SQLite created this file under the umask and has
	// no opinion about who else on the host may read it.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("state: tighten the ledger snapshot %s: %w", path, err)
	}

	if err := fsyncFile(path); err != nil {
		return err
	}

	return fsyncDir(filepath.Dir(path))
}

// AppliedMigration is one row of a ledger's migration bookkeeping, as a caller
// outside this package sees it.
//
// EXPORTED BECAUSE A BACKUP HAS TO RECORD IT AND A RESTORE HAS TO JUDGE IT. The
// unexported appliedMigration is keyed by version in a map for the migrator's
// own use; this is the ordered, self-describing form that goes into an archive
// manifest and comes back out of one.
type AppliedMigration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
}

// PeekMigrations reads the applied-migration set out of a database FILE.
//
// IT MUST NOT BE state.Open. This is asked about a snapshot, and about a target
// directory a restore has not committed to yet — Open creates the directory,
// chmods it, takes the process lock and MIGRATES, so using it to ask a question
// would upgrade a stopped ledger on the way to telling the operator the restore
// is refused.
//
// query_only, and the file must already exist: a caller asking about a database
// that is not there must not be handed one that now is.
func PeekMigrations(ctx context.Context, dbPath string) ([]AppliedMigration, error) {
	if err := requireRegularFile(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn(dbPath, "busy_timeout(2000)", "query_only(ON)", "foreign_keys(ON)"))
	if err != nil {
		return nil, fmt.Errorf("state: open %s to read its schema: %w", dbPath, err)
	}

	defer func() { _ = db.Close() }()

	if err := sqliteBackendAt(dbPath).checkBookkeepingSchema(ctx, db); err != nil {
		return nil, err
	}

	seen, err := readAppliedMigrations(ctx, ReadQueries(db))
	if err != nil {
		return nil, err
	}

	out := make([]AppliedMigration, 0, len(seen))
	for v, rec := range seen {
		out = append(out, AppliedMigration{Version: v, Name: rec.name, Checksum: rec.checksum})
	}

	sortMigrations(out)

	return out, nil
}

// AppliedMigrations reads the migration set out of an OPEN handle.
//
// THE LIVE DATABASE, WHICH PeekMigrations DELIBERATELY IS NOT. Peek opens a
// FILE, which is the right instrument for a snapshot: it reads back what was
// captured rather than what is happening now. This one is for the ledger there
// is no file of — a PostgreSQL deployment, where `billet local backup` writes an
// identity-only archive and the manifest records what the ledger carried at that
// moment as PROVENANCE.
//
// WHAT IT ANSWERS IS TRUE OF ONE INSTANT AND THE DATABASE KEEPS MOVING, so a
// caller must not treat it as a description of an artifact. The one thing it may
// still decide is a refusal that is safe when stale in the OLD direction — see
// deployarchive.LedgerFacts.
//
// On the reader pool: it is a read, and routing it through Tx would reserve the
// single writer slot while it scans.
func (db *DB) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	var out []AppliedMigration

	err := db.View(ctx, func(q Querier) error {
		seen, err := readAppliedMigrations(ctx, ReadQueries(q))
		if err != nil {
			return err
		}

		out = make([]AppliedMigration, 0, len(seen))
		for v, rec := range seen {
			out = append(out, AppliedMigration{Version: v, Name: rec.name, Checksum: rec.checksum})
		}

		sortMigrations(out)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RefuseUnknownVersions rejects a migration set carrying a version this binary
// has never heard of.
//
// EXPORTED FOR THE RESTORE PLANNER, and deliberately the SAME rule the migrator
// and the schema verifier apply — written twice, they drift, and the failure
// would be a restore that installs a ledger the control plane then refuses to
// start against.
//
// ITS DIAGNOSTIC IS NOT ErrSchemaBehind, and keeping them apart is the point.
// ErrSchemaBehind means a running plane is holding a ledger that needs
// migrating; this means the thing in front of you was written by a NEWER billet
// and the remedy is a newer binary, not a restart.
// IT IS THE ONE COLD ENTRY POINT INTO THIS RULE, so it asks whether this binary
// can read its own migrations before answering. Everything else that reaches
// refuseUnknownVersions came through openDir, which refuses first.
//
// Without that, a binary whose embedded set failed to load answers this question
// from an EMPTY known set, and both of its answers are wrong: an archive carrying
// migrations is refused as "written by a newer version", which sends an operator
// after a newer binary for a fault in the one they have; and an archive whose
// applied set is empty is ACCEPTED, because the loop has nothing to iterate — the
// restore planner reads that as permission to install a ledger this binary could
// never open.
// IT ANSWERS FROM THE SQLITE TIMELINE AND IS STILL BACKEND-INDEPENDENT, because
// the rule compares VERSIONS and a version is the same identity on every
// timeline. The cold caller has an archive and no open ledger, so it cannot know
// which engine wrote the set it is asking about — asking a question only
// versions can answer is what makes that survivable.
func RefuseUnknownVersions(applied []AppliedMigration) error {
	if err := sqliteTimeline.require(); err != nil {
		return err
	}

	seen := make(map[int]appliedMigration, len(applied))
	for _, m := range applied {
		seen[m.Version] = appliedMigration{name: m.Name, checksum: m.Checksum}
	}

	return sqliteTimeline.refuseUnknownVersions(seen)
}

// knownMigrations is the migration set this binary carries.
//
// Unexported: nothing outside this package needs it, and the one reader is a
// test that asserts a snapshot's schema is this binary's. THE SQLITE TIMELINE
// specifically, because a snapshot is a VACUUM INTO of a SQLite file and the
// checksums it carries are over SQLite's own statement bytes.
func knownMigrations() []AppliedMigration {
	out := make([]AppliedMigration, 0, len(sqliteTimeline.migrations))
	for _, m := range sqliteTimeline.migrations {
		out = append(out, AppliedMigration{Version: m.Version, Name: m.Name, Checksum: m.checksum()})
	}

	sortMigrations(out)

	return out
}

// sortMigrations orders by version, so two readings of one ledger compare and
// serialize identically.
func sortMigrations(in []AppliedMigration) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].Version > in[j].Version; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

// requireRegularFile refuses a path that is not one, WITHOUT following a
// symlink to find out.
//
// Everything in this file is asked about a path an operator named or an archive
// declared, and the answer decides whether a credential-bearing directory is
// written to. A name checked is not a name opened, so the callers that go on to
// open use O_NOFOLLOW; this is the check for the ones that hand the path to
// SQLite, which does not.
func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("state: inspect %s: %w", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state: %s is a symlink; billet reads a ledger only from the path "+
			"it was given", path)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("state: %s is not a regular file", path)
	}

	return nil
}

func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("state: open %s to sync it: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", path, err)
	}

	return nil
}

// fsyncDir persists the directory ENTRY, not just the file contents. Syncing a
// new file makes its bytes durable; the name that finds it is a separate write,
// and the state package has been bitten by exactly that gap before — see
// writeDeploymentID.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: open %s to sync it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", dir, err)
	}

	return nil
}

// LatestSchemaVersion is the highest migration this build knows.
//
// PUBLISHED IN THE RELEASE MANIFEST, which is the reason it is exported. A
// candidate release carries this number so an updater can refuse, BEFORE it stops
// anything, a binary that would inherit a ledger it cannot open: migrations are
// append-only and `migrate` refuses a database carrying a version it has never
// heard of, so a release behind the installed schema starts, refuses, and leaves
// the control plane down with a database no installed binary can read.
//
// DERIVED FROM THE MIGRATION LIST rather than written down beside it. A constant
// somebody has to remember to bump is a constant that is wrong exactly once —
// on the release that adds a migration, which is the only release where the
// number matters.
//
// ONE NUMBER DESCRIBES THE BINARY, NOT THE DEPLOYMENT'S BACKEND, which is why
// this reads a timeline rather than taking one. Two binaries compare this across
// an upgrade and neither knows what backend the other was configured for, so a
// backend-dependent answer would make the fence compare two different scales.
// Every timeline declares the same versions, so the SQLite one is read here as
// the canonical numbering rather than as a statement about storage.
func LatestSchemaVersion() int { return sqliteTimeline.latest() }
