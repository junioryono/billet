package supervise

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// WAIT RETURNS ONLY ONCE EVERY TASK HAS, background ones included, so what the
// tasks use can be closed after it. Each task records its return; a Wait that
// came back before a task's return would find that task unrecorded.
func TestWaitReturnsOnlyOnceEveryTaskHas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu    sync.Mutex
			ended []string
		)

		record := func(name string) {
			mu.Lock()
			defer mu.Unlock()

			ended = append(ended, name)
		}

		g := New(t.Context(), quiet())
		g.Background("loop", func(ctx context.Context) {
			<-ctx.Done()
			// Still unwinding after its cancellation, as a loop finishing a pass is.
			time.Sleep(time.Second)
			record("background")
		})
		g.Essential(func(context.Context) error {
			time.Sleep(time.Second)
			record("essential")

			return nil
		})

		if errs := g.Wait(); len(errs) != 0 {
			t.Fatalf("Wait = %v, want no errors", errs)
		}

		mu.Lock()
		defer mu.Unlock()

		if !slices.Equal(ended, []string{"essential", "background"}) {
			t.Errorf("tasks ended %v by the time Wait returned, want the essential task and then the background one", ended)
		}
	})
}

// AN ESSENTIAL TASK'S ERROR STOPS EVERY ESSENTIAL TASK, AND NO BACKGROUND TASK
// UNTIL THEY HAVE ALL RETURNED: a lease renewer running beside the tiers must
// keep renewing while a tier stopped by another's failure unwinds.
func TestAnEssentialFailureStopsTheOthersButNotTheBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failed := errors.New("tier a failed")
		sides := make(chan context.Context, 1)
		unwind := make(chan struct{})

		var sideEndedEarly bool

		g := New(t.Context(), quiet())
		g.Background("renewer", func(ctx context.Context) {
			sides <- ctx
			<-ctx.Done()
		})

		side := <-sides

		g.Essential(func(context.Context) error { return failed })
		g.Essential(func(ctx context.Context) error {
			<-ctx.Done()
			// Unwinding: the other tier's failure reached this one, and the
			// background task must still be running.
			<-unwind
			sideEndedEarly = side.Err() != nil

			return ctx.Err()
		})

		synctest.Wait()
		close(unwind)

		errs := g.Wait()
		if len(errs) != 2 || !errors.Is(errs[0], failed) || !errors.Is(errs[1], context.Canceled) {
			t.Errorf("Wait = %v, want the failure and then the cancellation it caused", errs)
		}

		if sideEndedEarly {
			t.Error("the background task was stopped while an essential task was still unwinding")
		}

		if side.Err() == nil {
			t.Error("the background task's context was still live after Wait")
		}
	})
}

// A BACKGROUND TASK'S RETURN STOPS NOTHING, and one that returned before it was
// asked to is logged by name.
func TestABackgroundReturnStopsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logged bytes.Buffer

		ctx, cancel := context.WithCancel(t.Context())

		g := New(ctx, slog.New(slog.NewTextHandler(&logged, nil)))
		works := make(chan context.Context, 1)

		g.Background("barrier loop", func(context.Context) {})
		g.Essential(func(ctx context.Context) error {
			works <- ctx
			<-ctx.Done()

			return nil
		})

		work := <-works

		synctest.Wait()

		if work.Err() != nil {
			t.Error("a background task's return stopped an essential task")
		}

		cancel()

		if errs := g.Wait(); len(errs) != 0 {
			t.Fatalf("Wait = %v, want none: an essential task returning nil is not a failure", errs)
		}

		if !strings.Contains(logged.String(), "barrier loop") {
			t.Errorf("a background task that returned by itself was not logged by name: %q", logged.String())
		}
	})
}

// A BACKGROUND TASK STOPPED BY ITS CONTEXT IS NOT LOGGED, or every shutdown
// would report each loop as having stopped by itself.
func TestABackgroundTaskAskedToStopIsNotLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logged bytes.Buffer

		g := New(t.Context(), slog.New(slog.NewTextHandler(&logged, nil)))
		g.Background("watch", func(ctx context.Context) { <-ctx.Done() })

		g.Wait()

		if logged.Len() != 0 {
			t.Errorf("a background task asked to stop was logged: %q", logged.String())
		}
	})
}

// WAIT TWICE IS ONE WAIT: a deferred Wait beside an explicit one returns the
// same errors and does not block.
func TestWaitTwiceReturnsTheSameErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failed := errors.New("failed")

		g := New(t.Context(), quiet())
		g.Essential(func(context.Context) error { return failed })

		first, second := g.Wait(), g.Wait()
		if len(first) != 1 || len(second) != 1 || !errors.Is(first[0], failed) || !errors.Is(second[0], failed) {
			t.Errorf("Wait returned %v and then %v, want the one failure both times", first, second)
		}
	})
}
