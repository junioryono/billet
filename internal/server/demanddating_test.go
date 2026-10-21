package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// sealedTier is one 4 vCPU tier on one host of a deployment an operator has
// sealed, and a read of what its listener last published.
func sealedTier(t *testing.T) (*alloc.Allocator, string, func() alloc.ListenerCapacity) {
	t.Helper()

	tiers := []config.Tier{tierOf("a-work", 4)}

	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	if _, err := db.Seal(t.Context(), state.SealRequest{Provenance: state.ProvenanceOperator, Actor: "ops"}); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	published := func() alloc.ListenerCapacity {
		t.Helper()

		got, err := a.CapacityReport(t.Context(), tiers[0].Label)
		if err != nil {
			t.Fatalf("CapacityReport: %v", err)
		}

		return got.Listener
	}

	return a, tiers[0].Label, published
}

// datedNone is whether a published count is a known none with a date, which is
// what a drain trusts as GitHub's word after its seal.
func datedNone(c alloc.ListenerCapacity) bool {
	return c.Unstarted != nil && *c.Unstarted == 0 && c.UnstartedAt != ""
}

// A DELIVERY'S COUNT IS UNKNOWN IN THE LEDGER BEFORE IT IS HANDLED (#140). Two
// empty withdrawn polls date a count of none; the third returns a message, and
// handling it can release the last runner (a completion) while an assignment in
// it waits. Read when that message is first reconciled, the ledger must not
// still show the dated none.
func TestADeliveryUndatesTheCountBeforeItIsHandled(t *testing.T) {
	t.Parallel()

	a, label, published := sealedTier(t)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		polls               atomic.Int32
		l                   *Listener
		armed               bool
		before, atReconcile alloc.ListenerCapacity
		reconciled          bool
	)

	session := &fakeSession{stats: &Statistics{}, onPoll: func(int) {
		if polls.Add(1) >= 5 {
			cancel()
		}
	}, onGet: func() (*Message, error) {
		if polls.Load() != 3 {
			return nil, ErrNoMessage
		}

		before, armed = published(), true

		return directlyAssigned(1, "J-pre-seal", 1), nil
	}}

	l = NewListener(a, label, session, WithRunner(&launchLog{}), WithLogger(slog.New(slog.DiscardHandler)))
	l.beforePoolReconcile = func() {
		if armed {
			atReconcile, reconciled, armed = published(), true, false
		}
	}

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if polls.Load() < 5 {
		t.Fatalf("the listener stopped after %d polls, before the case played out", polls.Load())
	}

	if !datedNone(before) {
		t.Fatalf("before the delivery the count was %v dated %q, want a dated none, or the delivery "+
			"has nothing to undo", before.Unstarted, before.UnstartedAt)
	}

	if !reconciled {
		t.Fatal("the delivery was never reconciled, so this case shows nothing")
	}

	if datedNone(atReconcile) {
		t.Errorf("while the delivery was handled the ledger still showed the dated none from before it "+
			"(%d at %q)", *atReconcile.Unstarted, atReconcile.UnstartedAt)
	}
}

// A REPLACED SESSION VOUCHES FOR NOTHING (#140). A sealed tier's count is a
// dated none when its poll fails; the replacement session opens with no
// statistics, so nothing counts again, and the dated none of the failed session
// must not stand where a drain reads it.
func TestAReplacedSessionDoesNotKeepTheOldDate(t *testing.T) {
	fastSessionReopen(t)

	a, label, published := sealedTier(t)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		polls        atomic.Int32
		beforeFail   alloc.ListenerCapacity
		afterReopen  alloc.ListenerCapacity
		secondPolled bool
	)

	first := &fakeSession{stats: &Statistics{}, onPoll: func(int) { polls.Add(1) }, onGet: func() (*Message, error) {
		if polls.Load() < 3 {
			return nil, ErrNoMessage
		}

		beforeFail = published()

		return nil, errBrokerTimedOut
	}}

	second := &fakeSession{onPoll: func(int) {
		if !secondPolled {
			afterReopen, secondPolled = published(), true
		}

		cancel()
	}}

	l := NewListener(a, label, first, WithRunner(&launchLog{}), WithLogger(slog.New(slog.DiscardHandler)),
		WithRunnerRegistry(&fakeRunnerRegistry{}),
		WithSessionReopen(func(context.Context) (Session, error) { return second, nil }),
		stopsWithoutWaiting())

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if !datedNone(beforeFail) {
		t.Fatalf("when the poll failed the count was %v dated %q, want a dated none, or the reopen has "+
			"nothing to undo", beforeFail.Unstarted, beforeFail.UnstartedAt)
	}

	if !secondPolled {
		t.Fatal("the replacement session was never polled, so this case shows nothing")
	}

	if datedNone(afterReopen) {
		t.Errorf("after the session was replaced the ledger still showed the failed session's dated none "+
			"(%d at %q)", *afterReopen.Unstarted, afterReopen.UnstartedAt)
	}
}
