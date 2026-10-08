package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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

// getWithin is get with a deadline of the request's own, so a collector that
// stopped bounding its reads fails the test rather than hangs it.
func getWithin(t *testing.T, srv *Server, path string) (string, time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr().String()+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	begun := time.Now()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the scrape did not answer within its own deadline: %v", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body), time.Since(begun)
}

// THE COLLECTOR BOUNDS THE READ ITSELF. A read that ignores its deadline holds
// neither this scrape nor the next past the bound, is reported failed, and no
// second read starts while it runs; once it returns, the next scrape reads
// afresh.
func TestAReadThatNeverAnswersEndsTheScrapeAtItsBound(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	deadlines := make(chan bool, 8)

	var (
		reads    atomic.Int32
		released atomic.Bool
	)

	srv := serveSnapshot(t, 50*time.Millisecond, func(ctx context.Context) (Snapshot, error) {
		n := reads.Add(1)

		_, ok := ctx.Deadline()
		deadlines <- ok

		if n == 1 {
			<-release // deaf to its context, on purpose
		}

		return Snapshot{"billet_test_nodes": {{Value: float64(n)}}}, nil
	})

	// Registered after serveSnapshot's, so it runs first: the read is let go
	// before the endpoint closes, whatever the test concluded.
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})

	for scrape := range 2 {
		body, took := getWithin(t, srv, "/metrics")

		if took > 2*time.Second {
			t.Errorf("scrape %d took %v against a 50ms bound", scrape, took)
		}

		if !strings.Contains(body, `billet_scrape_up{source="ledger"} 0`) {
			t.Errorf("scrape %d did not report the unanswered read as failed", scrape)
		}

		if strings.Contains(body, "billet_test_nodes ") {
			t.Errorf("scrape %d served a family the read never returned", scrape)
		}
	}

	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads started while the first was stuck, want 1", n)
	}

	if !<-deadlines {
		t.Error("the read was given no deadline")
	}

	released.Store(true)
	close(release)

	// ONCE IT RETURNS, A NEW READ ANSWERS: within a bound, a scrape serves the
	// second read's value.
	until := time.Now().Add(5 * time.Second)

	for {
		body, _ := getWithin(t, srv, "/metrics")
		if strings.Contains(body, `billet_scrape_up{source="ledger"} 1`) && strings.Contains(body, "billet_test_nodes 2") {
			break
		}

		if time.Now().After(until) {
			t.Fatalf("no fresh read answered after the stuck one returned:\n%s", body)
		}
	}
}

// TWO SCRAPES AT ONCE BOTH GET AN ANSWER from a read that is slow but within
// its bound: the second waits for the first's read rather than being turned
// away.
func TestConcurrentScrapesShareAHealthyRead(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32

	srv := serveSnapshot(t, 2*time.Second, func(context.Context) (Snapshot, error) {
		reads.Add(1)
		time.Sleep(200 * time.Millisecond) // a slow read, which is the scenario

		return Snapshot{"billet_test_nodes": {{Value: 3}}}, nil
	})

	bodies := make(chan string, 2)

	for range 2 {
		go func() {
			body, _ := getWithin(t, srv, "/metrics")
			bodies <- body
		}()
	}

	for range 2 {
		body := <-bodies

		if !strings.Contains(body, `billet_scrape_up{source="ledger"} 1`) || !strings.Contains(body, "billet_test_nodes 3") {
			t.Errorf("a concurrent scrape was not answered:\n%s", body)
		}
	}

	if n := reads.Load(); n < 1 || n > 2 {
		t.Errorf("%d reads for two scrapes", n)
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
