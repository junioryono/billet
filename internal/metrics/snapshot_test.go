package metrics

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var testFamilies = []Family{
	{Name: "billet_test_leases", Help: "A test gauge.", Labels: []string{"tier"}},
	{Name: "billet_test_nodes", Help: "Another.", Labels: nil},
}

func serveSnapshot(t *testing.T, timeout time.Duration, read func(context.Context) (Snapshot, error)) *Server {
	t.Helper()

	reg, err := New("server")
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.RegisterSnapshot(t.Context(), "ledger", testFamilies, timeout, read); err != nil {
		t.Fatal(err)
	}

	srv, err := Listen(t.Context(), "127.0.0.1:0", reg.Handler(false))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()

		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	return srv
}

// A SOURCE'S SAMPLES ARE SERVED, AND SO IS THAT IT WAS READ.
func TestASnapshotIsServedWithItsSource(t *testing.T) {
	t.Parallel()

	srv := serveSnapshot(t, time.Second, func(context.Context) (Snapshot, error) {
		return Snapshot{
			"billet_test_leases": {{Labels: []string{"small"}, Value: 2}, {Labels: []string{"large"}, Value: 0}},
			"billet_test_nodes":  {{Value: 3}},
		}, nil
	})

	_, body := get(t, srv, "/metrics")

	for _, want := range []string{
		`billet_test_leases{tier="small"} 2`, `billet_test_leases{tier="large"} 0`,
		"billet_test_nodes 3", `billet_scrape_up{source="ledger"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not carry %q", want)
		}
	}
}

// COULD NOT TELL IS NOT ZERO: a failed read serves none of its families, and
// says so.
func TestAFailedReadServesAGapNotZeros(t *testing.T) {
	t.Parallel()

	srv := serveSnapshot(t, time.Second, func(context.Context) (Snapshot, error) {
		return Snapshot{"billet_test_nodes": {{Value: 0}}}, errors.New("the ledger is away")
	})

	code, body := get(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics answered %d; a failed source must not fail the runtime's metrics", code)
	}

	if !strings.Contains(body, `billet_scrape_up{source="ledger"} 0`) {
		t.Error("a failed read does not report billet_scrape_up 0")
	}

	if strings.Contains(body, "billet_test_nodes ") {
		t.Error("a failed read still served its families")
	}

	if !strings.Contains(body, "go_goroutines ") {
		t.Error("a failed source took the runtime's metrics with it")
	}
}

// THE READ IS BOUNDED: a source that never answers ends at its timeout and is
// reported as failed.
func TestASlowReadEndsAtItsTimeout(t *testing.T) {
	t.Parallel()

	srv := serveSnapshot(t, 50*time.Millisecond, func(ctx context.Context) (Snapshot, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("the read was given no deadline")
		}

		<-ctx.Done()

		return nil, ctx.Err()
	})

	done := make(chan string, 1)

	go func() {
		_, body := get(t, srv, "/metrics")
		done <- body
	}()

	select {
	case body := <-done:
		if !strings.Contains(body, `billet_scrape_up{source="ledger"} 0`) {
			t.Error("a read that timed out is not reported as failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scrape did not end at the read's timeout")
	}
}

// A SAMPLE THAT DOES NOT FIT ITS FAMILY IS LEFT OUT, and the rest are served.
func TestASampleThatDoesNotFitItsFamilyIsLeftOut(t *testing.T) {
	t.Parallel()

	srv := serveSnapshot(t, time.Second, func(context.Context) (Snapshot, error) {
		return Snapshot{
			"billet_test_leases":  {{Labels: []string{"small", "extra"}, Value: 9}, {Labels: []string{"large"}, Value: 1}},
			"billet_test_unknown": {{Value: 7}},
			"billet_test_nodes":   {{Value: 3}},
		}, nil
	})

	_, body := get(t, srv, "/metrics")

	if strings.Contains(body, " 9\n") || strings.Contains(body, "billet_test_unknown") {
		t.Errorf("a sample that does not fit its family was served:\n%s", body)
	}

	if !strings.Contains(body, `billet_test_leases{tier="large"} 1`) || !strings.Contains(body, "billet_test_nodes 3") {
		t.Error("the samples that fit were not served beside the one that did not")
	}
}
