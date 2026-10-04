package ledgertest

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/junioryono/billet/internal/importcheck"
	"github.com/junioryono/billet/internal/state"
)

// peekAll reads the migration records of the ledger file in dir WITHOUT opening
// it, so nothing state.Open would apply can make a seed that copied nothing look
// complete.
func peekAll(t *testing.T, dir string) []state.AppliedMigration {
	t.Helper()

	applied, err := state.PeekMigrations(t.Context(), state.LedgerPath(dir))
	if err != nil {
		t.Fatalf("read the migrations of %s: %v", dir, err)
	}

	return applied
}

// A SEEDED LEDGER CARRIES EVERY MIGRATION BEFORE ANYTHING OPENS IT, once each and
// in order, and the real open then accepts it: what a test gets is what migrating
// from empty would have given it.
func TestASeededLedgerCarriesEveryMigration(t *testing.T) {
	t.Parallel()

	dir := Dir(t)

	applied := peekAll(t, dir)
	want := state.LatestSchemaVersion()
	if len(applied) != want {
		t.Fatalf("the seeded ledger records %d migrations before any open, want %d", len(applied), want)
	}
	for i, m := range applied {
		if m.Version != i+1 || m.Checksum == "" {
			t.Fatalf("migration record %d of the seeded ledger is %+v", i+1, m)
		}
	}

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open the seeded ledger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the seeded ledger: %v", err)
	}
}

// TWO TESTS GET TWO LEDGERS: a write to one is not seen in the other or in the
// template, which a seed that linked rather than copied would break.
func TestEachSeedIsItsOwnFile(t *testing.T) {
	t.Parallel()

	a, b := Dir(t), Dir(t)

	if err := os.WriteFile(state.LedgerPath(a), []byte("not a ledger"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := len(peekAll(t, b)); got != state.LatestSchemaVersion() {
		t.Fatalf("a write to one seeded ledger reached another: it records %d migrations", got)
	}

	c := Dir(t)
	if got := len(peekAll(t, c)); got != state.LatestSchemaVersion() {
		t.Fatalf("a write to a seeded ledger reached the template: a later seed records %d migrations", got)
	}
}

// A SEED NEVER WRITES OVER A LEDGER: that would hide what the test wrote.
func TestASeedRefusesALedgerThatExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(state.LedgerPath(dir), []byte("written by the test"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := seed(t, dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("seed over an existing ledger = %v, want os.ErrExist", err)
	}

	body, err := os.ReadFile(state.LedgerPath(dir))
	if err != nil || string(body) != "written by the test" {
		t.Fatalf("the existing ledger was changed: %q, %v", body, err)
	}
}

// A BUILD THAT NEVER RETURNED IS AN ERROR TO EVERY LATER CALLER, not an empty
// template: a t.Fatal inside it exits the goroutine with sync.Once marked done.
func TestABuildThatNeverReturnedIsAnError(t *testing.T) {
	t.Parallel()

	var b builder

	done := make(chan struct{})
	go func() {
		defer close(done)

		body, err := b.get(func() ([]byte, error) {
			runtime.Goexit()

			return []byte("unreachable"), nil
		})
		t.Errorf("get returned (%q, %v) from a build that exited its goroutine", body, err)
	}()
	<-done

	body, err := b.get(func() ([]byte, error) { return []byte("a second build"), nil })
	if !errors.Is(err, errNeverBuilt) || body != nil {
		t.Fatalf("after a build that never returned, get = (%q, %v), want (nil, errNeverBuilt)", body, err)
	}
}

// A RELATIVE TEMPORARY DIRECTORY STILL BUILDS: SnapshotInto refuses a relative
// destination, and t.TempDir is relative when the directory it is made under is.
// testing reads GOTMPDIR first and os.TempDir's TMPDIR after it, so both are set.
func TestARelativeTemporaryDirectoryStillBuilds(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A SUBTEST, because a test's temporary root is made at its first TempDir
	// call: the parent's is already absolute.
	t.Run("relative", func(t *testing.T) {
		t.Setenv("GOTMPDIR", rel)
		t.Setenv("TMPDIR", rel)

		if dir := t.TempDir(); filepath.IsAbs(dir) {
			t.Fatalf("t.TempDir is %s under a temporary root of %s, so this case stages nothing", dir, rel)
		}

		body, err := build(t)
		if err != nil {
			t.Fatalf("build under a relative temporary root: %v", err)
		}
		if len(body) == 0 {
			t.Fatal("build under a relative temporary root returned no template")
		}
	})
}

// NOTHING OUTSIDE A TEST IMPORTS THIS PACKAGE. A production path that opened a
// ledger seeded from a test's template would start from a schema no deployment
// migrated.
func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	importers := importcheck.ProductionImporters(t, "github.com/junioryono/billet/internal/state/ledgertest",
		"github.com/junioryono/billet/internal/replay")
	if len(importers) > 0 {
		t.Fatalf("the template ledger is imported by production code: %v", importers)
	}
}
