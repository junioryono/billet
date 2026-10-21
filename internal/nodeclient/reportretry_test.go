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
// counting each one, with onSend run as each leaves.
func unheardClient(t *testing.T, sends *int, onSend func()) *Client {
	t.Helper()

	c, err := New(Options{Base: "http://plane.invalid", Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	c.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		*sends++
		onSend()

		return nil, errors.New("the plane did not answer")
	})}

	return c
}

// THE RESENDS WAIT ONE SECOND AND THEN TWO, on the bubble's clock: exactly that,
// so neither a busy plane asked again at once nor a longer stall passes.
func TestADestroysResendsWaitASecondAndThenTwo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sends := 0
		c := unheardClient(t, &sends, func() {})

		start := time.Now()
		destroy := nodeapi.Command{ID: "d1", Kind: nodeapi.CommandDestroy}

		if err := reportOutcome(t.Context(), t.Context(), c, nil, slog.New(slog.DiscardHandler), &sync.Mutex{},
			destroy, nodeapi.CommandResult{ID: "d1", OK: true}); err != nil {
			t.Fatalf("reportOutcome = %v", err)
		}

		if waited := time.Since(start); sends != 3 || waited != 3*time.Second {
			t.Errorf("%d sends over %s, want 3 over 3s (a second, then two)", sends, waited)
		}
	})
}

// THE SECOND SIGNAL ENDS A WAIT THAT HAS BEGUN, at once and with nothing sent
// after it: here it arrives during the first send, so the wait it ends is the
// one before the second.
func TestTheSecondSignalEndsTheWaitBeforeAResend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hurry, second := context.WithCancel(t.Context())
		defer second()

		sends := 0
		c := unheardClient(t, &sends, second)

		start := time.Now()
		destroy := nodeapi.Command{ID: "d1", Kind: nodeapi.CommandDestroy}

		if err := reportOutcome(t.Context(), hurry, c, nil, slog.New(slog.DiscardHandler), &sync.Mutex{},
			destroy, nodeapi.CommandResult{ID: "d1", OK: true}); err != nil {
			t.Fatalf("reportOutcome = %v", err)
		}

		if waited := time.Since(start); sends != 1 || waited != 0 {
			t.Errorf("%d sends over %s after the second signal, want the one it interrupted and no wait",
				sends, waited)
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
