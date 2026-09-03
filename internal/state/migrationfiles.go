package state

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// THE MIGRATIONS ARE FILES, AND THE PARSER IS BYTE-EXACT.
//
// They used to be Go composite literals — about twelve hundred lines of raw
// strings inside state.go, appended to a package var by init. That is how one of
// them came to be edited by accident: commit ef84c7b added two explanatory lines
// INSIDE migration 1's CREATE TABLE, the SQL was unchanged in every sense that
// matters to SQLite, and every ledger written before that commit stopped opening.
// A migration's recorded checksum covers the STATEMENT BYTES, comments and
// whitespace included.
//
// SO THE EXTRACTION COULD NOT CHANGE ONE BYTE, and the file format is
// presentation only. What the parser returns is exactly what the Go literals
// held, which is why every recorded checksum in every live deployment stayed
// valid and the tamper detection is untouched. The visible cost is permanent:
// migrations 1-42 carry the tab indentation they inherited from being raw string
// literals nested in a struct literal, and THOSE FILES MAY NEVER BE REFORMATTED.
// migrationsAreFrozen is what enforces that, in CI, rather than on somebody's
// host during an upgrade.
//
// The format:
//
//	-- prose, freely editable, above the first marker and between statements
//	-- +billet:statement
//	<published bytes>
//	-- +billet:end
//
// The markers are billet's own rather than goose's, deliberately. Goose is not
// the authority here — billet's ledger is — and a format a third-party tool
// recognises is a format that tool may decide to normalise, which for bytes a
// running deployment's checksum depends on is a fleet that will not start.
//
// A statement is the lines strictly between the markers, joined with "\n", with
// no trailing newline. Everything the parser cannot read unambiguously is a
// REFUSAL rather than a best effort: the failure mode of guessing here is a
// statement that silently never runs, or bytes that silently differ.

// A GLOB PATTERN DOES NOT APPLY go:embed's DOT-AND-UNDERSCORE EXCLUSION, which
// was measured rather than read: `.hidden.sql` and `_probe.sql` are both embedded
// by `migrations/*.sql`, and both are then refused BY NAME by
// migrationFilePattern. That matters because the exclusion is documented for
// DIRECTORY patterns, and if it applied here such a file would be invisible to
// the binary while sitting in the directory — the silent-omission failure this
// whole file is arranged against. TestEveryMigrationFileIsEmbedded compares the
// directory with the embedded set for the same reason.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// The PostgreSQL timeline, in its own directory rather than nested under the
// SQLite one: parseMigrations refuses a directory entry, because a subdirectory
// inside a migration directory is a set of statements the parser would walk past
// without saying so.
//
//go:embed pgmigrations/*.sql
var pgMigrationFS embed.FS

const (
	migrationDir   = "migrations"
	pgMigrationDir = "pgmigrations"

	// A marker must be written EXACTLY, at the start of its own line. Padding
	// one turns it into an ordinary comment and silently changes the statements
	// on either side, so a near-miss is refused rather than tolerated.
	stmtOpenMarker  = "-- +billet:statement"
	stmtCloseMarker = "-- +billet:end"
)

// migrationFilePattern is the only source of a migration's version and name.
//
// Recorded in ONE place on purpose: a version declared in the filename and again
// inside the file is two facts that can disagree, and the ledger keys on the
// version.
//
// FOUR DIGITS OR MORE, zero-padded, so an ordinary `ls` reads in application
// order. The padding is cosmetic; the version is the parsed integer.
//
// `\d` is RE2's Perl class and is exactly [0-9] — ASCII only — so what it matches
// is always something strconv.Atoi can read. That matters here rather than being
// trivia: a Unicode-digit class would accept a filename whose version parses to
// something else, or to nothing.
var migrationFilePattern = regexp.MustCompile(`^(\d{4,})_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`)

// migration is identified by an explicit, immutable Version. Counting applied
// rows is not a schema version: a deleted row reruns a migration, a forged row
// skips one, and inserting a migration in the middle silently reruns the tail.
// The checksum additionally catches an edited migration, which would otherwise
// leave two deployments believing they share a schema they do not.
type migration struct {
	Version int
	Name    string
	Stmts   []string
}

// checksum hashes the statements, each followed by a separator.
//
// UNCHANGED SINCE THE MIGRATIONS WERE GO LITERALS, and that is the whole reason
// the extraction was possible without a flag day.
//
// THE SEPARATOR IS ABOUT STATEMENT BOUNDARIES, not about ordering. Plain
// concatenation already distinguishes a reorder, because "AB" and "BA" differ —
// what it cannot distinguish is ["ab", "c"] from ["a", "bc"], which are two
// different migrations that would hash identically. The 0 byte cannot appear in
// SQL billet accepts, so it makes the boundaries part of the identity.
func (m migration) checksum() string {
	h := sha256.New()

	for _, s := range m.Stmts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// timeline is ONE ENGINE'S ordered migration set.
//
// THERE IS ONE TIMELINE PER ENGINE BECAUSE DDL IS NOT PORTABLE, and it is the
// only place the two backends carry separate SQL. STRICT, an INTEGER column
// with CHECK (x IN (0,1)) standing in for a boolean, and the table rebuilds that
// exist only to work around SQLite's ALTER limits are all SQLite's own spelling.
// The QUERIES are shared — one directory, one generated package, executed by
// both engines.
//
// EVERY TIMELINE DECLARES THE SAME VERSIONS AND THE SAME NAMES, which is
// load-bearing rather than tidy: LatestSchemaVersion is published in the release
// manifest as a single int and the upgrade fence compares it, so one number has
// to describe the binary whatever backend a deployment runs. Checksums are the
// half that legitimately differs, because they are over one engine's own
// statement bytes.
//
// Migrations are append-only. Never edit or reorder an existing entry; add a new
// one. Rolling a CI control plane backwards is a restore-from-backup operation,
// not a schema operation.
//
// THE LOAD ERROR IS STORED RATHER THAN FATAL. Parsing cannot panic — a control
// plane that panics drops every in-flight lease, and forbidigo bans it — but the
// better reason is that a guard which fails during construction never runs. A
// tree carrying two migrations with the same version must still be able to
// execute TestNoShippedMigrationHasBeenEdited and be TOLD which two collided,
// rather than watching every test in the package fail for a reason none of them
// names.
//
// openDir reports this error once, at the top, before it creates a directory or
// takes a lock. Everything else derived from the set degrades fail-closed on its
// own: an empty set makes refuseUnknownVersions refuse every recorded version,
// and makes LatestSchemaVersion return 0, which releasesource already refuses as
// "the manifest names no ledger schema".
type timeline struct {
	// engine names this set in a diagnostic. A message about a migration that
	// does not say which engine's timeline it came from sends whoever reads it to
	// the wrong directory.
	engine string
	dir    string

	migrations []migration
	loadErr    error
}

// loadTimeline parses one engine's directory at package initialisation.
func loadTimeline(engine string, fsys fs.FS, dir string) *timeline {
	t := &timeline{engine: engine, dir: dir}
	t.migrations, t.loadErr = parseMigrations(fsys, dir)

	return t
}

// The published sets. Both are frozen; see each directory's README.
var (
	sqliteTimeline = loadTimeline("sqlite", migrationFS, migrationDir)
	pgTimeline     = loadTimeline("postgres", pgMigrationFS, pgMigrationDir)
)

// require refuses to touch a ledger with a migration set this binary cannot read.
//
// AT THE TOP OF openDir, because the two branches below it fail in opposite
// directions and both silently: the migrating path would create a database with
// no tables and report success, and the verifying path's loop over an empty set
// finds nothing missing and agrees that the schema is current. Refusing before
// MkdirAll also means a binary that cannot read its own migrations does not
// leave a directory, a lock file or a database behind.
func (t *timeline) require() error {
	if t.loadErr != nil {
		return fmt.Errorf("%w (%s): %w", errMigrationsUnavailable, t.engine, t.loadErr)
	}

	return nil
}

// latest is the highest version in this set, or 0 if it did not load.
//
// Zero is the fail-closed answer, and releasesource already refuses it as "the
// manifest names no ledger schema". See LatestSchemaVersion.
func (t *timeline) latest() int {
	var latest int

	for _, m := range t.migrations {
		if m.Version > latest {
			latest = m.Version
		}
	}

	return latest
}

// refuseUnknownVersions rejects a ledger written by a newer billet.
//
// ONE IMPLEMENTATION, called by the migrating path, the verifying path and the
// cold restore planner. Written more than once these drift, and the failure
// would be an operator command silently tolerating a database its own control
// plane refuses to start against.
//
// IT COMPARES VERSIONS AND NEVER CHECKSUMS, which is what lets the cold caller
// answer without knowing which backend wrote the ledger: a version is the same
// identity on both timelines, while a checksum is over one engine's own
// statement bytes.
func (t *timeline) refuseUnknownVersions(seen map[int]appliedMigration) error {
	known := make(map[int]struct{}, len(t.migrations))
	for _, m := range t.migrations {
		known[m.Version] = struct{}{}
	}

	for v := range seen {
		if _, ok := known[v]; !ok {
			return fmt.Errorf(
				"state database has migration %d, which this billet does not know about; "+
					"it was written by a newer version", v)
		}
	}

	return nil
}

// parseMigrations reads every migration out of fsys.
//
// A PURE FUNCTION OVER AN fs.FS so the refusals below can be driven against
// hand-built fixtures. Every one of them is a state that would otherwise be a
// statement which silently never runs, or a version that silently means two
// things.
func parseMigrations(fsys fs.FS, migrationDir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("state: read the embedded migration directory %s: %w",
			migrationDir, err)
	}

	out := make([]migration, 0, len(entries))
	claimed := make(map[int]string, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf(
				"state: %s/%s is a directory; the migration directory holds .sql files only",
				migrationDir, e.Name())
		}

		version, name, err := parseMigrationFilename(e.Name())
		if err != nil {
			return nil, err
		}

		// A DUPLICATE IS THE COLLISION THIS SCHEME IS BUILT AROUND. Two branches
		// both choosing the next integer is ordinary; one of the two silently
		// never running is not, so it is named here and in CI rather than
		// surfacing later as a checksum mismatch on an unrelated reopen.
		if prev, dup := claimed[version]; dup {
			return nil, fmt.Errorf(
				"state: migrations %s and %s both claim version %d; a version is an identity, "+
					"so one of the two would never run — renumber the one that has not shipped",
				prev, e.Name(), version)
		}

		claimed[version] = e.Name()

		data, err := fs.ReadFile(fsys, migrationDir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("state: read migration %s: %w", e.Name(), err)
		}

		stmts, err := parseMigrationStatements(e.Name(), data)
		if err != nil {
			return nil, err
		}

		out = append(out, migration{Version: version, Name: name, Stmts: stmts})
	}

	// AN EMPTY SET IS NOT AN EMPTY SCHEMA. It means the embed matched nothing,
	// and a control plane that accepted it would create a database with no tables
	// and report success.
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"state: no migrations were found in %s; the embedded set is empty, so nothing "+
				"would create the schema", migrationDir)
	}

	slices.SortFunc(out, func(a, b migration) int { return cmp.Compare(a.Version, b.Version) })

	return out, nil
}

// parseMigrationFilename reads the version and name out of the file's name.
func parseMigrationFilename(file string) (int, string, error) {
	m := migrationFilePattern.FindStringSubmatch(file)
	if m == nil {
		return 0, "", fmt.Errorf(
			"state: migration filename %q is not <zero-padded version>_<lower_snake_name>.sql, "+
				"for example 0043_lease_something.sql", file)
	}

	version, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", fmt.Errorf("state: migration %s has an unreadable version: %w", file, err)
	}

	if version <= 0 {
		return 0, "", fmt.Errorf(
			"state: migration %s has version %d; a version is a positive identity and zero is "+
				"what an unrecorded row reads as", file, version)
	}

	return version, m[2], nil
}

// parseMigrationStatements returns the published bytes of each statement.
func parseMigrationStatements(file string, data []byte) ([]string, error) {
	// A CARRIAGE RETURN CHANGES EVERY CHECKSUM IN THE FILE, silently, and the way
	// it arrives is a checkout on a machine configured to convert line endings.
	// .gitattributes pins these files to `text eol=lf` for that reason; this is
	// what makes the failure loud if anything gets past it.
	if i := bytes.IndexByte(data, '\r'); i >= 0 {
		return nil, fmt.Errorf(
			"state: migration %s has a carriage return at byte %d; these files are LF only, "+
				"because a line-ending conversion changes the published bytes and every "+
				"deployment's recorded checksum with them", file, i)
	}

	lines := strings.Split(string(data), "\n")

	var stmts []string

	// A STATEMENT IS A RANGE OF LINES, not an accumulator. Sliced rather than
	// appended so the bytes handed to strings.Join are the file's own, in the
	// file's order, with nothing between reading them and hashing them.
	body := -1

	for n, line := range lines {
		lineNo := n + 1

		// CHECKED BEFORE THE MARKER COMPARISON, and this is the one refusal that
		// would otherwise be silent in both directions: a padded open marker reads
		// as an ordinary comment, so the statement after it is never seen; a padded
		// close marker reads as part of the statement, so its bytes change.
		if trimmed := strings.TrimSpace(line); trimmed != line &&
			(trimmed == stmtOpenMarker || trimmed == stmtCloseMarker) {
			return nil, fmt.Errorf(
				"state: migration %s line %d is %q; a marker must be written exactly and start "+
					"the line, or the statements around it change without anything saying so",
				file, lineNo, line)
		}

		switch {
		case line == stmtOpenMarker:
			if body >= 0 {
				return nil, fmt.Errorf(
					"state: migration %s line %d opens a statement while one is already open; "+
						"every %s needs its %s", file, lineNo, stmtOpenMarker, stmtCloseMarker)
			}

			body = n + 1

		case line == stmtCloseMarker:
			if body < 0 {
				return nil, fmt.Errorf(
					"state: migration %s line %d closes a statement that was never opened",
					file, lineNo)
			}

			stmt := strings.Join(lines[body:n], "\n")

			body = -1

			if err := checkStatement(file, lineNo, stmt); err != nil {
				return nil, err
			}

			stmts = append(stmts, stmt)

		case body >= 0:
			// Inside a statement. The bytes are taken from lines[body:n] when the
			// close marker arrives, so there is nothing to collect here.

		default:
			// SQL OUTSIDE A STATEMENT NEVER RUNS. Refusing it is the difference
			// between a migration that is missing a marker and a migration that
			// quietly applies less than it says.
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "--") {
				return nil, fmt.Errorf(
					"state: migration %s line %d is outside any statement and is not a comment: "+
						"%q; only text between %s and %s is applied",
					file, lineNo, line, stmtOpenMarker, stmtCloseMarker)
			}
		}
	}

	if body >= 0 {
		return nil, fmt.Errorf(
			"state: migration %s ends with a statement still open; it needs a %s",
			file, stmtCloseMarker)
	}

	if len(stmts) == 0 {
		return nil, fmt.Errorf(
			"state: migration %s contains no statements; a migration that applies nothing "+
				"records a checksum over nothing", file)
	}

	return stmts, nil
}

// checkStatement refuses bytes no published statement has and none should gain.
//
// The interior whitespace of a statement is published and is kept verbatim. What
// is refused is whitespace at either END, because that is what an accidental
// blank line inside the markers produces — and it would be hashed, so it becomes
// part of the migration's identity forever.
//
// TrimSpace is WIDER than " \t\n": it uses unicode.IsSpace, so a non-breaking
// space or a NEL at either boundary is refused too. That is the direction to fail
// in — a stray NBSP arrives by pasting SQL out of a browser, and once it is inside
// a published checksum it is there for the life of the deployment.
//
// TIGHTENING THIS PREDICATE AFTER A MIGRATION HAS SHIPPED IS A FLAG DAY, and that
// is the thing to remember before adding a rule here. It runs in production, on
// every open, over files that are already published — so a new refusal does not
// reject a bad commit, it stops every control plane that has the file. A rule
// about what a NEW migration may contain belongs in a test; only a rule that no
// published statement can possibly violate belongs here.
func checkStatement(file string, lineNo int, stmt string) error {
	if stmt == "" {
		return fmt.Errorf("state: migration %s has an empty statement ending at line %d", file, lineNo)
	}

	if strings.TrimSpace(stmt) != stmt {
		return fmt.Errorf(
			"state: migration %s has a statement ending at line %d that begins or ends with "+
				"whitespace, which becomes part of the migration's published identity. A blank "+
				"line inside the markers is the usual cause; a stray space, tab or "+
				"non-breaking space at either end does it too", file, lineNo)
	}

	return nil
}

// errMigrationsUnavailable is what a caller sees when the embedded set could not
// be read at all.
var errMigrationsUnavailable = errors.New("state: this binary's migration set could not be read")
