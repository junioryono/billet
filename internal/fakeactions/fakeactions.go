// Package fakeactions is a stand-in for GitHub's Actions service.
//
// It exists because "it needs a GitHub organization" was wrong, and believing it
// left the code that talks to GitHub the least tested thing in the repository.
// The vendored client tests itself exactly this way, so the approach is the
// upstream one rather than an invention.
//
// What it buys is the class of bug that unit tests over billet's own types
// cannot reach. The worst defect in this project so far was calling AcquireJobs
// with ids taken from JobAssigned instead of JobAvailable: billet's types were
// self-consistent, every test passed, and the mistake existed only in the
// relationship between billet and the wire. A fake that speaks the wire catches
// it.
//
// It is a real package rather than a _test.go file because two different test
// suites need it — the scale-set client's own, and the end-to-end one that runs
// the control plane, the node and a real container together. It has no
// production callers and must not acquire any.
//
// IT IS DELIBERATELY NOT A SIMULATOR OF GITHUB'S BEHAVIOUR. It answers the
// handshake and serves whatever a test tells it to. Nothing here should be read
// as evidence of what the real service does; where billet depends on real
// behaviour, that behaviour was measured against the real API and written down
// where it is relied upon.
package fakeactions

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// Server is a fake Actions service, good enough to drive the REAL vendored
// client through a real HTTP round trip.
type Server struct {
	*httptest.Server

	// key signs the admin token the client parses for expiry. Generated per
	// server so no key material is checked in, even a throwaway one.
	key *rsa.PrivateKey

	mu       sync.Mutex
	requests []Request
}

// Request is one call the client made, so a test can assert on what billet
// actually put on the wire rather than on what it meant to.
type Request struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// New starts a fake service. The handler serves the scale-set API; the
// authentication handshake is answered here, because every test needs it and
// none of them are about it.
func New(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	f := &Server{Server: httptest.NewUnstartedServer(nil), key: key}

	f.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(t, r)

		switch {
		// The App exchanges its JWT for an installation token. The signature is
		// not checked: this fake proves billet's wiring, not GitHub's auth.
		//
		// 201, not 200. Measured against the real API — the client treats
		// anything else as a failure, and a fake answering 200 fails in a way
		// that looks like billet's bug rather than the fake's.
		case strings.Contains(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			WriteJSON(t, w, map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})

		// 201 here too. Both token endpoints check for StatusCreated exactly, and
		// a fake answering 200 produces an error that reads like billet's bug.
		case strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token"):
			w.WriteHeader(http.StatusCreated)
			WriteJSON(t, w, map[string]any{
				"token":      "registration-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})

		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			WriteJSON(t, w, map[string]any{
				"url":   f.URL + "/tenant/123/",
				"token": f.AdminToken(t),
			})

		default:
			handler(w, r)
		}
	})

	f.Start()
	t.Cleanup(f.Close)

	return f
}

// AdminToken mints the RS256 token the client reads an expiry out of.
func (f *Server) AdminToken(t *testing.T) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, &jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		Issuer:    "123",
	})

	signed, err := token.SignedString(f.key)
	if err != nil {
		t.Fatalf("sign admin token: %v", err)
	}

	return signed
}

// PrivateKeyPEM is the throwaway App key this server signs with, so a caller can
// build a client config that authenticates against it.
func (f *Server) PrivateKeyPEM() string {
	der := x509.MarshalPKCS1PrivateKey(f.key)

	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func (f *Server) record(t *testing.T, r *http.Request) {
	t.Helper()

	// Read to completion and PUT IT BACK. Recording is a side channel; a handler
	// downstream must still see the body it would have seen.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, Request{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Body:   string(body),
	})
}

// Calls returns every request whose path contains the fragment, so an assertion
// can name the endpoint it cares about rather than an index.
func (f *Server) Calls(fragment string) []Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []Request

	for _, req := range f.requests {
		if strings.Contains(req.Path, fragment) {
			out = append(out, req)
		}
	}

	return out
}

// WriteJSON encodes a response body.
func WriteJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// ScaleSetJSON is the shape the service returns for a scale set.
func ScaleSetJSON(id int, name, group string, labels ...string) map[string]any {
	typed := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		typed = append(typed, map[string]any{"name": l, "type": "System"})
	}

	return map[string]any{
		"id":                 id,
		"name":               name,
		"runnerGroupId":      1,
		"runnerGroupName":    group,
		"labels":             typed,
		"RunnerSetting":      map[string]any{},
		"createdOn":          time.Now().Format(time.RFC3339),
		"runnerJitConfigUrl": "",
	}
}

// ListJSON wraps values the way the service returns collections.
func ListJSON(values ...map[string]any) map[string]any {
	if values == nil {
		values = []map[string]any{}
	}

	return map[string]any{"count": len(values), "value": values}
}

// SessionJSON is what the service returns when a listener opens a session.
//
// The message queue URL points back at this same server, because a test that
// stood a second listener up somewhere else would be testing its own plumbing.
func SessionJSON(sessionID, owner string, set map[string]any, queueURL, queueToken string) map[string]any {
	return map[string]any{
		"sessionId":               sessionID,
		"ownerName":               owner,
		"runnerScaleSet":          set,
		"messageQueueUrl":         queueURL,
		"messageQueueAccessToken": queueToken,
		"statistics":              StatisticsJSON(0, 0),
	}
}

// StatisticsJSON is the capacity signal the service attaches to sessions and
// messages. Only the two fields billet reads are parameterised.
func StatisticsJSON(available, assigned int) map[string]any {
	return map[string]any{
		"totalAvailableJobs":     available,
		"totalAcquiredJobs":      0,
		"totalAssignedJobs":      assigned,
		"totalRunningJobs":       0,
		"totalRegisteredRunners": 0,
		"totalBusyRunners":       0,
		"totalIdleRunners":       0,
	}
}

// MessageJSON is the envelope the long poll returns.
//
// THE BODY IS A JSON STRING, not a nested object. That is the real wire format
// — the client json.Unmarshals `body` a second time — and a fake that nests the
// array instead produces a parse error that reads like billet's bug.
func MessageJSON(t *testing.T, id int, stats map[string]any, jobs ...map[string]any) map[string]any {
	t.Helper()

	if jobs == nil {
		jobs = []map[string]any{}
	}

	body, err := json.Marshal(jobs)
	if err != nil {
		t.Fatalf("encode batched messages: %v", err)
	}

	return map[string]any{
		"messageId":   id,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(body),
		"statistics":  stats,
	}
}

// JobJSON is one message inside a batch.
//
// messageType is one of JobAvailable, JobAssigned, JobStarted or JobCompleted.
// The distinction that matters most is the first two: an AVAILABLE job is one
// billet may bid for, and an ASSIGNED job is one it has been given. Acquiring by
// the wrong one is the defect this whole fake was built to catch.
func JobJSON(messageType string, requestID int64, event string, labels ...string) map[string]any {
	if labels == nil {
		labels = []string{}
	}

	return map[string]any{
		"messageType":     messageType,
		"runnerRequestId": requestID,
		"repositoryName":  "billet-test",
		"ownerName":       "acme",
		"jobId":           fmt.Sprintf("job-%d", requestID),
		"jobWorkflowRef":  "acme/billet-test/.github/workflows/ci.yml@refs/heads/main",
		"jobDisplayName":  fmt.Sprintf("test job %d", requestID),
		"workflowRunId":   requestID,
		"eventName":       event,
		"requestLabels":   labels,
		"queueTime":       time.Now().Format(time.RFC3339),
	}
}

// JitConfigJSON is what generatejitconfig returns.
//
// The encoded config is opaque to billet — it is handed to the runner verbatim —
// so a placeholder is honest here in a way that a hand-built runner config would
// not be.
func JitConfigJSON(runnerID int, name, encoded string) map[string]any {
	return map[string]any{
		"runner": map[string]any{
			"id":               runnerID,
			"name":             name,
			"runnerScaleSetId": 1,
		},
		"encodedJITConfig": encoded,
	}
}

// JobFields are the facts a job message may carry beyond what JobJSON fixes.
//
// JobJSON fixes one repository, one owner, one workflow and a run id equal to
// the request id, which is what every scenario about the lifecycle wants. A
// replay of a real workload wants each job to carry its own, and a started or
// completed message has to name the RUNNER GitHub bound the job to, which no
// other message does. A zero field leaves JobJSON's value in place.
type JobFields struct {
	Owner, Repository, WorkflowRef, JobID string
	RunID                                 int64
	// RunnerID and RunnerName identify the pool member, and Result is GitHub's
	// conclusion. All three are read only from started and completed messages.
	RunnerID   int64
	RunnerName string
	Result     string
}

// Apply sets the fields on a job message JobJSON built, and returns it.
func (f JobFields) Apply(job map[string]any) map[string]any {
	if f.Owner != "" {
		job["ownerName"] = f.Owner
	}

	if f.Repository != "" {
		job["repositoryName"] = f.Repository
	}

	if f.WorkflowRef != "" {
		job["jobWorkflowRef"] = f.WorkflowRef
	}

	if f.JobID != "" {
		job["jobId"] = f.JobID
	}

	if f.RunID != 0 {
		job["workflowRunId"] = f.RunID
	}

	if f.RunnerID != 0 {
		job["runnerId"] = f.RunnerID
	}

	if f.RunnerName != "" {
		job["runnerName"] = f.RunnerName
	}

	if f.Result != "" {
		job["result"] = f.Result
	}

	return job
}
