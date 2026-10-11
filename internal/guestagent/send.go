package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// The guest-facing routes of the node's cache service, and what a batch is sent as.
const (
	limitsPath       = "/v1/guest/limits"
	reportsPath      = "/v1/guest/reports"
	batchContentType = "application/vnd.billet.guest-batch"
)

// The bounds of talking to the node: the bytes of an answer read, the longest wait
// a 429 is honoured for, and how long one request may take.
const (
	answerLimit     = 4 << 10
	maxRetryAfter   = 60 * time.Second
	requestTimeout  = 10 * time.Second
	maxIntervalMS   = int64(time.Hour / time.Millisecond)
	schemaSpoken    = 1
	firstBackoff    = time.Second
	maxLimitBackoff = time.Minute
)

// verdict is what the agent does after an answer from the node.
type verdict int

const (
	// unknown is no answer, or one the contract does not name: could not tell.
	// It is the zero value, so an answer nobody classified is never taken as
	// permission to stop or to drop.
	unknown verdict = iota
	// accepted is a 204 to a batch, or a hello the agent can send under.
	accepted
	// rateLimited is a 429: wait and resend the same batch.
	rateLimited
	// refused is a 400: the node counted this batch and refused it; drop it.
	refused
	// stop is a 401, a 404 or a 413, or limits the agent cannot speak: send
	// nothing more, ever.
	stop
)

// limits is the node's answer to a hello.
type limits struct {
	interval time.Duration
	maxBody  int
}

// limitsAnswer is GET /v1/guest/limits's body, read leniently: a field the agent
// does not know is ignored.
type limitsAnswer struct {
	Schemas    []int  `json:"schemas"`
	IntervalMS *int64 `json:"interval_ms"`
	MaxBody    *int64 `json:"max_body"`
}

// node is the guest-facing service the agent reports to.
type node struct {
	base   string
	token  string
	client *http.Client
}

// newNode is the node at endpoint, refused unless it is an http or https URL
// naming a host and nothing else a request could be steered by.
func newNode(endpoint, token string, transport http.RoundTripper) (*node, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "" {
		return nil, errors.New("BILLET_CACHE_ENDPOINT is not an http or https URL naming only a host")
	}

	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return nil, errors.New("BILLET_CACHE_TOKEN is not a bearer token")
	}

	if transport == nil {
		transport = &http.Transport{
			// The bearer goes to the node and nowhere else: no proxy the
			// environment names carries it.
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:          1,
			IdleConnTimeout:       time.Minute,
			ResponseHeaderTimeout: requestTimeout,
		}
	}

	return &node{
		base:  u.Scheme + "://" + u.Host,
		token: token,
		client: &http.Client{
			Transport: transport,
			// A redirect is an answer the contract does not name, and following
			// one would carry the request elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// hello asks the node for its limits. It answers accepted with the limits, stop,
// or unknown.
func (n *node) hello(ctx context.Context) (limits, verdict) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+limitsPath, http.NoBody)
	if err != nil {
		return limits{}, unknown
	}

	status, body, _, err := n.do(req)
	if err != nil {
		return limits{}, unknown
	}

	switch status {
	case http.StatusOK:
		// The limits are the body, so a body not read to its end is
		// could-not-tell; a stop is its status alone.
		if body.err != nil {
			return limits{}, unknown
		}

		return readLimits(body.data)
	case http.StatusNotFound, http.StatusUnauthorized:
		return limits{}, stop
	default:
		return limits{}, unknown
	}
}

// readLimits is a 200's body. Limits the agent cannot use are could-not-tell,
// except a list of schemas without the one it speaks, which is the node saying no.
func readLimits(body []byte) (limits, verdict) {
	var a limitsAnswer
	if err := json.Unmarshal(body, &a); err != nil || a.Schemas == nil || a.IntervalMS == nil || a.MaxBody == nil {
		return limits{}, unknown
	}

	if !slices.Contains(a.Schemas, schemaSpoken) {
		return limits{}, stop
	}

	if *a.IntervalMS < 1 || *a.IntervalMS > maxIntervalMS || *a.MaxBody < 1 {
		return limits{}, unknown
	}

	return limits{
		interval: time.Duration(*a.IntervalMS) * time.Millisecond,
		maxBody:  int(min(*a.MaxBody, guestreport.MaxBatchBytes)),
	}, accepted
}

// report sends one encoded batch, marked final when it is the agent's last. It
// answers what to do, and for a 429 how long to wait.
func (n *node) report(ctx context.Context, body []byte, final bool) (verdict, time.Duration) {
	target := n.base + reportsPath
	if final {
		target += "?final=1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return unknown, 0
	}

	req.Header.Set("Content-Type", batchContentType)

	// A report's answer is its status and headers: a body cut short does not
	// turn a 413 or a 429 into an answer nobody can tell.
	status, _, header, err := n.do(req)
	if err != nil {
		return unknown, 0
	}

	switch status {
	case http.StatusNoContent:
		return accepted, 0
	case http.StatusTooManyRequests:
		return rateLimited, retryAfter(header.Get("Retry-After"))
	case http.StatusBadRequest:
		return refused, 0
	case http.StatusRequestEntityTooLarge, http.StatusUnauthorized, http.StatusNotFound:
		return stop, 0
	default:
		return unknown, 0
	}
}

// retryAfter is a 429's wait: whole seconds, at most maxRetryAfter. A value that
// is not a number of seconds is no wait beyond the interval.
func retryAfter(v string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || seconds < 0 {
		return 0
	}

	return min(time.Duration(min(seconds, int64(maxRetryAfter/time.Second)))*time.Second, maxRetryAfter)
}

// answerBody is at most answerLimit bytes of an answer's body, and why the rest
// was not read.
type answerBody struct {
	data []byte
	err  error
}

// do sends req with the bearer and answers the node's status and headers, and its
// body read as far as answerLimit; an error is no answer at all.
func (n *node) do(req *http.Request) (int, answerBody, http.Header, error) {
	req.Header.Set("Authorization", "Bearer "+n.token)

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, answerBody{}, nil, fmt.Errorf("the node did not answer: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit))

	return resp.StatusCode, answerBody{data: data, err: err}, resp.Header, nil
}
