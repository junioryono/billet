package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
)

// sessionReopenFirst and sessionReopenMax pace recoverSession. Vars so a test can
// drive a recovery without waiting it out.
var (
	sessionReopenFirst = 5 * time.Second
	sessionReopenMax   = 5 * time.Minute
)

// sessionFatal reports an error no session recovery may absorb, and which stops
// the control plane: a response billet cannot act on leaves it unable to tell
// which of its commitments are real, and a process that is no longer the
// controller must act on nothing.
func sessionFatal(err error) bool {
	return errors.Is(err, ErrUntrustworthySession) || errors.Is(err, state.ErrLeadershipLost)
}

// reopenDelay is the wait before the nth consecutive attempt: doubling from
// sessionReopenFirst, capped at sessionReopenMax.
func reopenDelay(n int) time.Duration {
	delay := sessionReopenFirst
	for i := 1; i < n && delay < sessionReopenMax; i++ {
		delay *= 2
	}

	return min(delay, sessionReopenMax)
}

// recoverOrStop runs recoverSession for Run's loop and reports whether Run must
// return, and with what. DURING A DRAIN TOO, on the drain's context: a drain ends
// on completions, and only a session can deliver them.
func (l *Listener) recoverOrStop(ctx, pollCtx context.Context, draining bool, cause error) (bool, error) {
	err := l.recoverSession(pollCtx, cause)

	// AS ITSELF, even in a drain or a shutdown: stopping would report it as the
	// cancellation, and the control plane as a clean stop.
	if sessionFatal(err) {
		return true, err
	}

	if err == nil || cancelledWhileServing(ctx, draining, err) || l.drainEnded(pollCtx, draining, err) {
		return false, nil
	}

	return true, stopping(ctx, err)
}

// recoverSession replaces this listener's session after a poll failed past the
// client's own retries, waiting with backoff before each attempt, until one opens,
// a fatal error arrives, or ctx ends.
//
// EVERYTHING THE LISTENER HOLDS STAYS HELD. Running leases keep their heartbeat
// and their compute, promises stay owed, and the cleanup loop keeps retrying, so
// nothing about a broker outage can free capacity under a running job. Only idle
// escrow goes back, and only once the old session is known closed, because until
// then GitHub can assign against the advertisement it carries.
//
// A NEW SESSION IS A RESTART'S VIEW OF THE QUEUE: the cursor starts at zero, a
// hold on a message the old session delivered is dropped because GitHub
// redelivers whatever was not acknowledged (measured, 2026-09-04), and the
// statistics are the new session's, including none.
func (l *Listener) recoverSession(ctx context.Context, cause error) error {
	l.recoverCause = cause

	for {
		l.sessionFailures++
		delay := reopenDelay(l.sessionFailures)

		l.log.Error("this tier's message session failed after the client's own retries, so "+
			"GitHub is not assigning it work; reopening it. Every other tier keeps serving, "+
			"and this one keeps its running jobs and their leases",
			"tier", l.tier, "failures", l.sessionFailures, "retry_in", delay, "error", cause)

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return l.interrupted(ctx)
		case <-timer.C:
		}

		if l.fenced() {
			return l.fencedRecovery(cause)
		}

		closedNow, err := l.closeReplaced(ctx)

		switch {
		case err != nil && ctx.Err() != nil:
			return l.interrupted(ctx)
		case err != nil:
			l.log.Warn("could not close the failed message session; its replacement waits "+
				"for GitHub to expire it, and idle escrow is kept until a poll lands",
				"tier", l.tier, "error", err)
		case closedNow:
			l.releaseIdleEscrow(ctx)
		}

		// AGAIN, IMMEDIATELY BEFORE THE OPEN: the close can take its whole grace.
		if l.fenced() {
			return l.fencedRecovery(cause)
		}

		session, err := l.openReplacement(ctx)
		if err != nil {
			// BEFORE THE CANCELLATION: the result has been taken off its channel,
			// so a fatal answer dropped here would never be seen again.
			if sessionFatal(err) {
				return fmt.Errorf("server: reopen session for %s: %w", l.tier, err)
			}

			if ctx.Err() != nil {
				return l.interrupted(ctx)
			}

			if l.fenced() {
				return l.fencedRecovery(err)
			}

			cause = err
			l.recoverCause = err

			continue
		}

		l.install(session)

		return nil
	}
}

// install makes session this listener's, as a restart would find it.
func (l *Listener) install(session Session) {
	l.session = session
	l.sessionClosed = false
	l.closing = nil
	l.recoverCause = nil
	l.lastMessageID = 0
	l.observed = session.Statistics()

	l.mu.Lock()
	l.heldMessageID = nil
	l.mu.Unlock()

	l.log.Warn("opened a new message session for this tier after its last one failed",
		"tier", l.tier, "failures", l.sessionFailures)
}

// interrupted is what recoverSession returns when ctx ends. An open that has
// already answered is taken first, whichever wait the cancellation cut short: a
// fatal answer is reported as itself, and a session it delivered is installed, so
// neither is left on a channel nothing will read again.
func (l *Listener) interrupted(ctx context.Context) error {
	if l.opening != nil {
		select {
		case opened := <-l.opening:
			l.opening = nil

			if sessionFatal(opened.err) {
				return fmt.Errorf("server: reopen session for %s: %w", l.tier, opened.err)
			}

			if opened.err == nil {
				l.install(opened.session)

				return nil
			}
		default:
		}
	}

	return ctx.Err()
}

// fencedRecovery is the error a recovery stops with once this process is no
// longer the controller.
func (l *Listener) fencedRecovery(cause error) error {
	return fmt.Errorf("server: not reopening the session for %s: %w; the poll had failed with: %w",
		l.tier, state.ErrLeadershipLost, cause)
}

// errCloseInFlight means a session close outlived its grace and its outcome is
// not yet known.
var errCloseInFlight = errors.New("server: the session close is still in flight past its grace")

// closeReplaced closes the session recoverSession is replacing, reporting whether
// this call is the one that saw it close. A close already in flight is awaited
// rather than started again.
func (l *Listener) closeReplaced(ctx context.Context) (bool, error) {
	if l.sessionClosed {
		return false, nil
	}

	if l.closing == nil {
		l.startClose(context.WithTimeout(context.WithoutCancel(ctx), l.closeGrace))
	}

	return l.awaitClose(ctx)
}

// startClose closes the current session aside on closeCtx, whose cancel the
// close owns.
func (l *Listener) startClose(closeCtx context.Context, endClose context.CancelFunc) {
	done := make(chan error, 1)
	session := l.session

	go func() {
		defer endClose()

		done <- session.Close(closeCtx)
	}()

	l.closing = done
}

// awaitClose waits up to the close grace for the close in flight. One still
// running when the wait ends stays in flight, an unknown outcome that licenses no
// release.
func (l *Listener) awaitClose(ctx context.Context) (bool, error) {
	wait := time.NewTimer(l.closeGrace)
	defer wait.Stop()

	select {
	case err := <-l.closing:
		l.closing = nil
		if err != nil {
			return false, err
		}

		l.sessionClosed = true

		return true, nil
	case <-wait.C:
		return false, errCloseInFlight
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// openReplacement opens the replacement session, or waits for the open an
// earlier attempt started. A session that open delivers after this listener has
// stopped waiting for good is never closed, and GitHub expires it.
func (l *Listener) openReplacement(ctx context.Context) (Session, error) {
	if l.opening == nil {
		done := make(chan openedSession, 1)
		open := l.reopen

		go func() {
			session, err := open(ctx)
			done <- openedSession{session: session, err: err}
		}()

		l.opening = done
	}

	select {
	case opened := <-l.opening:
		l.opening = nil

		return opened.session, opened.err
	case <-ctx.Done():
		// A RESULT READY BESIDE THE CANCELLATION IS LEFT FOR interrupted, which
		// takes it before the recovery reports the cancellation.
		return nil, ctx.Err()
	}
}

// cancelledWhileServing reports whether a failed call is the shutdown arriving
// mid-call rather than a failure to report.
//
// Neither obvious test works. The clock alone swallows fatal errors that coincide
// with a cancellation; context.Canceled alone misses one landing inside handle(),
// which surfaces as a domain error that does not wrap it — so the drain is skipped
// intermittently for the most ordinary reason there is.
//
// So: once stopping, an error IS the shutdown unless it is one billet must stop for
// regardless. A scale-set response it cannot act on is the only one, and a
// response is not what failed when the error also carries the cancellation: a
// runner lookup cut short by it is wrapped as untrustworthy, and answering that
// with a stop skipped the drain and left the tier's running jobs unwaited-for.
func cancelledWhileServing(ctx context.Context, draining bool, err error) bool {
	if draining || ctx.Err() == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return !errors.Is(err, ErrUntrustworthySession)
}

// Backlog is what GitHub last said was assigned to this scale set and not yet
// finished.
//
// TotalAssignedJobs is the documented scaling signal; counting messages is not,
// because a response carries at most 50 and a large backlog is truncated.
func (l *Listener) Backlog() int {
	if l.observed == nil {
		return 0
	}

	return l.observed.TotalAssignedJobs
}

// reportOrphanedBacklog says out loud when GitHub believes this scale set is already
// running work billet has no lease for.
//
// A fresh listener holds nothing, so a non-zero TotalAssignedJobs means jobs were
// assigned before the process restarted. The ones still waiting are reassigned at
// the pickup deadline; the ones a dead runner had STARTED are not, and fail.
//
// Deliberately NOT a failure — those jobs are already lost, and refusing to start
// would strand the tier's remaining capacity too.
func (l *Listener) reportOrphanedBacklog() {
	backlog := l.Backlog()
	if backlog == 0 {
		return
	}

	l.log.Warn("github reports jobs already assigned to this scale set that billet has no lease "+
		"for; they were assigned before this process started. Any that no runner had picked up "+
		"are reassigned when github's pickup deadline passes; any that were already running "+
		"have lost their runner and will fail",
		"tier", l.tier, "assigned", backlog)
}

// acknowledge tells GitHub the message was handled. An unacknowledged message is
// redelivered, which is why everything above it has to be idempotent.
func (l *Listener) acknowledge(ctx context.Context, msg *Message) error {
	if err := l.session.DeleteMessage(ctx, msg.MessageID); err != nil {
		return fmt.Errorf("server: acknowledge message %d: %w", msg.MessageID, err)
	}
	l.mu.Lock()
	if l.heldMessageID != nil && *l.heldMessageID == msg.MessageID {
		l.heldMessageID = nil
	}
	l.mu.Unlock()

	return nil
}

// messageHeld also covers holds discovered before an operational failure.
func (l *Listener) messageHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.heldMessageID != nil
}

// acknowledgeCompletions records that GitHub will not redeliver the
// source message. The store removes a row only once it is also retired.
func (l *Listener) acknowledgeCompletions(ctx context.Context, msg *Message) {
	persistCtx, cancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer cancel()
	for i := range msg.Completed {
		job := &msg.Completed[i]
		if l.completionStore != nil {
			if err := l.completionStore.AcknowledgePendingCompletion(
				persistCtx, l.tier, job.RequestID, msg.MessageID,
			); err != nil {
				l.log.Error("a completion acknowledgement could not be made durable; recovery remains conservatively blocked",
					"tier", l.tier, "request", job.RequestID, "message", msg.MessageID, "error", err)
			}
		}
		if l.alloc != nil {
			if err := l.alloc.AcknowledgePoolRunner(persistCtx, l.tier, job.RequestID); err != nil {
				l.log.Error("a pool runner acknowledgement could not be made durable; its retired identity remains conservatively reserved",
					"tier", l.tier, "request", job.RequestID, "message", msg.MessageID, "error", err)
			}
		}
	}
}
