package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/state"
)

// workTier is one 4 vCPU tier on one host, a read of what its listener last
// published, and the operator's seal of the deployment.
func workTier(t *testing.T) (*alloc.Allocator, string, func() alloc.ListenerCapacity, func()) {
	t.Helper()

	tiers := []config.Tier{tierOf("a-work", 4)}

	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	published := func() alloc.ListenerCapacity {
		t.Helper()

		got, err := a.CapacityReport(t.Context(), tiers[0].Label)
		if err != nil {
			t.Fatalf("CapacityReport: %v", err)
		}

		return got.Listener
	}

	seal := func() {
		t.Helper()

		if _, err := db.Seal(t.Context(), state.SealRequest{Provenance: state.ProvenanceOperator, Actor: "ops"}); err != nil {
			t.Fatalf("Seal: %v", err)
		}
	}

	return a, tiers[0].Label, published, seal
}

// sealedTier is workTier already sealed.
func sealedTier(t *testing.T) (*alloc.Allocator, string, func() alloc.ListenerCapacity) {
	t.Helper()

	a, label, published, seal := workTier(t)
	seal()

	return a, label, published
}

// datedNone is whether a published count is a known none with a date, which is
// what a drain trusts as GitHub's word after its seal.
func datedNone(c alloc.ListenerCapacity) bool {
	return c.Unstarted != nil && *c.Unstarted == 0 && c.UnstartedAt != ""
}

// A DELIVERY'S COUNT IS UNKNOWN IN THE LEDGER BEFORE IT IS HANDLED (#140). One
// job runs on the tier's one runner, the deployment is sealed, and withdrawn
// empty polls date a count of none. Then one message completes that job and
// assigns another, which the seal declines: the completion releases the last
// runner while the new job waits. Read inside that completion's destroy, before
// its lease goes, the ledger must not still show the dated none.
func TestADeliveryUndatesTheCountBeforeItIsHandled(t *testing.T) {
	t.Parallel()

	a, label, published, seal := workTier(t)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		polls             atomic.Int32
		before, atDestroy alloc.ListenerCapacity
		destroyed         bool
		acknowledged      []int64
		launched          []int64
	)

	one := &Statistics{TotalAssignedJobs: 1}

	session := &fakeSession{stats: &Statistics{}, onPoll: func(int) {
		if polls.Add(1) >= 8 {
			cancel()
		}
	}, onGet: func() (*Message, error) {
		switch polls.Load() {
		case 1:
			return &Message{MessageID: 1, Statistics: one,
				Assigned: []dispatch.Job{{RequestID: 11, RunID: 101}}}, nil
		case 3:
			seal()
		case 6:
			before = published()

			return &Message{MessageID: 2, Statistics: one,
				Completed: []dispatch.Job{{RequestID: 11, RunID: 101, Result: "succeeded"}},
				Assigned:  []dispatch.Job{{RequestID: 12, RunID: 102}}}, nil
		}

		return nil, ErrNoMessage
	}, onDelete: func(id int64) error {
		acknowledged = append(acknowledged, id)

		return nil
	}}

	l := NewListener(a, label, session, WithLogger(slog.New(slog.DiscardHandler)),
		WithRunner(&fakeRunner{onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		}, onDestroyCompleted: func(int64, string) error {
			atDestroy, destroyed = published(), true

			return nil
		}}), WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if polls.Load() < 8 {
		t.Fatalf("the listener stopped after %d polls, before the case played out", polls.Load())
	}

	if !datedNone(before) {
		t.Fatalf("before the delivery the count was %v dated %q, want a dated none with the job "+
			"running, or the delivery has nothing to undo", before.Unstarted, before.UnstartedAt)
	}

	if !destroyed {
		t.Fatal("the completion never destroyed its runner, so this case shows nothing")
	}

	if datedNone(atDestroy) {
		t.Errorf("while the completion released the last runner the ledger still showed the dated none "+
			"from before the delivery (%d at %q)", *atDestroy.Unstarted, atDestroy.UnstartedAt)
	}

	if !slices.Contains(acknowledged, 2) {
		t.Errorf("acknowledged %v, want the delivery (2) handled and acknowledged", acknowledged)
	}

	// THE SECOND JOB WAITS: the seal bought it nothing, so nothing started.
	if !slices.Equal(launched, []int64{11}) {
		t.Errorf("launched %v, want only the job assigned before the seal (11)", launched)
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
