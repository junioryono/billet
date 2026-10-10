package guestagent

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

const testToken = "guest-bearer-0123456789abcdef"

// reply is one scripted answer of the fake node. drop answers nothing at all, and
// hold answers nothing until the request's context ends, never serving it.
type reply struct {
	status     int
	retryAfter string
	location   string
	body       string
	drop       bool
	hold       bool
}

// post is a batch the fake node received.
type post struct {
	at     time.Time
	final  bool
	body   []byte
	batch  guestreport.Batch
	status int
}

// fakeNode is the node's guest-facing service as the contract fixes it: it
// enforces every limit the real one does, answers what a test scripts, and records
// what the agent sent. Whatever the agent does wrong that the contract forbids is
// a test failure here, not only an answer.
type fakeNode struct {
	t *testing.T

	mu sync.Mutex
	// helloScript and postScript answer the hellos and the posts in turn; past
	// their end, a hello is answered with limits and a post with 204.
	helloScript []reply
	postScript  []reply
	intervalMS  int64
	maxBody     int64

	hellos []time.Time
	posts  []post
	// held counts the posts held and never served, and heldEnded is when each
	// one's context ended.
	held      int
	heldEnded []time.Time
	tokens    float64
	refilled  time.Time
	spent     int
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()

	return &fakeNode{t: t, intervalMS: 10_000, maxBody: guestreport.MaxBatchBytes, tokens: 4}
}

// RoundTrip serves a request in the caller's goroutine, so a synctest bubble holds
// the whole exchange; a dropped one is a request nobody answered.
func (f *fakeNode) RoundTrip(req *http.Request) (*http.Response, error) {
	// A real transport sends nothing on a context already done.
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	drop, hold := false, false

	switch req.URL.Path {
	case limitsPath:
		if n := len(f.hellos); n < len(f.helloScript) {
			drop = f.helloScript[n].drop
		}
	case reportsPath:
		if n := len(f.posts) + f.held; n < len(f.postScript) {
			drop, hold = f.postScript[n].drop, f.postScript[n].hold
		}
	}

	if hold {
		f.held++
	}

	f.mu.Unlock()

	if hold {
		<-req.Context().Done()

		f.mu.Lock()
		f.heldEnded = append(f.heldEnded, time.Now())
		f.mu.Unlock()

		return nil, req.Context().Err()
	}

	if drop {
		f.record(req)

		return nil, errors.New("the node did not answer")
	}

	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)

	return rec.Result(), nil
}

// record notes a request that was dropped before it was served.
func (f *fakeNode) record(req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.URL.Path == limitsPath {
		f.hellos = append(f.hellos, time.Now())

		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Errorf("read a dropped batch: %v", err)
	}

	batch, err := guestreport.DecodeBatch(body)
	if err != nil {
		f.t.Errorf("a dropped batch does not decode: %v", err)
	}

	f.posts = append(f.posts, post{at: time.Now(), final: req.URL.RawQuery == "final=1", body: body, batch: batch})
}

func (f *fakeNode) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("a request carried Authorization %q", got)
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	switch {
	case req.Method == http.MethodGet && req.URL.Path == limitsPath && req.URL.RawQuery == "":
		f.hello(w)
	case req.Method == http.MethodPost && req.URL.Path == reportsPath:
		f.report(w, req)
	default:
		f.t.Errorf("a request the contract does not have: %s %s", req.Method, req.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeNode) hello(w http.ResponseWriter) {
	n := len(f.hellos)
	f.hellos = append(f.hellos, time.Now())

	if n < len(f.helloScript) {
		r := f.helloScript[n]
		w.WriteHeader(r.status)

		if _, err := io.WriteString(w, r.body); err != nil {
			f.t.Errorf("answer a hello: %v", err)
		}

		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, `{"schemas":[1],"interval_ms":`+strconv.FormatInt(f.intervalMS, 10)+
		`,"max_body":`+strconv.FormatInt(f.maxBody, 10)+`,"a_field_from_later":true}`); err != nil {
		f.t.Errorf("answer a hello: %v", err)
	}
}

func (f *fakeNode) report(w http.ResponseWriter, req *http.Request) {
	final := false

	switch req.URL.RawQuery {
	case "":
	case "final=1":
		final = true
	default:
		f.t.Errorf("a batch posted with the query %q", req.URL.RawQuery)
	}

	if got := req.Header.Get("Content-Type"); got != batchContentType {
		f.t.Errorf("a batch posted as %q", got)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, guestreport.MaxBatchBytes))
	if err != nil {
		f.t.Errorf("a batch past MaxBatchBytes: %v", err)
		w.WriteHeader(http.StatusRequestEntityTooLarge)

		return
	}

	if int64(len(body)) > f.maxBody {
		f.t.Errorf("a batch of %d bytes, past max_body %d", len(body), f.maxBody)
	}

	batch, err := guestreport.DecodeBatch(body)
	if err != nil {
		f.t.Errorf("a batch that does not decode: %v", err)
	}

	p := post{at: time.Now(), final: final, body: body, batch: batch}
	n := len(f.posts) + f.held

	switch {
	case n < len(f.postScript):
		r := f.postScript[n]
		p.status = r.status

		if r.retryAfter != "" {
			w.Header().Set("Retry-After", r.retryAfter)
		}

		if r.location != "" {
			w.Header().Set("Location", r.location)
		}
	case !f.take():
		f.t.Errorf("batch %d was sent faster than the node's bucket refills", batch.Seq)

		p.status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", "5")
	case f.spent+len(body) > 2<<20:
		p.status = http.StatusRequestEntityTooLarge
	default:
		p.status = http.StatusNoContent
		f.spent += len(body)
	}

	f.posts = append(f.posts, p)
	w.WriteHeader(p.status)
}

// take spends one of the node's tokens: one every five seconds, four at most.
func (f *fakeNode) take() bool {
	now := time.Now()
	if !f.refilled.IsZero() {
		f.tokens = min(4, f.tokens+now.Sub(f.refilled).Seconds()/5)
	}

	f.refilled = now

	if f.tokens < 1 {
		return false
	}

	f.tokens--

	return true
}

func (f *fakeNode) snapshot() ([]time.Time, []post) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]time.Time(nil), f.hellos...), append([]post(nil), f.posts...)
}

// finals counts the final batches received.
func finals(posts []post) int {
	n := 0

	for i := range posts {
		if posts[i].final {
			n++
		}
	}

	return n
}
