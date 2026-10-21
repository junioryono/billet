package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// roundTrip is an http.RoundTripper made of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// unheardClient is a client whose every request fails before the plane answers,
// recording when each left on the bubble's clock.
func unheardClient(t *testing.T, sent *[]time.Time) *Client {
	t.Helper()

	c, err := New(Options{Base: "http://plane.invalid", Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	c.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		*sent = append(*sent, time.Now())

		return nil, errors.New("the plane did not answer")
	})}

	return c
}

var unheardDestroy = nodeapi.Command{ID: "d1", Kind: nodeapi.CommandDestroy}

// THE RESENDS WAIT ONE SECOND AND THEN TWO, on the bubble's clock: each gap
// exactly, so neither a busy plane asked again at once nor a longer stall passes.
func TestADestroysResendsWaitASecondAndThenTwo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sent []time.Time
		c := unheardClient(t, &sent)

		if err := reportOutcome(t.Context(), t.Context(), c, nil, slog.New(slog.DiscardHandler), &sync.Mutex{},
			unheardDestroy, nodeapi.CommandResult{ID: "d1", OK: true}); err != nil {
			t.Fatalf("reportOutcome = %v", err)
		}

		if len(sent) != 3 || sent[1].Sub(sent[0]) != time.Second || sent[2].Sub(sent[1]) != 2*time.Second {
			t.Errorf("the sends left at %v, want three: a second apart and then two", sent)
		}
	})
}

// THE SECOND SIGNAL ENDS A WAIT THAT HAS BEGUN, at once and with nothing sent
// after it. The report is first shown blocked in its wait before the second
// send, and only then signalled; it must return without the bubble's clock
// moving.
func TestTheSecondSignalEndsTheWaitBeforeAResend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hurry, second := context.WithCancel(t.Context())
		defer second()

		var sent []time.Time
		c := unheardClient(t, &sent)

		done := make(chan error, 1)

		go func() {
			done <- reportOutcome(t.Context(), hurry, c, nil, slog.New(slog.DiscardHandler), &sync.Mutex{},
				unheardDestroy, nodeapi.CommandResult{ID: "d1", OK: true})
		}()

		// DURABLY BLOCKED: the first send has failed and the report waits for its
		// second, which the bubble's clock has not reached.
		synctest.Wait()

		if len(sent) != 1 {
			t.Fatalf("%d sends before the wait, want the first alone", len(sent))
		}

		blocked := time.Now()

		second()
		synctest.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reportOutcome = %v", err)
			}
		default:
			t.Fatal("the report was still waiting after the second signal")
		}

		if waited := time.Since(blocked); waited != 0 || len(sent) != 1 {
			t.Errorf("after the second signal the report waited %s and sent %d in all, want no wait and the one",
				waited, len(sent))
		}
	})
}

// WHICH REPORTS ARE SENT AGAIN: a destroy's that was not heard, and nothing
// else. A verdict about this node is the same answer the next time, and every
// other command's report is either custody (a launch) or one the plane does
// not wait on.
func TestOnlyADestroysUnheardReportIsSentAgain(t *testing.T) {
	t.Parallel()

	unheard := fmt.Errorf("nodeclient: POST /v1/nodes/n1/result: %w", context.DeadlineExceeded)
	destroy := nodeapi.Command{Kind: nodeapi.CommandDestroy}

	if !retriesReport(destroy, unheard) {
		t.Error("a destroy's report the plane did not answer is not sent again")
	}

	for _, verdict := range []error{ErrUnregistered, ErrRefused, ErrSuperseded, ErrUnauthenticated} {
		if retriesReport(destroy, fmt.Errorf("%w: from the plane", verdict)) {
			t.Errorf("a destroy's report answered %v is sent again, to hear the same verdict", verdict)
		}
	}

	for _, kind := range []nodeapi.CommandKind{
		nodeapi.CommandLaunch, nodeapi.CommandSweep, nodeapi.CommandTend,
		nodeapi.CommandUpgrade, nodeapi.CommandInventory,
	} {
		if retriesReport(nodeapi.Command{Kind: kind}, unheard) {
			t.Errorf("a %s report is sent again; only a destroy's is", kind)
		}
	}

	if retriesReport(destroy, errors.Join(unheard, ErrRefused)) {
		t.Error("a verdict joined to a transport error is sent again")
	}
}
