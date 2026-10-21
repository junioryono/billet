package pgdump

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

const testPassword = "hunter2-the-dump-password"

// fakeProgram writes an executable shell script standing in for a PostgreSQL
// client program: it answers --version with the given major, records its argv
// one argument per line and its environment, and for --list prints a TOC with
// the schema's own entries in it, in the shape pg_restore prints (measured
// 2026-10-20).
//
// EXECUTED, NOT PATTERN-MATCHED (billet-testing): the arguments and the
// environment asserted on are the ones a real process received.
func fakeProgram(t *testing.T, dir, name string, major int) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
    echo "%[1]s (PostgreSQL) %[2]d.3 (fake)"
    exit 0
fi
for a in "$@"; do printf '%%s\n' "$a"; done > "%[3]s/%[1]s.argv"
env > "%[3]s/%[1]s.env"
if [ "$1" = "--list" ]; then
    echo ';'
    echo '; Archive created at 2026-10-20'
    echo '6; 2615 16385 SCHEMA - billet_x billet'
    echo '3490; 0 0 COMMENT - SCHEMA billet_x billet'
    echo '220; 1259 16386 TABLE billet_x admission billet'
    echo '3482; 0 16386 TABLE DATA billet_x admission billet'
    exit 0
fi
for a in "$@"; do
    case "$a" in
        --use-list=*) cp "${a#--use-list=}" "%[3]s/%[1]s.list" ;;
    esac
done
exit 0
`, name, major, dir)

	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake %s: %v", name, err)
	}
}

// clientWithFakes is a Client whose programs are the fakes in dir.
func clientWithFakes(t *testing.T, dsn string, dir string) *Client {
	t.Helper()

	c, err := New(state.DSN(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.lookPath = func(name string) (string, error) {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			return "", exec.ErrNotFound
		}

		return path, nil
	}

	return c
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// A RESTORE HANDS pg_restore THE CONNECTION THROUGH ITS ENVIRONMENT, never argv,
// in one transaction that cleans what it replaces and assigns no source owner,
// and leaves the schema's own entries out of what it restores.
func TestARestoreRunsPgRestoreAsTheProbesProvedIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeProgram(t, dir, "pg_restore", 18)

	c := clientWithFakes(t, "postgres://billet:"+testPassword+
		"@db.internal:5432/ledger?sslmode=disable&search_path=billet_x", dir)

	if err := c.Restore(t.Context(), filepath.Join(dir, "ledger.pgdump"), 18); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	argv := readLines(t, filepath.Join(dir, "pg_restore.argv"))

	for _, want := range []string{
		"--single-transaction", "--exit-on-error", "--clean", "--if-exists",
		"--no-owner", "--no-privileges", "--no-password", "--dbname=dbname='ledger'",
	} {
		if !slices.Contains(argv, want) {
			t.Errorf("pg_restore was not given %s: %q", want, argv)
		}
	}

	for _, a := range argv {
		if strings.Contains(a, testPassword) {
			t.Errorf("the password reached pg_restore's argv: %q", a)
		}
	}

	env := readLines(t, filepath.Join(dir, "pg_restore.env"))

	for _, want := range []string{"PGPASSWORD=" + testPassword, "PGHOST=db.internal", "LC_ALL=C"} {
		if !slices.Contains(env, want) {
			t.Errorf("pg_restore's environment lacks %s", strings.SplitN(want, "=", 2)[0])
		}
	}

	if !slices.Contains(env, "PGOPTIONS=-c search_path=billet_x -c lock_timeout=30s") {
		t.Errorf("pg_restore's PGOPTIONS is not the DSN's search_path and the lock timeout: %q",
			grepPrefix(env, "PGOPTIONS="))
	}

	list := strings.Join(readLines(t, filepath.Join(dir, "pg_restore.list")), "\n")

	for _, line := range []string{
		"6; 2615 16385 SCHEMA - billet_x billet",
		"3490; 0 0 COMMENT - SCHEMA billet_x billet",
	} {
		if strings.Contains(list, "\n"+line) || strings.HasPrefix(list, line) {
			t.Errorf("the restore list still restores the schema itself: %q", line)
		}
	}

	if !strings.Contains(list, "\n220; 1259 16386 TABLE billet_x admission billet") {
		t.Errorf("the restore list dropped a table entry it must restore:\n%s", list)
	}
}

// AN OLDER pg_restore THAN THE ARCHIVE'S pg_dump IS REFUSED BEFORE IT RUNS, and
// an absent program is refused naming what to install.
func TestAClientProgramThatCannotDoTheJobIsRefusedBeforeItRuns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeProgram(t, dir, "pg_restore", 15)

	c := clientWithFakes(t, "postgres://billet:"+testPassword+"@h/d?sslmode=disable", dir)

	err := c.Restore(t.Context(), filepath.Join(dir, "ledger.pgdump"), 16)
	if err == nil || !strings.Contains(err.Error(), "postgresql") || !strings.Contains(err.Error(), "16") {
		t.Fatalf("an older pg_restore = %v, want a refusal naming the package for 16", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "pg_restore.argv")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the older pg_restore was run anyway")
	}

	empty := clientWithFakes(t, "postgres://billet:"+testPassword+"@h/d?sslmode=disable", t.TempDir())

	err = empty.Restore(t.Context(), filepath.Join(dir, "ledger.pgdump"), 16)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("an absent pg_restore = %v, want a refusal saying it is not installed", err)
	}
}

// A DUMP WILL NOT WRITE OVER A FILE: pg_dump truncates --file, so the
// no-clobber rule is held before it runs.
func TestADumpRefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dest := filepath.Join(dir, "ledger.pgdump")

	if err := os.WriteFile(dest, []byte("somebody's file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := clientWithFakes(t, "postgres://billet:"+testPassword+"@h/d?sslmode=disable", dir)

	if _, err := c.Dump(t.Context(), "", dest); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a dump over an existing file = %v, want a refusal", err)
	}

	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read the occupied destination: %v", err)
	}

	if string(body) != "somebody's file" {
		t.Errorf("the occupied destination was changed: %q", body)
	}
}

// The redaction of a failed program's complaint, which is the second line
// behind libpq's own care.
func TestAFailedProgramsComplaintIsRedacted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'pg_restore (PostgreSQL) 18.0'; exit 0; fi\n" +
		"echo \"FATAL: password authentication failed: $PGPASSWORD\" >&2\nexit 1\n"

	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := clientWithFakes(t, "postgres://billet:"+testPassword+"@h/d?sslmode=disable", dir)

	err := c.Restore(t.Context(), filepath.Join(dir, "ledger.pgdump"), 18)
	if err == nil {
		t.Fatal("a failing pg_restore reported success")
	}

	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("the error quotes the password: %v", err)
	}

	if !strings.Contains(err.Error(), "password authentication failed") {
		t.Errorf("the error lost what the program said: %v", err)
	}
}

func grepPrefix(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}

	return ""
}

// requireDatabase returns a DSN for a fresh database of this test's own with a
// schema named schema in it, or skips (and refuses to skip under CI).
//
// A DATABASE, NOT A SCHEMA, PER SIDE: pg_restore puts a ledger back into the
// schema it came from, so a round trip needs two databases holding schemas of
// one name.
func requireDatabase(t *testing.T, suffix, schema string) string {
	t.Helper()

	dsn := os.Getenv("BILLET_TEST_POSTGRES_DSN")
	if dsn == "" {
		if elsewhere := os.Getenv("BILLET_TEST_POSTGRES_ELSEWHERE"); elsewhere != "" {
			t.Skipf("BILLET_TEST_POSTGRES_DSN is unset here; this run's %q job exercises it", elsewhere)
		}

		if os.Getenv("CI") != "" {
			t.Fatal("BILLET_TEST_POSTGRES_DSN is unset under CI, so the PostgreSQL dump would go untested")
		}

		t.Skip("BILLET_TEST_POSTGRES_DSN is unset; start a PostgreSQL and set it to exercise the dump")
	}

	name := strings.ToLower("billet_pgdump_" + suffix)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() { _ = admin.Close() }()

	for _, stmt := range []string{
		`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`,
		`CREATE DATABASE "` + name + `"`,
	} {
		if _, err := admin.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("prepare database %s: %v", name, err)
		}
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		//nolint:errcheck,noctx // teardown, after the test's context is gone.
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	u.Path = "/" + name

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(t.Context(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	return u.String()
}

// A DUMP IS TAKEN IN THE SNAPSHOT BILLET READ THE LEDGER IN, of the schema the
// ledger is in, with the connection in the environment. The round trip below
// cannot see the snapshot (a dump without it restores the same rows when
// nothing writes in between, which is why removing it survived a mutation run);
// this asserts the argument a real process received.
func TestADumpIsTakenInTheSnapshotBilletRead(t *testing.T) {
	const schema = "billet_snapshot"

	dsn := requireDatabase(t, "snapshot", schema)

	if _, err := state.OpenPostgres(t.Context(), t.TempDir(), state.DSN(dsn)); err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	dir := t.TempDir()
	fakeProgram(t, dir, "pg_dump", 99)

	c := clientWithFakes(t, dsn, dir)

	dumped, err := c.Dump(t.Context(), "", filepath.Join(dir, "ledger.pgdump"))
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	argv := readLines(t, filepath.Join(dir, "pg_dump.argv"))

	snapshot := grepPrefix(argv, "--snapshot=")
	if len(snapshot) <= len("--snapshot=") {
		t.Errorf("pg_dump was not given the snapshot billet exported: %q", argv)
	}

	for _, want := range []string{"--format=custom", "--no-password", "--schema=" + schema} {
		if !slices.Contains(argv, want) {
			t.Errorf("pg_dump was not given %s: %q", want, argv)
		}
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// AS A CREDENTIAL, not as a substring: the test server's password is the
	// same word as the schema names, so a bare substring match would find it in
	// every argument.
	password, _ := u.User.Password()

	for _, a := range argv {
		if strings.Contains(a, ":"+password+"@") || strings.Contains(a, "password=") ||
			strings.Contains(a, "postgres://") {
			t.Errorf("the connection string reached pg_dump's argv: %q", a)
		}
	}

	if dumped.Schema != schema || dumped.DumpVersion != 99 || len(dumped.Migrations) == 0 {
		t.Errorf("the dump describes itself as %+v", dumped)
	}
}

// THE ROUND TRIP, with the real programs: a ledger a controller claimed and an
// operator sealed is dumped from one database and restored into another, and
// what comes back is the same ledger — its migrations, its binding and its
// admission — read through billet's own connection.
func TestALedgerDumpedFromOneDatabaseRestoresIntoAnother(t *testing.T) {
	const schema = "billet_round_trip"

	source := requireDatabase(t, "source", schema)
	target := requireDatabase(t, "target", schema)

	for _, program := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(program); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is not installed under CI, so the dump would go untested", program)
			}

			t.Skipf("%s is not installed", program)
		}
	}

	db, err := state.OpenPostgres(t.Context(), t.TempDir(), state.DSN(source))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	defer func() { _ = db.Close() }()

	const deployment = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

	if _, err := db.ClaimController(t.Context(), "host-a", deployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if _, err := db.Seal(t.Context(), state.SealRequest{
		Provenance: state.ProvenanceOperator, Reason: "dumped", Actor: "test",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	src, err := New(state.DSN(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dump := filepath.Join(t.TempDir(), "ledger.pgdump")

	dumped, err := src.Dump(t.Context(), "", dump)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	if dumped.Schema != schema || dumped.Binding != deployment || len(dumped.Migrations) == 0 ||
		dumped.DumpVersion == 0 {
		t.Fatalf("the dump describes itself as %+v", dumped)
	}

	dst, err := New(state.DSN(target))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := dst.Restore(t.Context(), dump, dumped.DumpVersion); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	led, err := dst.Peek(t.Context())
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}

	if led.Binding != deployment || !slices.Equal(led.Migrations, dumped.Migrations) {
		t.Errorf("the restored ledger is bound to %q with %d migrations, want %s with %d",
			led.Binding, len(led.Migrations), deployment, len(dumped.Migrations))
	}

	a, err := dst.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	if a.Mode != state.AdmissionSealed || a.Reason != "dumped" {
		t.Errorf("the restored admission is %+v, want the operator's seal", a)
	}

	// AND A SECOND RESTORE OVER THE FIRST IS THE SAME LEDGER, which is what lets
	// a resumed run simply restore again: --clean replaces what the first put
	// there, in one transaction.
	if err := dst.Restore(t.Context(), dump, dumped.DumpVersion); err != nil {
		t.Fatalf("a second Restore: %v", err)
	}

	again, err := dst.Peek(t.Context())
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}

	if again.Binding != deployment || !slices.Equal(again.Migrations, dumped.Migrations) {
		t.Errorf("after a second restore the ledger reads as %+v", again)
	}
}
