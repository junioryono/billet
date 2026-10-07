// Package supervise runs a process's long-lived loops as one group that is
// joined before it returns.
//
// Two kinds of task, by what their ending means. An essential task's error
// stops every essential task, which is the control plane's rule that one tier
// failing stops all of them. A background task stops nothing: it runs beside
// the essential ones until all of them have returned, or the group's context
// ends, and a pass that fails inside it is its own to log and retry. Either
// way, Wait returns only once every task the group started has returned, so
// what a loop uses (the ledger, a listener) can be closed after it.
//
// There is no panic recovery. billet panics nowhere on purpose, and a crash
// followed by the service manager's restart is the designed recovery.
package supervise

import (
	"context"
	"log/slog"
	"sync"
)

// Group is a set of tasks started under one context. Its zero value is not
// usable; build one with New.
type Group struct {
	log *slog.Logger

	// work is the essential tasks' context, cancelled by the first of them to
	// fail. side is the background tasks', cancelled once every essential task
	// has returned. Both end with the parent.
	work       context.Context //nolint:containedctx // the group is the scope these contexts belong to; its tasks receive them as arguments and Wait ends both
	cancelWork context.CancelFunc
	side       context.Context //nolint:containedctx // as work
	cancelSide context.CancelFunc

	essential  sync.WaitGroup
	background sync.WaitGroup

	mu   sync.Mutex
	errs []error

	wait sync.Once
	done []error
}

// New returns a group whose tasks all end when ctx does.
func New(ctx context.Context, log *slog.Logger) *Group {
	g := &Group{log: log}
	g.work, g.cancelWork = context.WithCancel(ctx)
	g.side, g.cancelSide = context.WithCancel(ctx)

	return g
}

// Essential starts fn. An error from it cancels every essential task's
// context and is among what Wait returns; nil ends this task alone.
func (g *Group) Essential(fn func(context.Context) error) {
	g.essential.Go(func() {
		if err := fn(g.work); err != nil {
			g.mu.Lock()
			g.errs = append(g.errs, err)
			g.mu.Unlock()

			g.cancelWork()
		}
	})
}

// Background starts fn, whose context ends once every essential task has
// returned or the group's context does. Its return stops nothing; one that
// comes before it was asked to stop is logged under name, because a loop that
// ended by itself has stopped doing its job without saying so.
func (g *Group) Background(name string, fn func(context.Context)) {
	g.background.Go(func() {
		fn(g.side)

		if g.side.Err() == nil {
			g.log.Warn("a background task returned before it was asked to stop", "task", name)
		}
	})
}

// Wait returns once every task has: it waits for the essential tasks, then
// cancels the background ones and waits for them. It returns the essential
// tasks' errors in the order they ended. Calling it again returns the same
// errors at once, so a deferred Wait can stand beside an explicit one; no task
// may be started once it has been called.
func (g *Group) Wait() []error {
	g.wait.Do(func() {
		g.essential.Wait()
		g.cancelWork()

		g.cancelSide()
		g.background.Wait()

		g.mu.Lock()
		g.done = g.errs
		g.mu.Unlock()
	})

	return g.done
}
