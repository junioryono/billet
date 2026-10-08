package state

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingObserver records what the ledger tells it, and signals each retry.
type recordingObserver struct {
	mu      sync.Mutex
	waited  []time.Duration
	held    []bool
	retries int
	retried chan struct{}
}

func (o *recordingObserver) WriteWaited(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.waited = append(o.waited, d)
}

func (o *recordingObserver) WriteHeld(_ time.Duration, committed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.held = append(o.held, committed)
}

func (o *recordingObserver) WriteRetried() {
	o.mu.Lock()
	o.retries++
	o.mu.Unlock()

	if o.retried != nil {
		select {
		case o.retried <- struct{}{}:
		default:
		}
	}
}

func (o *recordingObserver) snapshot() (waited int, held []bool, retries int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return len(o.waited), append([]bool(nil), o.held...), o.retries
}

// EACH WRITE IS TOLD ONCE: how long it waited, and that it held the slot and
// whether it committed. A write that fails is told it did not commit.
func TestTheObserverIsToldHowEachWriteWent(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// NO OBSERVER IS THE DEFAULT, and a write with none is told nothing and
	// fails nothing.
	if err := db.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a write with no observer: %v", err)
	}

	obs := &recordingObserver{}
	db.Observe(obs)

	if err := db.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("refused")
	if err := db.Tx(t.Context(), func(*sql.Tx) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("the failing write returned %v", err)
	}

	waited, held, _ := obs.snapshot()
	if waited != 2 || len(held) != 2 || !held[0] || held[1] {
		t.Errorf("told %d waits and holds %v, want 2 waits and [true false]", waited, held)
	}

	// A READ USES NO WRITER SLOT, and is told nothing.
	if err := db.View(t.Context(), func(Querier) error { return nil }); err != nil {
		t.Fatal(err)
	}

	db.Observe(nil)

	if err := db.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if waited, held, _ := obs.snapshot(); waited != 2 || len(held) != 2 {
		t.Errorf("after a read and with the observer removed, told %d waits and %d holds, want 2 and 2", waited, len(held))
	}
}

// A WRITE THAT WAITED FOR THE SLOT IS TOLD EACH RETRY, AND HOW LONG IT WAITED.
//
// The server holds the writer slot, so the operator's write retries; once the
// observer has been told of a retry, the server lets go.
func TestTheObserverIsToldOfEachBusyRetry(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.w.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the operator handle's busy timeout: %v", err)
	}

	obs := &recordingObserver{retried: make(chan struct{}, 1)}
	admin.Observe(obs)

	bound, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var adminErr error

	done := make(chan struct{})

	if err := server.Tx(ctx, func(*sql.Tx) error {
		go func() {
			defer close(done)

			adminErr = admin.Tx(bound, func(atx *sql.Tx) error {
				_, err := atx.ExecContext(bound, `DELETE FROM join_tokens`)

				return err
			})
		}()

		select {
		case <-obs.retried:
		case <-bound.Done():
			t.Error("the waiting write never told the observer of a retry")
		}

		return nil
	}); err != nil {
		t.Fatalf("the server's transaction: %v", err)
	}

	// The server's transaction has ended; the operator's write now goes through.
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("the operator's write never finished")
	}

	if adminErr != nil {
		t.Fatalf("the operator's write: %v", adminErr)
	}

	_, held, retries := obs.snapshot()
	if retries < 1 || !held[0] {
		t.Errorf("told %d retries and holds %v, want at least one retry and a commit", retries, held)
	}

	obs.mu.Lock()
	waited := obs.waited[0]
	obs.mu.Unlock()

	if waited < busyRetryInterval {
		t.Errorf("a write that retried was told it waited %v, less than one retry's pause (%v)", waited, busyRetryInterval)
	}
}

// blockingObserver holds its first WriteWaited until released, and passes on
// each hold it is told.
type blockingObserver struct {
	recordingObserver

	first    atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	heldTold chan time.Duration
}

func (o *blockingObserver) WriteWaited(d time.Duration) {
	o.recordingObserver.WriteWaited(d)

	// ONLY THE FIRST CALL IS HELD; a later write's callback passes through,
	// which sync.Once would not let it do while the first was still held.
	if o.first.CompareAndSwap(false, true) {
		close(o.entered)
		<-o.release
	}
}

func (o *blockingObserver) WriteHeld(d time.Duration, committed bool) {
	o.recordingObserver.WriteHeld(d, committed)

	select {
	case o.heldTold <- d:
	default:
	}
}

// THE OBSERVER RUNS ONCE THE SLOT IS FREE. While it is held inside its first
// callback, another write on the same handle, whose writer pool is one
// connection, still goes through; and the hold it reports for the first write
// leaves out the time the callback was held.
func TestTheObserverRunsAfterTheWriterSlotIsReleased(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			db, err := Open(t.Context(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = db.Close() })

			obs := &blockingObserver{
				entered: make(chan struct{}), release: make(chan struct{}), heldTold: make(chan time.Duration, 4),
			}
			db.Observe(obs)

			bound, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			failed := errors.New("rolled back on purpose")
			first := make(chan error, 1)

			go func() {
				first <- db.Tx(bound, func(*sql.Tx) error {
					if commit {
						return nil
					}

					return failed
				})
			}()

			select {
			case <-obs.entered:
			case <-bound.Done():
				t.Fatal("the first write never told its observer")
			}

			// HELD INSIDE THE CALLBACK, long enough that a hold measured across
			// it could not be mistaken for one that was not.
			const held = 200 * time.Millisecond

			second := make(chan error, 1)

			go func() { second <- db.Tx(bound, func(*sql.Tx) error { return nil }) }()

			select {
			case err := <-second:
				if err != nil {
					t.Fatalf("a write while the observer ran: %v", err)
				}
			case <-bound.Done():
				t.Fatal("a second write could not take the slot while the first write's observer ran")
			}

			time.Sleep(held) // the callback's own time, which is the scenario
			close(obs.release)

			if err := <-first; commit != (err == nil) {
				t.Fatalf("the first write returned %v", err)
			}

			for range 2 {
				select {
				case d := <-obs.heldTold:
					if d >= held {
						t.Errorf("a hold of %v was reported, which counts the observer's own %v", d, held)
					}
				case <-bound.Done():
					t.Fatal("a hold was never told")
				}
			}
		})
	}
}
