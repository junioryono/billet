package alloc

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/state"
)

// THIS PACKAGE'S TESTS ARE THE CAPACITY CONFORMANCE SUITE, AND THEY RUN AGAINST
// EITHER BACKEND.
//
// internal/alloc holds the rules a second engine could break without failing to
// compile: escrow before advertising, the fencing epoch, capacity charged on
// COALESCE(node, target_node), custody staying charged until evidence, and the
// quiescence answer `billet local down` stops services on. Every one of them is
// a property of a TRANSACTION, and the transaction is exactly what differs
// between a file with a write lock and a server with an advisory one.
//
// ONE CHOKE POINT rather than a parallel suite: `BILLET_TEST_LEDGER=postgres go
// test ./internal/alloc` is the SAME tests, so a rule added here is covered on
// both engines without anybody having to remember to add it twice. A second
// suite is the two-pins problem wearing a test's clothes.
const (
	ledgerEnv      = "BILLET_TEST_LEDGER"
	postgresDSNEnv = "BILLET_TEST_POSTGRES_DSN"
)

// testingPostgres reports whether this run was asked for the PostgreSQL backend.
func testingPostgres() bool {
	return strings.EqualFold(os.Getenv(ledgerEnv), "postgres")
}

// openTestLedger opens the ledger this run is exercising.
//
// SQLite unless asked otherwise, so an ordinary `go test ./...` is unchanged and
// needs nothing installed. Asked for PostgreSQL with no server, it FAILS rather
// than skipping: the variable is an explicit request, and answering it with a
// quiet SQLite run would report a conformance pass for an engine nothing
// touched.
func openTestLedger(t *testing.T) *state.DB {
	t.Helper()

	db, _ := openTestLedgerPair(t, false)

	return db
}

// openTestLedgerPair opens the ledger, and optionally a SECOND handle on the
// same one — which is what an operator command against a running control plane
// is.
//
// THE TWO BACKENDS DISAGREE ABOUT WHAT "THE SAME LEDGER" MEANS, which is why
// this is here rather than at the call sites: on SQLite it is the same DIRECTORY
// opened without the exclusive lock, and on PostgreSQL it is the same DSN, from
// a different directory, potentially from a different machine.
func openTestLedgerPair(t *testing.T, wantSecond bool) (*state.DB, *state.DB) {
	t.Helper()

	if !testingPostgres() {
		dir := t.TempDir()

		first := mustOpen(t, func() (*state.DB, error) { return state.Open(t.Context(), dir) })
		if !wantSecond {
			return first, nil
		}

		second := mustOpen(t, func() (*state.DB, error) { return state.OpenAdmin(t.Context(), dir) })

		return first, second
	}

	dsn := requireTestSchema(t)

	first := mustOpen(t, func() (*state.DB, error) {
		return state.OpenPostgres(t.Context(), t.TempDir(), state.DSN(dsn))
	})

	if !wantSecond {
		return first, nil
	}

	// A DIFFERENT STATE DIRECTORY, because the two handles stand for two
	// processes: the directory lock is per host and is not what is under test.
	second := mustOpen(t, func() (*state.DB, error) {
		return state.OpenPostgresAdmin(t.Context(), t.TempDir(), state.DSN(dsn))
	})

	return first, second
}

func mustOpen(t *testing.T, open func() (*state.DB, error)) *state.DB {
	t.Helper()

	db, err := open()
	if err != nil {
		t.Fatalf("open the test ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// testSchemaSeq makes every schema this package creates unique, so two ledgers
// opened by one test are two deployments rather than one shared by accident.
var testSchemaSeq atomic.Int64

// requireTestSchema hands back a DSN pointing at a schema of its own.
//
// ONE PER CALL, because these tests assume a ledger nobody else is writing to —
// which is what a deployment gets, and what two of them sharing one schema would
// not. It also exercises the scoping the backend depends on: every catalogue
// question it asks is scoped to current_schema().
func requireTestSchema(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(postgresDSNEnv)
	if dsn == "" {
		t.Fatalf("%s asks for the PostgreSQL backend and %s is unset, so this run would have "+
			"quietly exercised SQLite and reported a PostgreSQL pass", ledgerEnv, postgresDSNEnv)
	}

	// LOWERCASE, because search_path folds an unquoted identifier while the
	// CREATE SCHEMA below quotes one. A mixed-case name creates a schema nothing
	// then selects, and the failure names neither.
	//
	// AND TRUNCATED WITH A DIGEST RATHER THAN CUT SHORT. PostgreSQL identifiers
	// stop at 63 bytes, and this package's names are long enough that two
	// SUBTESTS of one test truncate to the same string — which does not read as
	// a name collision, it reads as `duplicate key value violates
	// pg_namespace_nspname_index` from a CREATE SCHEMA nobody was looking at.
	//
	// AND ONE SCHEMA PER CALL, NOT PER TEST. A test that opens the ledger more
	// than once is opening more than one DEPLOYMENT — each with its own fleet and
	// its own leases — and a shared schema would let the second see the first's
	// rows. It used to be per test and got away with it because this function
	// DROPS and recreates, which is a dependency on teardown ordering rather than
	// on isolation; on PostgreSQL it is also two control planes on one ledger,
	// which the controller exclusion now correctly refuses.
	seq := testSchemaSeq.Add(1)
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	sum := sha256.Sum256(fmt.Appendf(nil, "%s#%d", t.Name(), seq))
	schema := fmt.Sprintf("billet_%.32s_%x", name, sum[:4])

	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", postgresDSNEnv, err)
	}

	defer func() { _ = admin.Close() }()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`,
		`CREATE SCHEMA ` + quoted,
	} {
		if _, err := admin.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("prepare the test schema: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		//nolint:errcheck,noctx // teardown, after the test's context is gone.
		_, _ = cleanup.Exec(`DROP SCHEMA IF EXISTS ` + quoted + ` CASCADE`)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresDSNEnv, err)
	}

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	return u.String()
}

// skipOnPostgres skips a test that asserts something about SQLITE ITSELF.
//
// A SKIP THAT PASSES QUIETLY IS THE FAILURE MODE THIS PACKAGE CARES ABOUT, so
// this exists for exactly one situation and takes the reason as an argument: a
// test whose SUBJECT is one engine's own behaviour rather than billet's rule.
// Those cannot be translated, only written again for the other engine, and
// pretending otherwise would either fail the conformance run over no defect or
// weaken the assertion until it says nothing on either.
//
// Everything else in this package runs on both. Two tests use this.
func skipOnPostgres(t *testing.T, why string) {
	t.Helper()

	if testingPostgres() {
		t.Skip("this asserts SQLite's own behaviour: " + why)
	}
}
