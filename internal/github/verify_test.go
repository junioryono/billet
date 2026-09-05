package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// testAppKey mints one RSA key for the whole file — key generation is the slow
// part of these tests and the key's identity is irrelevant to every case.
var testAppKey = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
})

// verifyFake serves one canned installation response and records the bearer the
// request carried, so redaction tests can prove the credential never reaches an
// error message.
func verifyFake(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()

	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write fake response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &bearer
}

func TestVerifyAppAcceptsAnExactInstallation(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{"id": 42, "permissions": {
		"metadata": "read", "organization_self_hosted_runners": "write"}}`)

	inst, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if err != nil {
		t.Fatalf("an exact installation was refused: %v", err)
	}
	if inst.ID != 42 {
		t.Errorf("installation id = %d, want 42", inst.ID)
	}
}

func TestVerifyAppRefusesAMismatchedInstallationID(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{"id": 42, "permissions": {
		"metadata": "read", "organization_self_hosted_runners": "write"}}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 99)
	if err == nil {
		t.Fatal("a mismatched installation id was accepted")
	}
	for _, must := range []string{"42", "99"} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("the mismatch does not name %s: %v", must, err)
		}
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Error("a definite mismatch was classified as unverifiable")
	}
}

// EVERY PERMISSION MISMATCH IS FATAL, IN BOTH DIRECTIONS — matching
// PermissionMismatches' own contract. A missing one breaks registration later
// with an error that never mentions permissions; an EXTRA one falsifies the
// credential-isolation claim while everything appears to work.
func TestVerifyAppRefusesPermissionDrift(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"missing": `{"id": 42, "permissions": {"metadata": "read"}}`,
		"downgraded": `{"id": 42, "permissions": {
			"metadata": "read", "organization_self_hosted_runners": "read"}}`,
		"extra": `{"id": 42, "permissions": {
			"metadata": "read", "organization_self_hosted_runners": "write", "contents": "read"}}`,
		"widened read": `{"id": 42, "permissions": {
			"metadata": "write", "organization_self_hosted_runners": "write"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := verifyFake(t, http.StatusOK, body)

			_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
			if err == nil {
				t.Fatal("a permission mismatch was accepted")
			}
			if errors.Is(err, ErrAppUnverifiable) {
				t.Error("a definite permission verdict was classified as unverifiable")
			}
		})
	}
}

func TestVerifyAppRefusesAnUninstalledApp(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusNotFound, `{"message": "Not Found"}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("an uninstalled App did not get the install remedy: %v", err)
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Error("not-installed is a verdict, not an unverifiable state")
	}
}

// A 401 IS A VERDICT: GitHub understood the request and refused the credential.
// And the error must carry neither the JWT the request bore nor the key it was
// signed with — a check failure gets pasted into issues and chat.
func TestVerifyAppRefusesABadCredentialWithoutLeakingIt(t *testing.T) {
	t.Parallel()

	srv, bearer := verifyFake(t, http.StatusUnauthorized, `{"message": "Bad credentials"}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if err == nil {
		t.Fatal("a refused credential was accepted")
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Error("a credential refusal was classified as unverifiable")
	}

	if *bearer == "" {
		t.Fatal("the fake captured no bearer; the redaction assertion would be vacuous")
	}
	if strings.Contains(err.Error(), *bearer) {
		t.Errorf("the error leaks the JWT: %v", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("the error leaks key material: %v", err)
	}
}

// A 5xx OR A DEAD NETWORK SAYS NOTHING ABOUT THE APP. Both classify as
// unverifiable — the advisory band `billet check` reports without failing, and
// the one `local up` must never treat as proven.
func TestVerifyAppClassifiesGitHubDownAsUnverifiable(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusBadGateway, `{"message": "upstream error"}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if !errors.Is(err, ErrAppUnverifiable) {
		t.Fatalf("a 502 was not classified as unverifiable: %v", err)
	}
}

func TestVerifyAppClassifiesTransportFailureAsUnverifiable(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{}`)
	base := srv.URL
	srv.Close() // the address now refuses connections

	_, err := verifyAppAt(t.Context(), http.DefaultClient, base, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if !errors.Is(err, ErrAppUnverifiable) {
		t.Fatalf("a connection refusal was not classified as unverifiable: %v", err)
	}
}

// A KEY THAT CANNOT SIGN IS A VERDICT ABOUT THE CONFIGURATION, reached before
// any request — classifying it as "could not reach GitHub" would send the
// operator to their network instead of their key file.
func TestVerifyAppTreatsABrokenKeyAsFatal(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7,
		[]byte("not a key"), OrganizationTarget("acme"), 42)
	if err == nil {
		t.Fatal("a broken key was accepted")
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Errorf("a local key failure was classified as unverifiable: %v", err)
	}
}

// A THROTTLE SAYS NOTHING ABOUT THE APP. GitHub's primary rate limit answers
// 403 and its secondary limit 429; classifying either as a credential refusal
// sends the operator to their key file over a wait — the same lesson
// conversionError already records for the manifest exchange.
func TestVerifyAppClassifiesRateLimitsAsUnverifiable(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"secondary 429": {http.StatusTooManyRequests, `{"message": "You have exceeded a secondary rate limit"}`},
		"primary 403":   {http.StatusForbidden, `{"message": "API rate limit exceeded for installation ID 42."}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := verifyFake(t, tc.status, tc.body)

			_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
			if !errors.Is(err, ErrAppUnverifiable) {
				t.Fatalf("a throttle was classified as a verdict: %v", err)
			}
		})
	}
}

// AND A PLAIN 403 STAYS A VERDICT — the throttle carve-out must not swallow a
// genuine refusal.
func TestVerifyAppKeepsAPlainForbiddenFatal(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusForbidden, `{"message": "This installation has been suspended"}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if err == nil || errors.Is(err, ErrAppUnverifiable) {
		t.Fatalf("a non-throttle 403 was not a fatal verdict: %v", err)
	}
}

// A SUSPENDED INSTALLATION ANSWERS 200 WITH MATCHING EVERYTHING — GitHub's way
// of disabling an App without uninstalling it — while every token request
// fails. "Verified" here would be the check lying.
func TestVerifyAppRefusesASuspendedInstallation(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{"id": 42, "suspended_at": "2026-08-01T00:00:00Z",
		"permissions": {"metadata": "read", "organization_self_hosted_runners": "write"}}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if err == nil || !strings.Contains(err.Error(), "SUSPENDED") {
		t.Fatalf("a suspended installation was not refused by name: %v", err)
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Error("suspension is a verdict, not an unverifiable state")
	}
}

// A 200 THAT IS NOT A GITHUB ANSWER — a captive portal, a transparent proxy —
// says nothing about the App and must not blame it.
func TestVerifyAppClassifiesANonGitHubAnswerAsUnverifiable(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"html":  `<html><body>hotel wifi login</body></html>`,
		"no id": `{"ok": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := verifyFake(t, http.StatusOK, body)

			_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
			if !errors.Is(err, ErrAppUnverifiable) {
				t.Fatalf("a non-GitHub 200 was classified as a verdict: %v", err)
			}
		})
	}
}

// A RESPONSE THAT DIES MID-READ is the network failing, not the App — and the
// classification is by a typed sentinel, so rewording a message cannot
// silently move this band.
func TestVerifyAppClassifiesAMidReadFailureAsUnverifiable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		if _, err := w.Write([]byte(`{"id": 42`)); err != nil { // then the connection dies short
			t.Errorf("write the truncated body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if !errors.Is(err, ErrAppUnverifiable) {
		t.Fatalf("a mid-read failure was classified as a verdict: %v", err)
	}
}

// THE CALLER'S CANCELLATION IS NEITHER BAND: an interrupted check surfaces the
// interruption, or a SIGINT reads as an advisory line and the command sails on.
func TestVerifyAppPropagatesCallerCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()

	_, err := verifyAppAt(ctx, srv.Client(), srv.URL, 7, testAppKey(), OrganizationTarget("acme"), 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation surfaced as %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrAppUnverifiable) {
		t.Error("cancellation was classified as unverifiable")
	}
}
