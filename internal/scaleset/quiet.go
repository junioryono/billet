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
// logger — go-retryablehttp's leveled path, which the client reaches because
// *slog.Logger satisfies its LeveledLogger interface. Stopping billet cancels
// the long poll in flight, so a perfectly clean shutdown ends with:
//
//	level=ERROR msg="request failed" error="context canceled" method=GET url=...
//
// Under a service manager that is the first thing in the journal after every
// restart. An error that appears on every single restart is one operators learn
// to scroll past, and the next one that matters scrolls past with it. The fix is
// not to hide the record — it is still written, with everything it said — but to
// stop calling a thing billet did on purpose an error.
//
// KEYED ON THE ERROR VALUE, NOT THE MESSAGE. A cancellation is always billet's
// own doing, so it is never something an operator can act on; that is a property
// of the error, not of the sentence around it. Matching "request failed" would
// rot the first time upstream reworded it, and would be a rule about a string
// rather than about a fact.
func quieten(h slog.Handler) slog.Handler {
	return &quietHandler{Handler: h}
}

type quietHandler struct {
	slog.Handler
}

func (q *quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo && cancelled(r) {
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

// cancelled reports whether a record carries an error that is a cancellation.
func cancelled(r slog.Record) bool {
	found := false

	r.Attrs(func(a slog.Attr) bool {
		err, ok := a.Value.Any().(error)
		if !ok {
			return true
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			found = true

			return false
		}

		return true
	})

	return found
}
