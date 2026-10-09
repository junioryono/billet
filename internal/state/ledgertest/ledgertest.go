// Package ledgertest gives a test a migrated SQLite ledger without migrating one
// from empty.
//
// MIGRATING IS MOST OF WHAT OPENING A FRESH LEDGER COSTS A TEST: 1.27s an open
// under -race, against 31ms for a seeded one (measured 2026-10-03). Seed writes
// a template that this test binary migrated once into the test's own fresh
// directory, and the caller then opens it with the real state.Open. So
// everything an open proves still runs for real against a directory nothing else
// touches: the directory lock, the pragma read-back, the schema and checksum
// verification of every recorded migration, and the deployment identity. Only
// the replay of the migration statements is skipped.
//
// THE TEMPLATE IS BUILT BY THE BINARY THAT USES IT AND KEPT IN MEMORY, never
// cached on disk between runs. A file under a shared temporary directory could
// be planted by another account with forged migration records that state.Open
// would accept, and a key derived from the checkout could describe migrations
// other than the ones this binary embeds. Built here, the template is exactly
// what state.Open in this binary produces, at the cost of one migration per test
// binary.
//
// StageSuccessor is the other thing a test outside internal/state cannot do
// for itself: write a successor's controller claim, so the controller holding
// the ledger has its next write refused as a replaced one's is.
//
// NOTHING OUTSIDE A TEST IMPORTS IT (boundary in ledgertest_test.go). The
// PostgreSQL ledgers tests open are untouched: a schema there is a server's, not
// a file to copy.
package ledgertest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// builder builds a template once and hands every caller the same answer.
type builder struct {
	once     sync.Once
	template []byte
	err      error
}

// errNeverBuilt is the answer when the first caller stopped inside the build
// without returning, as a t.Fatal in it does: sync.Once then counts the build as
// done, and without this every later seed would write an empty file.
var errNeverBuilt = errors.New("the first caller stopped before the template was built")

// get runs build once and returns its result to every caller.
func (b *builder) get(build func() ([]byte, error)) ([]byte, error) {
	b.once.Do(func() {
		b.err = errNeverBuilt
		b.template, b.err = build()
	})

	return b.template, b.err
}

var ledger builder

// Dir is a fresh t.TempDir() holding a migrated ledger, for state.Open to open.
func Dir(tb testing.TB) string {
	tb.Helper()

	dir := tb.TempDir()
	Seed(tb, dir)

	return dir
}

// Seed puts a migrated ledger at dir's billet.db for state.Open to open, creating
// dir if it does not exist. It refuses a dir that already holds a ledger, since
// a seed over one would hide what that test wrote.
func Seed(tb testing.TB, dir string) {
	tb.Helper()

	if err := seed(tb, dir); err != nil {
		tb.Fatalf("ledgertest: %v", err)
	}
}

func seed(tb testing.TB, dir string) error {
	tb.Helper()

	template, err := ledger.get(func() ([]byte, error) { return build(tb) })
	if err != nil {
		return fmt.Errorf("build the template ledger: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	if err := writeNew(state.LedgerPath(dir), template); err != nil {
		return fmt.Errorf("seed %s: %w", dir, err)
	}

	return nil
}

// build migrates a ledger in a scratch directory with this binary's state.Open
// and returns a consistent single-file copy of it. The scratch directory is the
// first caller's, and nothing outlives the call but the bytes.
func build(tb testing.TB) ([]byte, error) {
	tb.Helper()

	// ABSOLUTE, because SnapshotInto refuses a relative destination and t.TempDir
	// is relative when GOTMPDIR or TMPDIR is.
	work, err := filepath.Abs(tb.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolve the scratch directory: %w", err)
	}

	db, err := state.Open(tb.Context(), filepath.Join(work, "state"))
	if err != nil {
		return nil, fmt.Errorf("migrate a ledger: %w", err)
	}

	snapshot := filepath.Join(work, "template.db")
	snapErr := db.SnapshotInto(tb.Context(), snapshot)

	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close the migrated ledger: %w", err)
	}

	if snapErr != nil {
		return nil, fmt.Errorf("snapshot the migrated ledger: %w", snapErr)
	}

	body, err := os.ReadFile(snapshot)
	if err != nil {
		return nil, fmt.Errorf("read the snapshot: %w", err)
	}

	return body, nil
}

// writeNew writes body to a new file at path, mode 0600, refusing one that
// exists.
func writeNew(path string, body []byte) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	if _, err := out.Write(body); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// StageSuccessor writes holder's claim as the deployment's controller through
// db, an operator handle on a ledger a controller holds, which bumps the claim's
// epoch: that controller's next write is then refused, and its leadership lost.
func StageSuccessor(tb testing.TB, db *state.DB, holder string) {
	tb.Helper()

	if err := db.Tx(tb.Context(), func(tx *sql.Tx) error {
		_, err := state.WriteQueries(tx).ClaimController(tb.Context(), ledgerdb.ClaimControllerParams{
			Holder:    holder,
			ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})

		return err
	}); err != nil {
		tb.Fatalf("ledgertest: stage %s's claim: %v", holder, err)
	}
}
