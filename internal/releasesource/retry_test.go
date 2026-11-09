package releasesource

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer answers 504 Gateway Timeout for its first failures requests, as
// GitHub's release downloads did on 2026-11-08 and 2026-11-09, and then status.
func flakyServer(t *testing.T, failures int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) <= failures {
			w.WriteHeader(http.StatusGatewayTimeout)

			return
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, "the release"); err != nil {
			t.Errorf("write the release: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return server, &served
}

// A FETCH THAT GITHUB ANSWERS 504 IS TRIED AGAIN, and one it then answers is
// what the caller gets.
func TestAFetchAnswered504IsTriedAgain(t *testing.T) {
	t.Parallel()

	server, served := flakyServer(t, fetchAttempts-1, http.StatusOK)
	client := &Client{HTTP: server.Client(), backoff: time.Millisecond}

	body, err := client.get(t.Context(), server.URL+"/manifest", 1<<10)
	if err != nil {
		t.Fatalf("get after %d 504s: %v", fetchAttempts-1, err)
	}
	if string(body) != "the release" || served.Load() != fetchAttempts {
		t.Fatalf("got %q after %d request(s), want the release after %d", body, served.Load(), fetchAttempts)
	}
}

// AND NOT FOREVER: the last attempt's failure is the answer.
func TestAFetchGivesUpAfterItsAttempts(t *testing.T) {
	t.Parallel()

	server, served := flakyServer(t, fetchAttempts, http.StatusOK)
	client := &Client{HTTP: server.Client(), backoff: time.Millisecond}

	if _, err := client.get(t.Context(), server.URL+"/manifest", 1<<10); err == nil {
		t.Fatal("a fetch answered 504 every time succeeded")
	}
	if served.Load() != fetchAttempts {
		t.Fatalf("tried %d time(s), want %d", served.Load(), fetchAttempts)
	}
}

// A 404 IS AN ANSWER, NOT AN OUTAGE: it is not tried again, and it stays
// ErrNotFound.
func TestANotFoundIsNotTriedAgain(t *testing.T) {
	t.Parallel()

	server, served := flakyServer(t, 0, http.StatusNotFound)
	client := &Client{HTTP: server.Client(), backoff: time.Millisecond}

	if _, err := client.get(t.Context(), server.URL+"/manifest", 1<<10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a 404 = %v, want ErrNotFound", err)
	}
	if served.Load() != 1 {
		t.Fatalf("a 404 was tried %d times, want once", served.Load())
	}
}

// NOR IS A REFUSAL: a 403 says the same thing the second time.
func TestAForbiddenFetchIsNotTriedAgain(t *testing.T) {
	t.Parallel()

	server, served := flakyServer(t, 0, http.StatusForbidden)
	client := &Client{HTTP: server.Client(), backoff: time.Millisecond}

	if _, err := client.get(t.Context(), server.URL+"/manifest", 1<<10); err == nil {
		t.Fatal("a 403 succeeded")
	}
	if served.Load() != 1 {
		t.Fatalf("a 403 was tried %d times, want once", served.Load())
	}
}

// A CALLER THAT GIVES UP ENDS THE WAIT BETWEEN ATTEMPTS.
func TestACancelledFetchStopsTrying(t *testing.T) {
	t.Parallel()

	server, served := flakyServer(t, fetchAttempts, http.StatusOK)
	client := &Client{HTTP: server.Client(), backoff: time.Hour}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := client.get(ctx, server.URL+"/manifest", 1<<10); err == nil {
		t.Fatal("a cancelled fetch succeeded")
	}
	if waited := time.Since(started); waited > 30*time.Second || served.Load() != 1 {
		t.Fatalf("a cancelled fetch waited %s over %d request(s), want it to stop at the cancellation", waited,
			served.Load())
	}
}
