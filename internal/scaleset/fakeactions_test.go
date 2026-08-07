package scaleset

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// fakeActions is a stand-in for GitHub's Actions service, good enough to drive
// the REAL vendored client through a real HTTP round trip.
//
// This exists because "it needs a GitHub organization" was wrong, and believing
// it left internal/scaleset — the one package that talks to GitHub — the least
// tested thing in the repository. The vendored client tests itself exactly this
// way (client_test.go newActionsServer), so the approach is the upstream one
// rather than an invention.
//
// What it buys is the class of bug that unit tests over billet's own types
// cannot reach. The worst defect in this project so far was calling AcquireJobs
// with ids taken from JobAssigned instead of JobAvailable: billet's types were
// self-consistent, every test passed, and the mistake only existed in the
// relationship between billet and the wire. A fake that speaks the wire catches
// it — see TestAnAvailableJobIsAcquiredByItsOwnRequestID.
//
// It is deliberately NOT a simulator of GitHub's behaviour. It answers the
// handshake and serves whatever the test tells it to; nothing here should be
// read as evidence of what the real service does.
type fakeActions struct {
	*httptest.Server

	// key signs the admin token the client parses for expiry. Generated per
	// server so no key material is checked in, even a throwaway one.
	key *rsa.PrivateKey

	mu       sync.Mutex
	requests []recordedRequest
}

// recordedRequest is one call the client made, so a test can assert on what
// billet actually put on the wire rather than on what it meant to.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// newFakeActions starts a fake service. The handler serves the scale-set API;
// the authentication handshake is answered here, because every test needs it and
// none of them are about it.
func newFakeActions(t *testing.T, handler http.HandlerFunc) *fakeActions {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	f := &fakeActions{Server: httptest.NewUnstartedServer(nil), key: key}

	f.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(t, r)

		switch {
		// The App exchanges its JWT for an installation token. The signature is
		// not checked: this fake proves billet's wiring, not GitHub's auth.
		case strings.Contains(r.URL.Path, "/access_tokens"):
			// 201, not 200: fetchAccessToken checks for StatusCreated exactly.
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"token":      "installation-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})

		case strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token"):
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"token":      "registration-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})

		// The admin connection hands back the actions-service URL and a token the
		// client PARSES for its expiry, so it has to be a real signed JWT.
		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			writeJSON(t, w, map[string]any{
				"url":   f.URL + "/tenant/123/",
				"token": f.adminToken(t),
			})

		default:
			handler(w, r)
		}
	})

	f.Start()
	t.Cleanup(f.Close)

	return f
}

// adminToken mints the RS256 token the client reads an expiry out of.
func (f *fakeActions) adminToken(t *testing.T) string {
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

func (f *fakeActions) record(t *testing.T, r *http.Request) {
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

	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Body:   string(body),
	})
}

// calls returns every request whose path contains the fragment, so an assertion
// can name the endpoint it cares about rather than an index.
func (f *fakeActions) calls(fragment string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []recordedRequest

	for _, req := range f.requests {
		if strings.Contains(req.Path, fragment) {
			out = append(out, req)
		}
	}

	return out
}

// config points billet's client at the fake, with a throwaway App key.
func (f *fakeActions) config(t *testing.T) Config {
	t.Helper()

	der := x509.MarshalPKCS1PrivateKey(f.key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})

	return Config{
		ConfigURL:      f.URL + "/acme",
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     string(pemKey),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// testSetName and testGroup are the tier every test here provisions. Constants
// rather than parameters because varying them proved nothing and only invited a
// helper that could disagree with the tests using it.
const (
	testSetName = "billet-4vcpu"
	testGroup   = "billet"
)

// scaleSetJSON is the shape the service returns for a scale set.
func scaleSetJSON(id int, labels ...string) map[string]any {
	typed := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		typed = append(typed, map[string]any{"name": l, "type": "System"})
	}

	return map[string]any{
		"id":                 id,
		"name":               testSetName,
		"runnerGroupId":      1,
		"runnerGroupName":    testGroup,
		"labels":             typed,
		"RunnerSetting":      map[string]any{},
		"createdOn":          time.Now().Format(time.RFC3339),
		"runnerJitConfigUrl": "",
	}
}

// listJSON wraps values the way the service returns collections.
func listJSON(values ...map[string]any) map[string]any {
	if values == nil {
		values = []map[string]any{}
	}

	return map[string]any{"count": len(values), "value": values}
}
