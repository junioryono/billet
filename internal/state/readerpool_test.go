package state

import (
	"context"
	"database/sql"
	"net/url"
	"sync"
	"testing"
	"time"
)

// withReaderIdleTime shortens how long an idle reader stays connected, so a test
// can watch one go without waiting out the production minute.
func withReaderIdleTime(d time.Duration) OpenOption {
	return func(m *openMode) { m.readerIdle = d }
}

func TestTheReaderPoolIsTheSizeTheOpenAskedFor(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		opts []OpenOption
		want int
	}{
		"no size named keeps the default": {want: defaultReaderConns},
		"a budget's size is taken":        {opts: []OpenOption{WithReaderConns(1)}, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db, err := Open(t.Context(), t.TempDir(), tc.opts...)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			if got := db.r.Stats().MaxOpenConnections; got != tc.want {
				t.Fatalf("the reader pool holds at most %d connections, want %d", got, tc.want)
			}
		})
	}
}

// AN IDLE DEPLOYMENT GIVES ITS READERS BACK. Every reader a burst opened closes
// once nothing has borrowed it for the idle time, which is what brings a packed
// server's connection count down when its deployments go quiet.
func TestAReaderNothingBorrowsIsClosed(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir(), withReaderIdleTime(50*time.Millisecond))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	conns := make([]*sql.Conn, 0, defaultReaderConns)
	for range defaultReaderConns {
		conn, err := db.r.Conn(t.Context())
		if err != nil {
			t.Fatalf("borrow a reader: %v", err)
		}
		conns = append(conns, conn)
	}

	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("return a reader: %v", err)
		}
	}

	if got := db.r.Stats().OpenConnections; got != defaultReaderConns {
		t.Fatalf("%d readers open right after a burst of %d; the pool must keep them for the next one",
			got, defaultReaderConns)
	}

	deadline := time.Now().Add(30 * time.Second)
	for db.r.Stats().OpenConnections > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d readers still connected 30s past an idle time of 50ms",
				db.r.Stats().OpenConnections)
		}

		time.Sleep(10 * time.Millisecond)
	}

	if got := db.r.Stats().MaxIdleTimeClosed; got != defaultReaderConns {
		t.Fatalf("%d readers closed for being idle, want %d", got, defaultReaderConns)
	}
}

// A BUDGET IS WHAT THE SERVER SEES, not what billet believes it opened: the
// writer, the claim's session and one reader, counted by the server while more
// reads than the pool holds are running.
func TestAPostgresLedgerHoldsNoMoreThanItsConnectionBudget(t *testing.T) {
	t.Parallel()

	const app = "billet-budget-test"

	dsn := requirePostgres(t)

	u, err := url.Parse(string(dsn))
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}

	q := u.Query()
	q.Set("application_name", app)
	u.RawQuery = q.Encode()

	db, err := OpenPostgres(t.Context(), t.TempDir(), DSN(u.String()), WithReaderConns(1))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	watcher, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open the watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	var reads sync.WaitGroup

	for range 4 {
		reads.Go(func() {
			err := db.View(ctx, func(tx Querier) error {
				var slept string

				return tx.QueryRowContext(ctx, `SELECT pg_sleep(0.2)::text`).Scan(&slept)
			})
			if err != nil {
				t.Errorf("a read: %v", err)
			}
		})
	}

	most := 0

	for range 10 {
		var n int
		if err := watcher.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_stat_activity WHERE application_name = $1`, app,
		).Scan(&n); err != nil {
			t.Fatalf("count the ledger's connections: %v", err)
		}

		most = max(most, n)

		time.Sleep(50 * time.Millisecond)
	}

	reads.Wait()

	if most != 3 {
		t.Fatalf("the server saw at most %d of the ledger's connections, want the writer, the claim and "+
			"the one reader the budget leaves", most)
	}
}
