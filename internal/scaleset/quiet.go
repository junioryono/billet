package scaleset

import (
	"context"
	"errors"
	"log/slog"
)

// quieten wraps a handler so a record carrying a cancellation is logged at Debug
// rather than at whatever level it was written with.
//
// THE VENDORED CLIENT LOGS EVERY FAILED REQUEST AT ERROR, through billet's own
// logger — go-retryablehttp's leveled path, which it reaches because *slog.Logger
// satisfies its LeveledLogger interface. Stopping billet cancels the long poll in
// flight, so a clean shutdown ends with:
//
//	level=ERROR msg="request failed" error="context canceled" method=GET url=...
//
// An error that appears after every single restart is one operators learn to
// scroll past, and the next one that matters scrolls past with it. The record is
// still written, with everything it said; it is only no longer called an error.
//
// KEYED ON THE ERROR VALUE, NOT THE MESSAGE — the cancellation being billet's own
// doing is a property of the error, not of the sentence around it. CANCELLATION
// ONLY, NOT DEADLINES: a request that ran out of time means GitHub is slow or
// unreachable, which is exactly what an operator needs to see. AND ONLY WHEN THAT
// IS ALL IT IS — errors.Join(realFailure, context.Canceled) satisfies errors.Is
// while carrying a genuine failure, so a joined error is demoted only when every
// branch of it is a cancellation.
func quieten(h slog.Handler) slog.Handler {
	return &quietHandler{Handler: h}
}

type quietHandler struct {
	slog.Handler
}

func (q *quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo && onlyCancellations(r) {
		// A NEW RECORD, because slog.Record.Level is a field on a value the caller
		// still owns; mutating the one handed in would be a data race with any
		// other handler in the chain.
		demoted := slog.NewRecord(r.Time, slog.LevelDebug, r.Message, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			demoted.AddAttrs(a)

			return true
		})

		return q.Handler.Handle(ctx, demoted)
	}

	return q.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must rewrap, or the demotion is lost the moment
// anything calls logger.With — which the client does. A handler that only works
// before the first With is a handler that does not work.
func (q *quietHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &quietHandler{Handler: q.Handler.WithAttrs(attrs)}
}

func (q *quietHandler) WithGroup(name string) slog.Handler {
	return &quietHandler{Handler: q.Handler.WithGroup(name)}
}

// Enabled reports on the DEMOTED level as well as the written one.
//
// Without this a record that is about to become Debug is refused by the
// underlying handler at its original level — at the default Info threshold, that
// is the whole point of the exercise disappearing before it can happen.
func (q *quietHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return q.Handler.Enabled(ctx, level) || q.Handler.Enabled(ctx, slog.LevelDebug)
}

// onlyCancellations reports whether a record carries at least one error and
// every error it carries is nothing but a cancellation.
func onlyCancellations(r slog.Record) bool {
	found := false
	all := true

	r.Attrs(func(a slog.Attr) bool {
		err, ok := a.Value.Any().(error)
		if !ok {
			return true
		}

		found = true

		if !onlyCancellation(err) {
			all = false

			return false
		}

		return true
	})

	return found && all
}

// onlyCancellation reports whether an error is a cancellation and nothing else.
//
// A joined error — errors.Join, or fmt.Errorf with several %w — unwraps to a
// list, and errors.Is is satisfied by ANY branch matching. That is the wrong
// question here: a failure joined with a cancellation is still a failure, and
// demoting it would hide the half that matters. Every branch has to be a
// cancellation for the whole to be one.
func onlyCancellation(err error) bool {
	if err == nil {
		return false
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		branches := joined.Unwrap()
		if len(branches) == 0 {
			return false
		}

		for _, branch := range branches {
			if !onlyCancellation(branch) {
				return false
			}
		}

		return true
	}

	return errors.Is(err, context.Canceled)
}
