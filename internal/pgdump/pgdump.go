// Package pgdump runs PostgreSQL's own client programs against a deployment's
// ledger: pg_dump to copy it into an archive, pg_restore to put a copy back.
//
// BILLET DOES NOT COPY ROWS THROUGH ITS OWN CONNECTION (ADR-008). A consistent
// copy of a database is the database's own tool's to take, so this package
// takes it with that tool and adds what billet alone can: the copy is of the
// snapshot billet read the ledger's migrations and binding in (pg_dump
// --snapshot), so the archive's description of the dump is exact, and the
// connection reaches the client program through its environment, never argv,
// because it carries a password.
//
// THE ONE IMPORTER OF os/exec FOR THESE PROGRAMS. internal/state may run no
// subprocess (a ledger writer must not, and depguard holds it), so the SQL half
// of a dump and a restore lives there and the process half lives here.
//
// The programs are the operator's, installed with the PostgreSQL client, not
// billet's: when one is absent or too old for the server it must talk to, the
// refusal names the package to install rather than running a tool that would
// fail late.
package pgdump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/junioryono/billet/internal/state"
)

// Dumped is what a dump carries, read in the snapshot the dump was taken in.
type Dumped struct {
	// Schema is the PostgreSQL schema the ledger lives in, and the only one the
	// dump holds. A restore must land in a schema of the same name.
	Schema string
	// ServerVersion is the source server's server_version_num.
	ServerVersion int
	// DumpVersion is the major version of the pg_dump that wrote it; a pg_restore
	// older than this may not read it.
	DumpVersion int
	// Migrations and Binding are the ledger's, in the dump's own snapshot.
	Migrations []state.AppliedMigration
	Binding    string
}

// Client runs pg_dump and pg_restore against one ledger.
type Client struct {
	dsn state.DSN
	env state.ClientEnv

	// lookPath finds a program, and dropOutside drops what a restore replaces
	// (state.DropTablesOutside); a test replaces them to run fakes.
	lookPath    func(string) (string, error)
	dropOutside func(ctx context.Context, dsn state.DSN, schema string, keep []string) ([]string, error)
}

// New renders dsn for the client programs. It refuses a DSN that libpq cannot
// be handed faithfully (state.LibpqEnv), before anything runs.
func New(dsn state.DSN) (*Client, error) {
	env, err := state.LibpqEnv(dsn)
	if err != nil {
		return nil, err
	}

	return &Client{dsn: dsn, env: env, lookPath: exec.LookPath, dropOutside: state.DropTablesOutside}, nil
}

// String names the client and never the connection.
func (c *Client) String() string { return "pgdump.Client{" + c.env.String() + "}" }

// Peek reports what the ledger holds, read-only (state.PeekPostgresLedger).
func (c *Client) Peek(ctx context.Context) (state.PostgresLedger, error) {
	return state.PeekPostgresLedger(ctx, c.dsn)
}

// Admission reads the ledger's admission row behind any fence
// (state.PeekPostgresAdmission).
func (c *Client) Admission(ctx context.Context) (state.Admission, error) {
	return state.PeekPostgresAdmission(ctx, c.dsn)
}

// Barrier proves no billet write transaction is in flight on the ledger, from
// any host (state.PostgresWriterBarrier).
func (c *Client) Barrier(ctx context.Context) error {
	return state.PostgresWriterBarrier(ctx, c.dsn)
}

// Dump writes a custom-format dump of the ledger to dest, which must not exist.
//
// fenceDir is the directory whose maintenance fence the copy honours, empty for
// a caller that holds the fence itself (see state.ExportPostgresSnapshot).
func (c *Client) Dump(ctx context.Context, fenceDir, dest string) (Dumped, error) {
	// pg_dump TRUNCATES an existing --file, so the no-clobber rule every archive
	// write follows is held here: a caller writes a dump only to a name it has
	// just made free.
	switch _, err := os.Lstat(dest); {
	case err == nil:
		return Dumped{}, fmt.Errorf("pgdump: %s already exists and billet will not write a dump over it", dest)
	case !errors.Is(err, os.ErrNotExist):
		return Dumped{}, fmt.Errorf("pgdump: inspect %s: %w", dest, err)
	}

	path, major, err := c.program(ctx, "pg_dump")
	if err != nil {
		return Dumped{}, err
	}

	var out Dumped

	err = state.ExportPostgresSnapshot(ctx, c.dsn, fenceDir, func(snap state.LedgerSnapshot) error {
		// pg_dump REFUSES A NEWER SERVER, and says so only after connecting
		// ("aborting because of server version mismatch", measured 2026-10-20 with
		// pg_dump 17 against 18). Asked here first so the refusal names the
		// package that fixes it.
		if server := snap.ServerVersion / 10000; major < server {
			return clientRefusal(fmt.Sprintf("pgdump: %s is PostgreSQL %d and the ledger's server is %d, and "+
				"pg_dump cannot copy a newer server. %s", path, major, server, installAdvice(server)))
		}

		// THE SCHEMA AS AN EXACT NAME, NOT A PATTERN. pg_dump reads --schema as
		// a pattern: unquoted, the name is folded to lower case and `*`, `?` and
		// regular-expression characters match other schemas, so `Billet` could
		// dump `billet` and `billet*` every schema it matches. Double-quoted,
		// every character is literal.
		args := []string{
			"--format=custom",
			"--no-password",
			"--schema=" + quoteName(snap.Schema),
			"--snapshot=" + snap.ID,
			"--file=" + dest,
		}

		if _, err := c.run(ctx, path, args...); err != nil {
			return err
		}

		out = Dumped{
			Schema: snap.Schema, ServerVersion: snap.ServerVersion, DumpVersion: major,
			Migrations: snap.Migrations, Binding: snap.Binding,
		}

		return nil
	})
	if err != nil {
		return Dumped{}, err
	}

	return out, nil
}

// CheckDump proves pg_dump is installed and can copy this ledger's server,
// before a caller writes anything it would have to clear up after a refusal.
func (c *Client) CheckDump(ctx context.Context) error {
	path, major, err := c.program(ctx, "pg_dump")
	if err != nil {
		return err
	}

	led, err := c.Peek(ctx)
	if err != nil {
		return err
	}

	if server := led.ServerVersion / 10000; major < server {
		return clientRefusal(fmt.Sprintf("pgdump: %s is PostgreSQL %d and the ledger's server is %d, and "+
			"pg_dump cannot copy a newer server. %s", path, major, server, installAdvice(server)))
	}

	return nil
}

// Restore replaces the ledger in schema with a dump's, and only with what the
// dump holds of that schema.
//
// --single-transaction MAKES THE RESTORE ALL OR NOTHING: a failure part-way
// leaves the schema exactly as it was (measured 2026-10-20: a restore refused
// at its third statement left none of its tables behind).
//
// AND IT IS A REPLACEMENT, NOT AN OVERLAY. --clean drops only what the dump
// holds, so the tables in schema the dump does not name (a later migration's)
// are dropped first, in a transaction of their own (state.DropTablesOutside).
// Every caller holds a durable copy of what is there, or proof that nothing is,
// before it calls this, so a failure between the two is recovered from that
// copy rather than lost.
//
// SCHEMA IS THE ONE THE CALLER PLANNED FOR, and nothing outside it is touched:
// pg_restore restores only that schema's entries (--schema), and a dump that
// holds no table of it is refused before anything is dropped. A manifest whose
// schema disagrees with its dump would otherwise send the cleanup into one
// schema and the restore into another.
//
// THE SCHEMA ITSELF IS NOT RESTORED. pg_dump --schema writes the schema as an
// entry of its own, and --clean then emits `DROP SCHEMA IF EXISTS <s>` and
// `CREATE SCHEMA <s>` (measured): the drop fails, and with it the whole restore,
// when anything else lives in that schema, and for `public` it would recreate a
// schema the database owner owns as one the restoring role owns. So the entry is
// filtered out of the restore's list and the schema must already exist, which
// the caller has proved by finding current_schema() equal to it.
//
// --no-owner and --no-privileges, because the objects belong to whichever role
// this deployment connects as here, which need not be the source's.
//
// THE WAIT FOR A LOCK IS NOT BOUNDED BY A SETTING: pg_restore sets
// lock_timeout to 0 itself before it restores anything, whatever the session
// was started with. The writer barrier has proved no billet transaction is in
// flight and the deployment is fenced, so a lock it waits on belongs to some
// other session, which pg_stat_activity names; interrupting the command ends
// the restore's transaction and leaves the schema as the drop left it, which
// the caller's copy, or its proof the schema held nothing, covers.
func (c *Client) Restore(ctx context.Context, dump string, dumpVersion int, schema string) error {
	path, major, err := c.program(ctx, "pg_restore")
	if err != nil {
		return err
	}

	if major < dumpVersion {
		return clientRefusal(fmt.Sprintf("pgdump: %s is PostgreSQL %d and this archive's dump was "+
			"written by pg_dump %d, which an older pg_restore may not read. %s", path, major, dumpVersion,
			installAdvice(dumpVersion)))
	}

	// A NAME pg_restore'S LIST CANNOT CARRY UNAMBIGUOUSLY is refused before
	// anything runs: the list separates its fields with spaces and quotes nothing.
	if schema == "" || strings.ContainsFunc(schema, unicode.IsSpace) {
		return fmt.Errorf("pgdump: billet restores a ledger only into a schema whose name has no "+
			"whitespace in it, and this one is %q", schema)
	}

	toc, err := c.run(ctx, path, "--list", dump)
	if err != nil {
		return err
	}

	keep, err := tablesOf(toc, schema)
	if err != nil {
		return err
	}

	if len(keep) == 0 {
		return fmt.Errorf("pgdump: %s holds no table of schema %s, so it is not a ledger to "+
			"restore there; billet changed nothing", dump, schema)
	}

	dir, err := os.MkdirTemp("", "billet-restore-list-*")
	if err != nil {
		return fmt.Errorf("pgdump: stage the restore list: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	list := filepath.Join(dir, "toc.list")
	if err := os.WriteFile(list, withoutSchemaEntries(toc), 0o600); err != nil {
		return fmt.Errorf("pgdump: stage the restore list: %w", err)
	}

	db, err := c.databaseConninfo()
	if err != nil {
		return err
	}

	if _, err := c.dropOutside(ctx, c.dsn, schema, keep); err != nil {
		return err
	}

	_, err = c.run(ctx, path,
		"--single-transaction", "--exit-on-error", "--clean", "--if-exists",
		"--no-owner", "--no-privileges", "--no-password",
		"--schema="+schema, "--use-list="+list, "--dbname="+db, dump)

	return err
}

// ErrClientUnusable is what every refusal about the client program itself
// matches (errors.Is): absent, unrunnable, or too old for the server or the
// dump. A caller tells it from a database error, which is not the client's.
var ErrClientUnusable = errors.New("pgdump: the PostgreSQL client program cannot do this")

// clientRefusal is a refusal about the client program, worded for the
// operator and matching ErrClientUnusable.
type clientRefusal string

func (r clientRefusal) Error() string { return string(r) }

// Is makes a clientRefusal match ErrClientUnusable.
func (clientRefusal) Is(target error) bool { return target == ErrClientUnusable }

// quoteName quotes a name for a pg_dump pattern, so it matches that name and
// nothing else.
func quoteName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// tocEntry matches a restore-list entry, e.g. `220; 1259 16386 TABLE s1 t
// billet` (measured): the ids, then the entry's type, namespace, name and
// owner, separated by spaces and quoted not at all.
var tocEntry = regexp.MustCompile(`^\d+; \d+ \d+ (.*)$`)

// tablesOf lists the tables a dump's restore list holds in schema.
//
// A TABLE ENTRY THAT IS NOT EXACTLY THREE FIELDS IS REFUSED, never split on a
// guess: `TABLE s foo bar billet` is a table `foo bar` or a table `foo` owned
// by `bar billet`, and reading the wrong one keeps a table the restore should
// have replaced. billet's own names have no spaces in them.
func tablesOf(toc []byte, schema string) ([]string, error) {
	var out []string

	for line := range strings.SplitSeq(string(toc), "\n") {
		m := tocEntry.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		rest, ok := strings.CutPrefix(m[1], "TABLE ")
		if !ok || strings.HasPrefix(rest, "DATA ") {
			continue
		}

		fields := strings.Split(rest, " ")
		if len(fields) != 3 {
			return nil, fmt.Errorf("pgdump: the dump lists a table as %q, whose namespace, name "+
				"and owner cannot be told apart, so billet cannot say which tables it holds", rest)
		}

		if fields[0] == schema {
			out = append(out, fields[1])
		}
	}

	return out, nil
}

// tocSchemaEntry matches the restore-list lines for the schema itself: its
// creation and its comment, e.g. `6; 2615 16385 SCHEMA - s1 billet` and
// `3490; 0 0 COMMENT - SCHEMA public pg_database_owner` (measured).
var tocSchemaEntry = regexp.MustCompile(`^\d+; \d+ \d+ (SCHEMA|COMMENT) - (SCHEMA )?\S+ `)

// withoutSchemaEntries comments out the schema's own entries in a restore list.
func withoutSchemaEntries(toc []byte) []byte {
	lines := strings.Split(string(toc), "\n")

	for i, line := range lines {
		if tocSchemaEntry.MatchString(line) {
			lines[i] = ";" + line
		}
	}

	return []byte(strings.Join(lines, "\n"))
}

// databaseConninfo names the database for pg_restore's --dbname, which must be
// given for pg_restore to connect at all rather than print a script. A conninfo
// with only dbname in it, so nothing secret reaches argv and a name holding '='
// is not read as a connection string.
func (c *Client) databaseConninfo() (string, error) {
	for _, v := range c.env.Environ(nil) {
		if name, ok := strings.CutPrefix(v, "PGDATABASE="); ok {
			return "dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(name) + "'", nil
		}
	}

	return "", errors.New("pgdump: the PostgreSQL data source names no database to restore into")
}

// program finds a client program and its major version, asked with the same
// environment it will run with: Debian's pg_wrapper picks a version by the
// connection it is given.
func (c *Client) program(ctx context.Context, name string) (string, int, error) {
	path, err := c.lookPath(name)
	if err != nil {
		return "", 0, clientRefusal(fmt.Sprintf("pgdump: %s is not installed, and billet copies a "+
			"PostgreSQL ledger with the PostgreSQL client's own tools. %s", name, installAdvice(0)))
	}

	out, err := c.run(ctx, path, "--version")
	if err != nil {
		return "", 0, clientRefusal(fmt.Sprintf("%v. %s", err, installAdvice(0)))
	}

	major, err := parseMajor(string(out))
	if err != nil {
		return "", 0, clientRefusal(fmt.Sprintf("pgdump: %s: %v", path, err))
	}

	return path, major, nil
}

// versionLine is `pg_dump (PostgreSQL) 18.1 (Homebrew)` or
// `pg_dump (PostgreSQL) 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1)` (measured).
var versionLine = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

func parseMajor(out string) (int, error) {
	m := versionLine.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("its --version did not say which PostgreSQL it is")
	}

	return strconv.Atoi(m[1])
}

// installAdvice names what to install for a major, or the client in general.
func installAdvice(major int) string {
	pkg, brew := "postgresql-client", "postgresql"
	if major > 0 {
		pkg, brew = fmt.Sprintf("postgresql-client-%d", major), fmt.Sprintf("postgresql@%d", major)
	}

	if runtime.GOOS == "darwin" {
		return "Install it with `brew install " + brew + "` and put its bin directory on PATH."
	}

	return "Install it with `apt-get install " + pkg + "` (from apt.postgresql.org when the " +
		"distribution's is older than the server), or your distribution's equivalent."
}

// stderrLimit bounds what a client program's complaint can grow to in an error.
const stderrLimit = 8 << 10

// run runs a client program with the connection in its environment, and
// returns its standard output.
func (c *Client) run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(c.env.Environ(os.Environ()), "LC_ALL=C")

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &limitWriter{w: &stderr, left: stderrLimit}

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pgdump: %s %s failed (%w): %s", filepath.Base(path),
			firstArg(args), err, strings.TrimSpace(c.env.Redact(stderr.String())))
	}

	return stdout.Bytes(), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}

	return args[0]
}

// limitWriter keeps the first left bytes and discards the rest, so a program
// that writes without end cannot grow an error without end.
type limitWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.left > 0 {
		n := min(len(p), l.left)
		l.w.Write(p[:n])
		l.left -= n
	}

	return len(p), nil
}
