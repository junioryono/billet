package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// loadedMigrations is the migration set, with the guard every test that iterates
// it needs.
//
// A LOOP OVER AN EMPTY SLICE PASSES. Parsing stores its error rather than failing
// construction (see timeline.loadErr), which is what lets a guard name a
// collision instead of every test in the package dying for a reason none of them
// states — and it is also what makes an unguarded range over a timeline's
// migrations vacuous exactly when something is wrong.
//
// FOUND BY MUTATION, not by reading: making the parser keep the trailing newline
// before a close marker turned 104 tests in this package red and left
// TestNoMigrationStatementHasStrayWhitespace green, because checkStatement had
// refused the whole set and there was nothing left to iterate.
func loadedMigrations(t *testing.T) []migration {
	t.Helper()

	return loadedTimeline(t, sqliteTimeline)
}

// loadedTimeline is the same question asked of a NAMED timeline, so a test that
// covers both engines cannot accidentally assert twice about one of them.
func loadedTimeline(t *testing.T, tl *timeline) []migration {
	t.Helper()

	if tl.loadErr != nil {
		t.Fatalf("the embedded %s migration set did not load, so this test checked nothing: %v",
			tl.engine, tl.loadErr)
	}

	if len(tl.migrations) == 0 {
		t.Fatalf("the embedded %s migration set is empty, so this test checked nothing", tl.engine)
	}

	return tl.migrations
}

// migrationByVersion finds one migration by the only identity it has.
//
// A test that reached for a Go variable name could not survive the migrations
// becoming files, and a test that reached for a filename could not survive one
// being renamed. The version is what the ledger records.
func migrationByVersion(t *testing.T, version int) migration {
	t.Helper()

	set := loadedMigrations(t)

	i := slices.IndexFunc(set, func(m migration) bool { return m.Version == version })
	if i < 0 {
		t.Fatalf("no migration with version %d", version)
	}

	return set[i]
}

// THE EMBED PATTERN IS A THIRD THING THAT CAN BE WRONG, and its failure is
// silence: a migration whose file the pattern does not match simply never runs,
// and every other test in this package passes because the schema it expects was
// built by the migrations that did.
//
// So the directory on disk is compared against what the binary carries, rather
// than the binary being trusted to have picked everything up.
func TestEveryMigrationFileIsEmbedded(t *testing.T) {
	t.Parallel()

	onDisk, err := os.ReadDir(migrationDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationDir, err)
	}

	var want []string

	for _, e := range onDisk {
		if e.IsDir() {
			t.Errorf("%s/%s is a directory; the migration directory holds .sql files only",
				migrationDir, e.Name())

			continue
		}

		// The one file here that is deliberately not a migration. Allowlisted by
		// name rather than by "anything that is not .sql", because the failure
		// this test exists for is a file that LOOKS like a migration and is
		// invisible to the embed pattern — 0043_x.SQL, 0043_x.sql.bak, an
		// editor's 0043_x.sql~. Each of those would simply never run.
		if e.Name() == "README.md" {
			continue
		}

		if !strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("%s/%s is in the migration directory and go:embed skips it silently; "+
				"if it is a migration, name it <version>_<name>.sql, and if it is not, it "+
				"does not belong here", migrationDir, e.Name())

			continue
		}

		want = append(want, e.Name())
	}

	embedded, err := migrationFS.ReadDir(migrationDir)
	if err != nil {
		t.Fatalf("read the embedded %s: %v", migrationDir, err)
	}

	var got []string
	for _, e := range embedded {
		got = append(got, e.Name())
	}

	slices.Sort(want)
	slices.Sort(got)

	if !slices.Equal(want, got) {
		t.Errorf("the embedded migration set is not the directory:\n  on disk:  %v\n  embedded: %v",
			want, got)
	}

	// GUARDED RATHER THAN FATAL, and deliberately last: a file the embed pattern
	// missed is one of the reasons the set would fail to load, so the comparison
	// above has to run either way. Only this count depends on a parsed set.
	if sqliteTimeline.loadErr != nil {
		t.Errorf("the embedded migration set did not load: %v", sqliteTimeline.loadErr)
	} else if len(got) != len(sqliteTimeline.migrations) {
		t.Errorf("%d files embedded but %d migrations parsed", len(got), len(sqliteTimeline.migrations))
	}
}

// The filename is the only source of a migration's version and name, so the file
// this binary applies as version N has to be the file an operator reading the
// directory would call version N.
func TestMigrationFilesParseToTheirDeclaredVersionAndName(t *testing.T) {
	t.Parallel()

	for _, m := range loadedMigrations(t) {
		file := fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)

		if _, err := migrationFS.ReadFile(migrationDir + "/" + file); err != nil {
			t.Errorf("migration %d (%s) does not live in %s: %v", m.Version, m.Name, file, err)
		}
	}
}

// PINS A PROPERTY OTHER TESTS ALREADY DEPEND ON WITHOUT SAYING SO.
// TestRebuildingMigrationsCopyEveryColumn and indexNamesInMigrations both use
// strings.HasPrefix(stmt, "CREATE TABLE ") — a statement with one leading space
// would make both of them examine nothing and pass.
//
// Trailing whitespace is refused for a different reason: no published statement
// has any, so an editor configured to trim on save is harmless today. One added
// later would make that editor a way to change the published bytes.
func TestNoMigrationStatementHasStrayWhitespace(t *testing.T) {
	t.Parallel()

	for _, m := range loadedMigrations(t) {
		for i, stmt := range m.Stmts {
			if stmt != strings.TrimSpace(stmt) {
				t.Errorf("migration %d (%s) statement %d begins or ends with whitespace: %q",
					m.Version, m.Name, i, stmt)
			}

			for n, line := range strings.Split(stmt, "\n") {
				if strings.TrimRight(line, " \t") != line {
					t.Errorf("migration %d (%s) statement %d line %d has trailing whitespace: %q\n"+
						"An editor that trims on save would change the published bytes.",
						m.Version, m.Name, i, n+1, line)
				}
			}
		}
	}
}

// The round trip, at unit scale: interior tabs are kept, a `--` line INSIDE a
// statement is part of the statement rather than a comment, prose outside the
// markers is ignored, and there is no trailing newline.
func TestTheParserReturnsExactlyTheBytesBetweenTheMarkers(t *testing.T) {
	t.Parallel()

	body := "-- prose above\n--\n\n" +
		"-- +billet:statement\n" +
		"CREATE TABLE t (\n\t\t\ta TEXT,\n\t\t\t-- a comment that is part of the statement\n\t\t\tb TEXT\n\t\t) STRICT\n" +
		"-- +billet:end\n" +
		"\n-- prose between\n" +
		"-- +billet:statement\n" +
		"CREATE INDEX t_a ON t(a)\n" +
		"-- +billet:end\n"

	got, err := parseMigrationStatements("0001_t.sql", []byte(body))
	if err != nil {
		t.Fatalf("parseMigrationStatements: %v", err)
	}

	want := []string{
		"CREATE TABLE t (\n\t\t\ta TEXT,\n\t\t\t-- a comment that is part of the statement\n\t\t\tb TEXT\n\t\t) STRICT",
		"CREATE INDEX t_a ON t(a)",
	}

	if !slices.Equal(got, want) {
		t.Errorf("statements =\n%q\nwant\n%q", got, want)
	}
}

// Each case is a state that would otherwise be a statement which silently never
// runs, or bytes that silently differ. The assertion is the SPECIFIC diagnostic:
// "an error came back" agrees with a parser that refuses everything.
func TestTheMigrationParserRefusesWhatItCannotReadUnambiguously(t *testing.T) {
	t.Parallel()

	const ok = "-- +billet:statement\nSELECT 1\n-- +billet:end\n"

	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "two files claim one version",
			files: map[string]string{"0001_a.sql": ok, "0001_b.sql": ok},
			want:  "both claim version 1",
		},
		{
			name:  "no version in the filename",
			files: map[string]string{"nodes.sql": ok},
			want:  "is not <zero-padded version>",
		},
		{
			name:  "version is not zero padded to four",
			files: map[string]string{"001_a.sql": ok},
			want:  "is not <zero-padded version>",
		},
		{
			name:  "name is not lower snake case",
			files: map[string]string{"0001_Nodes.sql": ok},
			want:  "is not <zero-padded version>",
		},
		{
			name:  "name has a doubled underscore",
			files: map[string]string{"0001_a__b.sql": ok},
			want:  "is not <zero-padded version>",
		},
		{
			name:  "version zero",
			files: map[string]string{"0000_a.sql": ok},
			want:  "a version is a positive identity",
		},
		{
			name:  "a carriage return",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\r\nSELECT 1\n-- +billet:end\n"},
			want:  "carriage return",
		},
		{
			name:  "sql outside a statement",
			files: map[string]string{"0001_a.sql": ok + "SELECT 2\n"},
			want:  "outside any statement and is not a comment",
		},
		{
			name:  "the open marker is indented",
			files: map[string]string{"0001_a.sql": " -- +billet:statement\nSELECT 1\n-- +billet:end\n"},
			want:  "a marker must be written exactly",
		},
		{
			name:  "the close marker has trailing space",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\nSELECT 1\n-- +billet:end \n"},
			want:  "a marker must be written exactly",
		},
		{
			name:  "a statement is never closed",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\nSELECT 1\n"},
			want:  "ends with a statement still open",
		},
		{
			name:  "a statement opens inside a statement",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\n-- +billet:statement\nSELECT 1\n-- +billet:end\n"},
			want:  "opens a statement while one is already open",
		},
		{
			name:  "a close with no open",
			files: map[string]string{"0001_a.sql": "-- +billet:end\n" + ok},
			want:  "closes a statement that was never opened",
		},
		{
			name:  "an empty statement",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\n-- +billet:end\n"},
			want:  "has an empty statement",
		},
		{
			name:  "a statement starting with a blank line",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\n\nSELECT 1\n-- +billet:end\n"},
			want:  "begins or ends with whitespace",
		},
		{
			name:  "a statement ending with a blank line",
			files: map[string]string{"0001_a.sql": "-- +billet:statement\nSELECT 1\n\n-- +billet:end\n"},
			want:  "begins or ends with whitespace",
		},
		{
			name:  "a file with prose and no statements",
			files: map[string]string{"0001_a.sql": "-- just prose\n"},
			want:  "contains no statements",
		},
		{
			name:  "nothing at all",
			files: map[string]string{},
			want:  "no migrations were found",
		},
		{
			name:  "a directory where a migration should be",
			files: map[string]string{"0001_a.sql/inner.sql": ok},
			want:  "is a directory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{}
			for name, body := range tc.files {
				fsys[migrationDir+"/"+name] = &fstest.MapFile{Data: []byte(body)}
			}

			// A MapFS materialises a directory only as the prefix of a file, so
			// with no files there is no migrations/ at all and ReadDir reports a
			// missing path rather than an empty directory. Declare it, so the
			// "nothing at all" case reaches the empty-set refusal.
			if len(tc.files) == 0 {
				fsys[migrationDir] = &fstest.MapFile{Mode: fs.ModeDir}
			}

			got, err := parseMigrations(fsys, migrationDir)
			if err == nil {
				t.Fatalf("parseMigrations accepted %s and returned %d migrations", tc.name, len(got))
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain %q", err, tc.want)
			}
		})
	}
}

// TWO TESTS RATHER THAN ONE, AND THAT SPLIT IS THE POINT.
//
// One fixture was doing both jobs and each weakened the other. Seeding EVERY
// published migration proves every published identity is still accepted and
// leaves Open nothing to do; holding one back makes the upgrade real and stops
// the newest published checksum from ever being presented to Open as an existing
// row. A single test cannot have both, and while it tried, its "Open refused"
// diagnostic blamed the extraction for what could equally be the held-back
// migration failing to apply — a remedy that would not help.
//
// So: TestALedgerCarryingEveryPublishedChecksumOpens is the compatibility half and
// keeps the extraction diagnostic. TestTheNewestMigrationUpgradesAPopulatedLedger
// is the upgrade half and has its own.

// THE COMPATIBILITY HALF, and the one test in this package that can see the
// failure the extraction risked.
//
// Every other migration test computes both sides with this binary's own checksum
// function, so a change to the checksum SCHEME — or to a published statement's
// bytes — is invisible to them: a fresh database records whatever the binary now
// produces and agrees with itself. This one records the sums that were PUBLISHED,
// as literals, the way an earlier release did, and asks the production Open path
// to accept them. Migrations are applied through the same statements, so what is
// under test is the identity of the bytes rather than the SQL.
func TestALedgerCarryingEveryPublishedChecksumOpens(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	const appliedAt = "2020-01-01T00:00:00Z"

	// 0 = hold nothing back. Open has no migration to apply, which is exactly
	// what this half wants: every row it sees was written with a published sum.
	seeded := writePublishedLedger(t, dir, appliedAt, 0)

	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open refused a ledger written with the published checksums: %v\n\n"+
			"The extraction changed a published statement's bytes. Revert it — do NOT "+
			"update migrationsAreFrozen, whose sums are what every deployment recorded.", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	recorded := readBookkeeping(t, ctx, db)

	if len(recorded) != len(migrationsAreFrozen) {
		t.Errorf("%d migrations recorded, want the %d published", len(recorded),
			len(migrationsAreFrozen))
	}

	for version, got := range recorded {
		want, published := migrationsAreFrozen[version]
		if !published {
			t.Errorf("migration %d is recorded but not published", version)
			continue
		}

		if !seeded[version] {
			t.Errorf("migration %d was not seeded, so this half of the fixture is not "+
				"the complete published ledger it claims to be", version)

			continue
		}

		if got.checksum != want.Sum {
			t.Errorf("migration %d (%s) now records %s, published %s",
				version, got.name, got.checksum, want.Sum)
		}

		// THE NAME IS HALF THE IDENTITY, and the seeded ledger carries the
		// published one — so this asserts migrate left the row alone rather than
		// rewriting it from the file's current name.
		if got.name != want.Name {
			t.Errorf("migration %d records the name %q, published %q",
				version, got.name, want.Name)
		}

		// THE BOOKKEEPING ROW WAS NOT REWRITTEN, which is exactly and only what
		// this asserts. It is NOT proof that nothing was re-applied: what Open
		// succeeding rules out is a replay that hits non-idempotent DDL — a
		// re-run CREATE TABLE or table rebuild — and a selective replay of an
		// idempotent statement would leave both this timestamp and the two seeded
		// rows alone. Statement effects are covered by
		// TestTheNewestMigrationUpgradesAPopulatedLedger, which compares against a
		// ledger whose newest migration's statements were applied DIRECTLY rather
		// than by migrate — not one migrate never opened, since it opens both.
		// What this does catch is a migrator that
		// recorded with REPLACE instead of INSERT.
		if got.appliedAt != appliedAt {
			t.Errorf("migration %d's bookkeeping was rewritten: applied_at is %q, want the "+
				"seeded %q", version, got.appliedAt, appliedAt)
		}
	}

	requireSeededRowsSurvive(t, ctx, db)
}

// THE UPGRADE HALF: the newest published migration applied to a POPULATED ledger
// standing one migration behind it.
//
// The compatibility half above deliberately leaves Open nothing to do, so this is
// the only test that drives migrate over rows it did not create. It needs no
// hand-maintained version number — the newest published version comes off
// migrationsAreFrozen.
//
// TWO DATABASES DIFFERING ONLY IN HOW THE LAST MIGRATION WAS APPLIED. Both are
// seeded identically through N-1, with the same rows; then migrate applies N to
// one and a raw ExecContext applies it to the other. Everything observable about
// them afterwards must be equal, so a defect in migrate has nowhere to hide.
//
// THAT SHAPE TOOK THREE ATTEMPTS AND EACH FAILURE IS WORTH KEEPING:
//
//   - Asserting the bookkeeping row alone passes a migrator that records the row
//     and skips the SQL. The seeded rows live in tables that already exist one
//     migration back, so they cannot see it either.
//   - Comparing against a FRESH Open does not catch that — measured, the mutant
//     survived. A fresh database is built by the same migrate, so skipping a
//     statement affects both sides equally and they agree.
//   - Comparing sqlite_master against a raw-applied reference catches a
//     STRUCTURAL migration and not a DATA-ONLY one: if the reference is seeded
//     through N and only then given rows, migration N's UPDATE has nothing to act
//     on there either, both schemas match, and the mutant survives again. Hence
//     seeding both through N-1 and comparing table CONTENTS as well.
//
// WHAT IS LEFT is stated rather than claimed away: a migration whose only effect
// is on rows that neither database happens to hold is unobservable here. Nothing
// short of knowing what that migration does can close it, so a migration written
// to repair specific data wants its own test — migration 7's is
// TestMacOSLeasesAreBackfilledFromTheirSlot.
func TestTheNewestMigrationUpgradesAPopulatedLedger(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	const appliedAt = "2020-01-01T00:00:00Z"

	holdBack := newestPublishedVersion(t)

	seeded := writePublishedLedger(t, dir, appliedAt, holdBack)

	if seeded[holdBack] {
		t.Fatalf("migration %d was seeded, so there is no upgrade to observe", holdBack)
	}

	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("migration %d failed to upgrade a populated ledger standing one migration "+
			"behind it: %v\n\nThis is the newest migration applied over rows it did not "+
			"create. If the extraction is at fault, "+
			"TestALedgerCarryingEveryPublishedChecksumOpens says so instead.",
			holdBack, err)
	}

	t.Cleanup(func() { _ = db.Close() })

	recorded := readBookkeeping(t, ctx, db)

	if len(recorded) != len(sqliteTimeline.migrations) {
		t.Errorf("%d migrations recorded, want the %d this build carries",
			len(recorded), len(sqliteTimeline.migrations))
	}

	// EXACTLY ONE ROW MAY BE NEW, and it must be the one held back. An earlier
	// version treated every unseeded version as "held back", which would have
	// silently absorbed a second unapplied migration.
	for version, got := range recorded {
		switch {
		case version == holdBack:
			if got.appliedAt == appliedAt {
				t.Errorf("migration %d carries the seeded timestamp, so this Open never "+
					"applied it", version)
			}

			// APPLIED BY THIS BINARY, so the sum it recorded is the binary's own —
			// and it still has to equal what was published.
			if want := migrationsAreFrozen[version]; got.checksum != want.Sum ||
				got.name != want.Name {
				t.Errorf("migration %d applied as (%s, %s), published (%s, %s)",
					version, got.name, got.checksum, want.Name, want.Sum)
			}
		case !seeded[version]:
			t.Errorf("migration %d was neither seeded nor the held-back %d, so more than "+
				"one migration was applied and this test cannot say which did what",
				version, holdBack)
		case got.appliedAt != appliedAt:
			t.Errorf("migration %d was seeded but its applied_at is %q, want %q",
				version, got.appliedAt, appliedAt)
		}
	}

	requireSeededRowsSurvive(t, ctx, db)

	// THE REFERENCE IS SEEDED IDENTICALLY THROUGH N-1, ROWS INCLUDED, and only then
	// given migration N by a raw ExecContext. That is what makes migrate the sole
	// difference between the two databases — and what makes a data-only migration
	// observable, since its statements act on the same rows in both.
	refDir := t.TempDir()

	writePublishedLedger(t, refDir, appliedAt, holdBack)
	applyMigrationDirectly(t, refDir, holdBack, appliedAt)

	reference, err := Open(ctx, refDir)
	if err != nil {
		t.Fatalf("Open the reference ledger: %v", err)
	}

	t.Cleanup(func() { _ = reference.Close() })

	requireSameSchema(t, ctx, db, reference, holdBack)
	requireSameContents(t, ctx, db, reference)
}

// applyMigrationDirectly runs one migration's statements against a ledger with a
// raw handle and records it under its PUBLISHED identity.
//
// THE POINT IS THAT migrate DID NOT APPLY THEM. It is the reference arm of the
// upgrade test: whatever migrate does to the other database, this one had the same
// statements run over the same rows without migrate deciding to run them.
//
// The caller still OPENS this ledger afterwards, so migrate does look at it — and
// finds every version recorded with a matching checksum, so it applies nothing. A
// migrator that replayed anyway would show up as a difference between the two,
// which is what the comparison is for.
func applyMigrationDirectly(t *testing.T, dir string, version int, appliedAt string) {
	t.Helper()

	m := migrationByVersion(t, version)

	want, published := migrationsAreFrozen[version]
	if !published {
		t.Fatalf("migration %d is not published, so there is no identity to record it under",
			version)
	}

	raw, err := sql.Open("sqlite", dsnWith(filepath.Join(dir, "billet.db"),
		map[string]string{"_txlock": "immediate"},
		"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("open a raw ledger: %v", err)
	}

	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the raw ledger: %v", err)
		}
	}()

	ctx := t.Context()

	for _, stmt := range m.Stmts {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("apply migration %d (%s) directly: %v", m.Version, m.Name, err)
		}
	}

	if _, err := raw.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		version, want.Name, want.Sum, appliedAt); err != nil {
		t.Fatalf("record migration %d: %v", version, err)
	}
}

// requireSameSchema compares every declared object in two ledgers.
func requireSameSchema(t *testing.T, ctx context.Context, got, want *DB, applied int) {
	t.Helper()

	upgraded := schemaObjects(t, ctx, got)
	direct := schemaObjects(t, ctx, want)

	if len(direct) == 0 {
		t.Fatal("the reference ledger declares no schema objects, so this comparison " +
			"checked nothing")
	}

	for name, ddl := range direct {
		have, ok := upgraded[name]

		switch {
		case !ok:
			t.Errorf("the upgraded ledger has no %q, so migration %d's statements did not "+
				"take effect", name, applied)
		case have != ddl:
			t.Errorf("the upgraded ledger's %q is\n%s\nand a directly applied one's is\n%s",
				name, have, ddl)
		}
	}

	for name := range upgraded {
		if _, ok := direct[name]; !ok {
			t.Errorf("the upgraded ledger has %q and a directly applied one does not", name)
		}
	}
}

// requireSameContents compares every user table's rows in two ledgers.
//
// SCHEMA IS NOT ENOUGH. A migration whose statements are UPDATEs changes no
// declared object, so a migrate that skipped them would leave two identical
// schemas — which is exactly how the previous version of this comparison passed a
// mutant. Comparing rows is what makes a data-only migration observable.
//
// schema_migrations IS EXCLUDED, and deliberately: applied_at legitimately differs
// between the two arms, which is the very discriminator the caller uses to prove
// migrate did the work. Its rows are asserted by the caller instead.
func requireSameContents(t *testing.T, ctx context.Context, got, want *DB) {
	t.Helper()

	upgraded := tableContents(t, ctx, got)
	direct := tableContents(t, ctx, want)

	if len(direct) == 0 {
		t.Fatal("the reference ledger has no user tables, so this comparison checked nothing")
	}

	for table, rows := range direct {
		have, ok := upgraded[table]

		switch {
		case !ok:
			t.Errorf("the upgraded ledger has no table %q", table)
		case !slices.Equal(have, rows):
			t.Errorf("table %q differs after the upgrade:\n  through migrate: %v\n"+
				"  applied directly: %v", table, have, rows)
		}
	}

	// BOTH DIRECTIONS. requireSameSchema would also catch a target-only table,
	// since a table is a declared object — but that is a reasoning dependency
	// between two helpers, and iterating one map is exactly the shape that has made
	// three tests in this file vacuous already. Cheaper to ask.
	for table := range upgraded {
		if _, ok := direct[table]; !ok {
			t.Errorf("the upgraded ledger has table %q and a directly applied one does not",
				table)
		}
	}
}

// renderCell writes one value with its storage class, so two values SQLite stores
// differently can never render the same.
//
// A TYPE SWITCH RATHER THAN %v, because %v on an int64 and on a string that holds
// its digits produce identical text — which is the collision this exists to
// remove.
//
// THE CASES ARE MEASURED, not guessed, because what a driver hands back is not
// billet's to assume. Probed against modernc.org/sqlite with a STRICT table:
// TEXT -> string, INTEGER -> int64, REAL -> float64, BLOB -> []uint8, NULL -> nil.
// TEXT arriving as a string is the one worth pinning — as []byte it would render
// `blob:…` and the tag would be actively misleading rather than merely coarse.
//
// AN UNHANDLED TYPE FAILS THE TEST, and it takes a *testing.T for that alone. The
// previous version returned `%T:%v` under a comment calling that a loud failure,
// which it was not — it was silence.
//
// WHAT AN UNHANDLED TYPE COSTS is not that different values compare equal — a
// `%T:%v` fallback does distinguish most of them. It is that nothing has shown the
// fallback to be injective or to name the storage class truthfully, and the
// comparison in rowsOf rests on both. So it reports rather than guessing, and the
// five cases below are exactly the ones
// TestTheDriverReturnsOneGoTypePerStorageClass drives against a real database.
//
// THERE IS NO bool CASE, deliberately. SQLite has no Boolean storage class, so a
// bool cannot arrive from these tables — and a branch the pinning test cannot
// exercise is a branch whose rendering nothing has checked. It falls to the
// default, which says so.
func renderCell(t *testing.T, v any) string {
	t.Helper()

	switch c := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("int:%d", c)
	case float64:
		return fmt.Sprintf("real:%v", c)
	case string:
		return fmt.Sprintf("text:%q", c)
	case []byte:
		return fmt.Sprintf("blob:%x", c)
	default:
		t.Errorf("the driver returned a %T (%v), which renderCell does not tag; nothing has "+
			"shown that rendering to be collision-free or to name the storage class "+
			"correctly, and the comparison in rowsOf depends on both", c, c)

		return fmt.Sprintf("untagged(%T):%v", c, c)
	}
}

// tableContents is every row of every user table, rendered as strings. rowsOf
// does the rendering and says why.
func tableContents(t *testing.T, ctx context.Context, db *DB) map[string][]string {
	t.Helper()

	// PRODUCTION'S OWN LIST, not a second copy of the question. `userTables` in
	// fence.go is what the maintenance fence asks when it needs to know what a
	// deployment could have written to; a test that re-derived it could disagree
	// with it about a table and prove the wrong thing.
	tables, err := userTables(ctx, db.Reader())
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	out := make(map[string][]string, len(tables))

	for _, table := range tables {
		// EXCLUDED HERE RATHER THAN IN THE QUERY, because the caller's whole
		// discriminator is that applied_at legitimately differs between the two
		// arms. Its rows are asserted separately.
		if table == "schema_migrations" {
			continue
		}

		out[table] = rowsOf(t, ctx, db, table)
	}

	return out
}

// rowsOf renders one table's rows, sorted.
//
// SORTED, because row order is not part of a table's contents and a rebuild
// migration legitimately changes it.
//
// EVERY CELL CARRIES ITS STORAGE CLASS, and that is not decoration. Scanning into
// sql.NullString coerces INTEGER 1, REAL 1.0, TEXT "1" and the blob 0x31 to the
// same three characters, so a migration that changed a value's storage class could
// be skipped and both sides would still compare equal. Every table is STRICT
// today, which makes that unreachable — and "unreachable because of a rule in
// another file" is exactly the reasoning dependency this file has already been
// wrong about three times, so the tag closes it instead.
//
// NULL is its own tag and every text value is quoted, so the two cannot collide
// the way a bare rendering let the text "NULL" pass for SQL NULL.
func rowsOf(t *testing.T, ctx context.Context, db *DB, table string) []string {
	t.Helper()

	// THE NAME COMES FROM sqlite_master, not from a caller, so there is nothing
	// here for a placeholder to protect against — and SQLite does not accept one
	// for an identifier anyway. Quoted, with an embedded quote doubled, exactly as
	// PeekLedger does it one file over.
	rows, err := db.Reader().QueryContext(ctx,
		`SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}

	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}

	var out []string

	for rows.Next() {
		cells := make([]any, len(cols))
		into := make([]any, len(cols))

		for i := range cells {
			into[i] = &cells[i]
		}

		if err := rows.Scan(into...); err != nil {
			t.Fatalf("scan a row of %s: %v", table, err)
		}

		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = cols[i] + "=" + renderCell(t, c)
		}

		out = append(out, strings.Join(parts, " "))
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}

	slices.Sort(out)

	return out
}

// bookkeepingRow is one schema_migrations row as the tests read it.
type bookkeepingRow struct {
	name      string
	checksum  string
	appliedAt string
}

func readBookkeeping(t *testing.T, ctx context.Context, db *DB) map[int]bookkeepingRow {
	t.Helper()

	rows, err := db.Reader().QueryContext(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}

	defer rows.Close()

	out := map[int]bookkeepingRow{}

	for rows.Next() {
		var (
			version int
			r       bookkeepingRow
		)

		if err := rows.Scan(&version, &r.name, &r.checksum, &r.appliedAt); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}

		out[version] = r
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}

	return out
}

// schemaObjects is every table, index and view the database declares, keyed by
// name, with the DDL SQLite kept for it.
//
// A MAP RATHER THAN A LIST, because sqlite_master's row order is not part of the
// schema and comparing it would make this fail for a reason nobody cares about.
// The DDL text is, and it is what a rebuild migration changes.
//
// KEYED ON type AND name TOGETHER, since an index and a table may share neither
// namespace nor a name safely across SQLite versions.
//
// sqlite_autoindex_* ROWS CARRY A NULL sql, which COALESCE turns into "" — so for
// those entries what is compared is their PRESENCE, not their text. That is the
// right signal: an autoindex exists exactly because a UNIQUE or PRIMARY KEY
// constraint does, so losing one means losing the constraint.
func schemaObjects(t *testing.T, ctx context.Context, db *DB) map[string]string {
	t.Helper()

	rows, err := db.Reader().QueryContext(ctx,
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}

	defer rows.Close()

	out := map[string]string{}

	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}

		out[kind+" "+name] = ddl
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}

	return out
}

// requireSeededRowsSurvive asserts the rows writePublishedLedger wrote are still
// there.
//
// `leases` is the table migrations 16 and 20 rebuild by copying, so a migration
// replayed over a populated ledger is exactly how a deployment's capacity record
// would go missing.
func requireSeededRowsSurvive(t *testing.T, ctx context.Context, db *DB) {
	t.Helper()

	for _, q := range []struct {
		what, sql string
	}{
		{"the seeded node", `SELECT COUNT(*) FROM nodes WHERE name = 'epyc-1'`},
		{"the seeded lease", `SELECT COUNT(*) FROM leases WHERE id = 'lease-seed'`},
	} {
		var n int
		if err := db.Reader().QueryRowContext(ctx, q.sql).Scan(&n); err != nil {
			t.Errorf("count %s: %v", q.what, err)
			continue
		}

		if n != 1 {
			t.Errorf("%s did not survive: found %d rows, want 1", q.what, n)
		}
	}
}

// newestPublishedVersion is the highest version migrationsAreFrozen carries.
//
// IT IS NOT A RELEASE BOUNDARY, and calling it one was wrong. The frozen table
// records published IDENTITIES, not which release shipped which: one release may
// carry several migrations and several releases may share one schema. So holding
// this version back produces the ledger ONE MIGRATION BACK, which is all the
// upgrade test needs and all this can honestly say.
func newestPublishedVersion(t *testing.T) int {
	t.Helper()

	highest := 0
	for version := range migrationsAreFrozen {
		if version > highest {
			highest = version
		}
	}

	if highest == 0 {
		t.Fatal("no migration is published, so there is no earlier ledger to imitate")
	}

	return highest
}

// writePublishedLedger builds a database carrying PUBLISHED migrations only,
// applied through the same statements and recorded against the published
// checksums rather than against anything this binary computes. It returns the
// versions it seeded.
//
// It deliberately does not call migration.checksum(). Using it here is what would
// make the caller agree with any change to the checksum scheme, which is the one
// failure no other test in this package can see.
//
// holdBack names one published version to LEAVE OUT, so the caller's Open has a
// real migration to apply; 0 leaves nothing out, which gives the complete
// published ledger. Both callers exist because one fixture cannot be both — see
// the comment above TestALedgerCarryingEveryPublishedChecksumOpens.
func writePublishedLedger(t *testing.T, dir, appliedAt string, holdBack int) map[int]bool {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	raw, err := sql.Open("sqlite", dsnWith(filepath.Join(dir, "billet.db"),
		map[string]string{"_txlock": "immediate"},
		"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"))
	if err != nil {
		t.Fatalf("open a raw ledger: %v", err)
	}

	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the raw ledger: %v", err)
		}
	}()

	ctx := t.Context()

	if _, err := raw.ExecContext(ctx, bootstrapSchemaMigrations); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}

	set := loadedMigrations(t)

	// THE VERSION TO HOLD BACK MUST EXIST. If it names a published version that no
	// file carries, nothing gets skipped, the seeded count still comes out right,
	// and the caller reports "no upgrade to observe" — true, and about the wrong
	// cause. Named here, where the cause is.
	if holdBack != 0 &&
		!slices.ContainsFunc(set, func(m migration) bool { return m.Version == holdBack }) {
		t.Fatalf("asked to hold back version %d and no file carries it", holdBack)
	}

	if holdBack != 0 {
		if _, published := migrationsAreFrozen[holdBack]; !published {
			t.Fatalf("asked to hold back version %d, which is not published, so leaving it "+
				"out does not imitate any release", holdBack)
		}
	}

	seeded := make(map[int]bool, len(migrationsAreFrozen))

	for _, m := range set {
		want, published := migrationsAreFrozen[m.Version]
		if !published || m.Version == holdBack {
			// Either not published at all, or the one deliberately left for the
			// caller's Open to apply.
			continue
		}

		seeded[m.Version] = true

		for _, stmt := range m.Stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("apply migration %d (%s): %v", m.Version, m.Name, err)
			}
		}

		if _, err := raw.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, want.Name, want.Sum, appliedAt); err != nil {
			t.Fatalf("record migration %d: %v", m.Version, err)
		}
	}

	// EVERY PUBLISHED MIGRATION BUT THE HELD-BACK ONE HAD TO BE FOUND IN THE LOADED
	// SET. A published version missing from the files would otherwise produce a
	// shorter ledger here, which the caller's Open then accepts without complaint
	// because that migration is not in the set it applies either — the two
	// shortfalls agree, and the whole test gets quietly weaker. Named here, where
	// the cause is.
	want := len(migrationsAreFrozen)
	if holdBack != 0 {
		want--
	}

	if len(seeded) != want {
		t.Fatalf("seeded %d migrations, want %d; the rest are missing from %s",
			len(seeded), want, migrationDir)
	}

	// ROWS, BECAUSE A LEDGER AN OPERATOR RESTORES HAS ROWS IN IT. `leases` is the
	// table migrations 16 and 20 rebuild by copying, so it is the one where a
	// replayed migration would take a deployment's capacity record with it. The
	// caller asserts both of these survive.
	//
	// THEY MUST BE VALID AT BOTH SHAPES THIS HELPER PRODUCES — the complete
	// published schema and the one a migration short of it — because it has two
	// callers. Only columns from migrations 1 and 2 are named, which is what keeps
	// that true; a migration that constrains `leases` further will need this
	// insert to satisfy it before AND after, and the failure will say so here.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO nodes (name, provider, last_seen_at) VALUES ('epyc-1', 'docker', 't')`,
	); err != nil {
		t.Fatalf("seed a node: %v", err)
	}

	if _, err := raw.ExecContext(ctx,
		`INSERT INTO leases (id, tier, node, phase, vcpu, memory, created_at, heartbeat_at, expires_at)
		 VALUES ('lease-seed', 'linux-4', 'epyc-1', 'online', 4, 8589934592, 't', 't', 't')`,
	); err != nil {
		t.Fatalf("seed a lease: %v", err)
	}

	return seeded
}

// A BINARY THAT CANNOT READ ITS OWN MIGRATIONS MUST NOT TOUCH A LEDGER.
//
// Both branches at the bottom of openDir agree that an empty set is healthy: the
// migrating one applies nothing and reports success, and the verifying one finds
// no migration missing. So the refusal has to sit above them — and above
// MkdirAll, so nothing is left behind either.
//
// NOT t.Parallel(), AND IT MUST NOT BECOME SO. It swaps the package-level
// migration set, which every other test in this file reads. Go defers parallel
// tests until the sequential ones have finished, so the swap and its Cleanup both
// happen while nothing else is running; adding t.Parallel() here would put a real
// data race between this and any parallel neighbour. Same reason openAt in
// state_test.go is sequential.
func TestAMigrationSetThatFailedToLoadRefusesToOpen(t *testing.T) {
	fullSet, fullErr := sqliteTimeline.migrations, sqliteTimeline.loadErr

	t.Cleanup(func() { sqliteTimeline.migrations, sqliteTimeline.loadErr = fullSet, fullErr })

	sqliteTimeline.migrations, sqliteTimeline.loadErr = nil, errors.New("0007_x.sql: pretend the embed broke")

	dir := t.TempDir()

	db, err := Open(t.Context(), dir)
	if err == nil {
		_ = db.Close()

		t.Fatal("Open accepted a binary whose migration set could not be read")
	}

	if !errors.Is(err, errMigrationsUnavailable) {
		t.Errorf("error %q is not errMigrationsUnavailable", err)
	}

	if !strings.Contains(err.Error(), "pretend the embed broke") {
		t.Errorf("error %q does not carry the underlying reason", err)
	}

	for _, name := range []string{"billet.db", "billet.lock"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Errorf("Open created %s before refusing", name)
		}
	}
}

// THE RESTORE PLANNER ASKS THIS RULE COLD, so it must not answer from an empty
// set.
//
// RefuseUnknownVersions is exported for internal/deployarchive, which calls it
// BEFORE anything has opened a ledger — so unlike every other reader of the
// migration set it is not downstream of openDir's gate. With the set unreadable
// its two answers were wrong in opposite directions: an archive carrying
// migrations was refused as "written by a newer version", pointing an operator at
// a newer binary for a fault in the one in their hand, and an archive with an
// EMPTY applied set was accepted, because the loop had nothing to walk.
//
// The second is the dangerous one: the planner reads a nil as permission to
// install a ledger this binary could never open.
//
// NOT t.Parallel(), AND IT MUST NOT BECOME SO — it swaps the package-level
// migration set. See TestAMigrationSetThatFailedToLoadRefusesToOpen.
func TestTheColdVersionRuleRefusesAnUnreadableMigrationSet(t *testing.T) {
	fullSet, fullErr := sqliteTimeline.migrations, sqliteTimeline.loadErr

	t.Cleanup(func() { sqliteTimeline.migrations, sqliteTimeline.loadErr = fullSet, fullErr })

	sqliteTimeline.migrations, sqliteTimeline.loadErr = nil, errors.New("0007_x.sql: pretend the embed broke")

	// THE EMPTY APPLIED SET IS THE CASE THAT USED TO PASS. An archive with rows
	// would have been refused already, for the wrong reason.
	for _, applied := range [][]AppliedMigration{
		nil,
		{{Version: 12, Name: "cert_revocation", Checksum: "x"}},
	} {
		err := RefuseUnknownVersions(applied)
		if err == nil {
			t.Fatalf("RefuseUnknownVersions(%v) accepted an archive while this binary "+
				"cannot read its own migrations", applied)
		}

		if !errors.Is(err, errMigrationsUnavailable) {
			t.Errorf("error %q does not say the migration set is the problem", err)
		}

		if strings.Contains(err.Error(), "newer version") {
			t.Errorf("error %q blames a newer billet for this binary's own broken set", err)
		}
	}
}

// THE VERSIONS ARE 1..N WITH NO GAPS, AND THAT IS WHAT MAKES SEQUENTIAL INTEGERS
// SAFE.
//
// LatestSchemaVersion() is published in the release manifest as a single int, and
// the upgrade fence refuses a candidate whose number is below the installed
// ledger's maximum. ONE NUMBER IS A SOUND PROXY FOR "this binary knows every
// version the ledger applied" ONLY WHILE THE VERSIONS ARE DENSE. With a gap, a
// binary carrying {1..42, 44} publishes 44, an installed ledger holding
// {1..42, 44} reports 44, the fence passes — and a later binary that fills in 43
// applies it AFTER 44 on that deployment and BEFORE 44 on a fresh one. Two
// ledgers, the same recorded versions and checksums, and potentially different
// schemas.
//
// DENSITY IS ALSO WHAT FORCES THE COLLISION. Two branches must both reach for the
// same next integer, so CI names them and the one that has not shipped renumbers.
// A rule of merely "above every published version" does not: two branches take 43
// and 44, both merge, and nothing objects.
//
// ASSERTED HERE RATHER THAN IN THE PARSER, deliberately. A gap is an authoring
// mistake in this repository, and a released binary that refused to start over one
// would take a fleet down for something CI should have caught — the parser reads
// what is there; this decides what may be committed.
//
// The earlier version of this test asked only whether an UNPUBLISHED migration
// sat above the published maximum, which was vacuous twice over: every migration
// in a green tree is published (TestNoShippedMigrationHasBeenEdited requires it),
// so the comparison never ran, and it would have permitted 44-without-43 anyway.
// Measured: a tree with exactly that gap passed both version tests.
func TestMigrationVersionsAreDenseFromOne(t *testing.T) {
	t.Parallel()

	requireDenseFromOne(t, sqliteTimeline, migrationsAreFrozen, "migrationsAreFrozen")
}

// The same rule for the PostgreSQL timeline.
//
// NOT IMPLIED BY THE ONE ABOVE, even though the two timelines are proved to
// declare the same versions: that proof is about the LOADED sets, and half of
// what density asserts is about the published TABLE. A typo in
// pgMigrationsAreFrozen is invisible to every other check here.
func TestPostgresMigrationVersionsAreDenseFromOne(t *testing.T) {
	t.Parallel()

	requireDenseFromOne(t, pgTimeline, pgMigrationsAreFrozen, "pgMigrationsAreFrozen")
}

func requireDenseFromOne(
	t *testing.T, tl *timeline, frozen map[int]struct{ Name, Sum string }, table string,
) {
	t.Helper()

	set := loadedTimeline(t, tl)

	for i, m := range set {
		want := i + 1
		if m.Version != want {
			t.Fatalf("migration %d (%s) is at position %d of the sorted set, so the versions "+
				"are not 1..%d with no gaps — expected version %d here.\n"+
				"Renumber so every integer from 1 to %d is present exactly once: "+
				"LatestSchemaVersion() is one number in the release manifest, and it can only "+
				"stand for \"knows every applied version\" while the set is dense.",
				m.Version, m.Name, i, len(set), want, len(set))
		}
	}

	// AND THE PUBLISHED SET IS THE DENSE PREFIX 1..len(published), so a new
	// migration can only ever be len(published)+1 — which is the collision the
	// scheme wants.
	//
	// ASKED BOTH WAYS ROUND, because one way is not the property. "No key above
	// len(published)" is satisfied by {0, 2, 3, …, N} and by {-1, 2, 3, …, N},
	// which are missing version 1; migrationsAreFrozen is a hand-written map, so
	// nothing else stops a typo like that. Requiring every version from 1 to N to
	// be PRESENT is what actually says prefix.
	published := len(frozen)

	for version := 1; version <= published; version++ {
		if _, ok := frozen[version]; !ok {
			t.Errorf("version %d is missing from %s, which has %d entries — "+
				"so the published set is not the dense prefix 1..%d",
				version, table, published, published)
		}
	}

	for version := range frozen {
		if version < 1 || version > published {
			t.Errorf("version %d is published but outside 1..%d, so the published set is not "+
				"a dense prefix", version, published)
		}
	}

	// A SHORT SET IS DENSE TOO. 1..40 with 42 published passes the loop above, so
	// density alone does not notice a published migration that has gone missing.
	// TestNoShippedMigrationHasBeenEdited names that case better; this is kept so
	// the density claim is not read as covering it.
	if len(set) < published {
		t.Errorf("%d migrations loaded but %d are published; a published version has gone "+
			"missing from %s", len(set), published, tl.dir)
	}
}

// PARSING IS A FUNCTION OF THE BYTES AND NOTHING ELSE.
//
// The checksum is the migration's identity, so anything that made it depend on
// iteration order, a map, or the clock would let two processes disagree about a
// schema they share. Two parses of the same FS have to produce the same versions
// in the same order with the same sums.
func TestParsingTheSameFilesTwiceProducesTheSameIdentities(t *testing.T) {
	t.Parallel()

	set := loadedMigrations(t)

	again, err := parseMigrations(migrationFS, migrationDir)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}

	if len(again) != len(set) {
		t.Fatalf("second parse found %d migrations, first found %d", len(again), len(set))
	}

	for i := range again {
		switch {
		case again[i].Version != set[i].Version:
			t.Errorf("position %d is version %d on a second parse, %d on the first",
				i, again[i].Version, set[i].Version)
		case again[i].checksum() != set[i].checksum():
			t.Errorf("migration %d hashes differently on a second parse", again[i].Version)
		}
	}
}
