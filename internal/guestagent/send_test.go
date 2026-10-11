package guestagent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

func TestTheEndpointAndTheTokenAreRefusedUnlessTheyAreWhatTheNodeGives(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		endpoint, token string
		ok              bool
	}{
		{"http://172.31.0.1:7718", testToken, true},
		{"http://172.31.0.1:7718/", testToken, true},
		{"https://node.internal", testToken, true},
		{"", testToken, false},
		{"172.31.0.1:7718", testToken, false},
		{"ftp://172.31.0.1", testToken, false},
		{"http://", testToken, false},
		{"http://user:pass@172.31.0.1:7718", testToken, false},
		{"http://172.31.0.1:7718/elsewhere", testToken, false},
		{"http://172.31.0.1:7718?x=1", testToken, false},
		{"http://172.31.0.1:7718#x", testToken, false},
		{"http://172.31.0.1:7718", "", false},
		{"http://172.31.0.1:7718", "two words", false},
		{"http://172.31.0.1:7718", "line\nbreak", false},
		{"http://172.31.0.1:7718", "café", false},
	} {
		n, err := newNode(tc.endpoint, tc.token, nil)
		if (err == nil) != tc.ok {
			t.Errorf("%q %q: %v, want ok %v", tc.endpoint, tc.token, err, tc.ok)
		}

		if err != nil && tc.token != "" && strings.Contains(err.Error(), tc.token) {
			t.Errorf("the refusal quotes the token: %v", err)
		}

		if n != nil && n.base != "http://172.31.0.1:7718" && n.base != "https://node.internal" {
			t.Errorf("%q became the base %q", tc.endpoint, n.base)
		}
	}
}

// The bearer goes to the node and nowhere else: the transport the agent builds
// names no proxy, whatever the environment says, and a redirect is an answer
// nobody can tell, never followed.
func TestTheBearerGoesToTheNodeAlone(t *testing.T) {
	t.Parallel()

	n, err := newNode("http://172.31.0.1:7718", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}

	transport, ok := n.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("the agent's transport %T names a proxy", n.client.Transport)
	}

	var elsewhere atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(target.Close)

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)

	n, err = newNode(redirecting.URL, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, v := n.hello(t.Context()); v != unknown {
		t.Errorf("a redirected hello is %v, want could-not-tell", v)
	}

	if v, _ := n.report(t.Context(), []byte("x"), false); v != unknown {
		t.Errorf("a redirected batch is %v, want could-not-tell", v)
	}

	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect was followed %d times", elsewhere.Load())
	}
}

func TestTheNodesLimitsAreReadLeniently(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		body string
		want verdict
		lim  limits
	}{
		{`{"schemas":[1],"interval_ms":10000,"max_body":65536}`, accepted, limits{10 * time.Second, 65536}},
		{`{"schemas":[2,1],"interval_ms":10000,"max_body":65536,"later":{"x":1}}`, accepted, limits{10 * time.Second, 65536}},
		{`{"schemas":[1],"interval_ms":5000,"max_body":1048576}`, accepted, limits{5 * time.Second, guestreport.MaxBatchBytes}},
		{`{"schemas":[1],"interval_ms":10000,"max_body":4096}`, accepted, limits{10 * time.Second, 4096}},
		{`{"schemas":[2],"interval_ms":10000,"max_body":65536}`, stop, limits{}},
		{`{"schemas":[],"interval_ms":10000,"max_body":65536}`, stop, limits{}},
		{`{"interval_ms":10000,"max_body":65536}`, unknown, limits{}},
		{`{"schemas":[1],"max_body":65536}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":10000}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":0,"max_body":65536}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":-1,"max_body":65536}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":3600001,"max_body":65536}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":10000,"max_body":0}`, unknown, limits{}},
		{`{"schemas":[1],"interval_ms":1e400,"max_body":65536}`, unknown, limits{}},
		{`not json`, unknown, limits{}},
		{``, unknown, limits{}},
	} {
		lim, v := readLimits([]byte(tc.body))
		if v != tc.want || lim != tc.lim {
			t.Errorf("%s: %+v %v, want %+v %v", tc.body, lim, v, tc.lim, tc.want)
		}
	}
}

func TestARetryAfterIsWholeSecondsAtMostAMinute(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"5", 5 * time.Second},
		{" 30 ", 30 * time.Second},
		{"60", time.Minute},
		{"61", time.Minute},
		{"9223372036854775807", time.Minute},
		{"0", 0},
		{"-3", 0},
		{"", 0},
		{"1.5", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
	} {
		if got := retryAfter(tc.header); got != tc.want {
			t.Errorf("Retry-After %q is %v, want %v", tc.header, got, tc.want)
		}
	}
}
