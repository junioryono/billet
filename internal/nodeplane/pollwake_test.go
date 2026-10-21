package nodeplane

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A POLL WITH NOTHING QUEUED WAITS, EVEN WHEN A SIGNAL WAS LEFT BEHIND. Queuing a
// command signals the node's one-slot wake without asking whether anybody is
// polling, so a command a poll took without waiting leaves its signal, and the
// next poll woke on it at once and answered empty instead of waiting out its
// window. TestACompletionsCacheAuthorityRidesOnItsDestroy failed on exactly that
// under load (#188, runs 39493076702 and 39535039226): its first destroy was
// queued before its poll began, and the second poll returned nothing.
func TestAPollDoesNotWakeOnTheSignalOfACommandAlreadyTaken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := testPlane(t, WithCommandTimeout(5*time.Second), WithPollTimeout(time.Minute))
		register(t, p, "n1", config.ProviderDocker)

		done := make(chan error, 1)

		// QUEUED BEFORE ANY POLL, and taken without waiting: its signal stays.
		go func() { done <- p.NewRunner().Destroy(t.Context(), 7) }()
		synctest.Wait()

		cmd, took, err := p.Poll(t.Context(), "n1", "")
		if err != nil || !took || cmd.Kind != nodeapi.CommandDestroy || cmd.RequestID != 7 {
			t.Fatalf("the first poll = %+v, %v, %v, want the destroy queued before it", cmd, took, err)
		}

		if err := p.Result("n1", "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			t.Fatalf("result: %v", err)
		}

		if err := <-done; err != nil {
			t.Fatalf("the first destroy: %v", err)
		}

		var (
			next      nodeapi.Command
			nextTook  bool
			nextErr   error
			answered  = make(chan struct{})
			startedAt = time.Now()
		)

		go func() {
			next, nextTook, nextErr = p.Poll(t.Context(), "n1", "")
			close(answered)
		}()

		// DURABLY BLOCKED, or it has answered on the stale signal.
		synctest.Wait()

		select {
		case <-answered:
			t.Fatalf("a poll with nothing queued answered at once: %+v, %v, %v", next, nextTook, nextErr)
		default:
		}

		// AND THE NEXT COMMAND IS WHAT ANSWERS IT, without the clock moving.
		go func() { done <- p.NewRunner().Destroy(t.Context(), 8) }()
		synctest.Wait()

		select {
		case <-answered:
		default:
			t.Fatal("the waiting poll did not take the destroy queued for it")
		}

		if nextErr != nil || !nextTook || next.RequestID != 8 || time.Since(startedAt) != 0 {
			t.Fatalf("the second poll = %+v, %v, %v after %s, want the second destroy at once",
				next, nextTook, nextErr, time.Since(startedAt))
		}

		if err := p.Result("n1", "", nodeapi.CommandResult{ID: next.ID, OK: true}); err != nil {
			t.Fatalf("result: %v", err)
		}

		if err := <-done; err != nil {
			t.Fatalf("the second destroy: %v", err)
		}
	})
}
