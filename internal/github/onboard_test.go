package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub stands in for the API half of the flow. The browser half is driven
// by the OpenBrowser hook, which is what makes the whole handshake testable
// without a real organization.
type fakeGitHub struct {
	t *testing.T

	appID          int64
	installationID int64
	pem            string
	permissions    map[string]string

	// installed flips once the test "completes" the installation, so the poller
	// sees the same not-installed-then-installed transition an operator produces.
	installed atomic.Bool

	conversions atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return &fakeGitHub{
		t:              t,
		appID:          4242,
		installationID: 909090,
		pem:            string(encoded),
		permissions:    map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
	}
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		g.conversions.Add(1)

		if !strings.HasSuffix(r.URL.Path, "/conversions") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"slug":"billet-acme","name":"billet","html_url":"https://github.com/apps/billet-acme","pem":%q}`,
			g.appID, g.pem)
	})

	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, _ *http.Request) {
		if !g.installed.Load() {
			// The ordinary "created but not installed yet" state.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)

			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"id":%d,"account":{"login":"acme","type":"Organization"},"permissions":{`, g.installationID)

		first := true

		for k, v := range g.permissions {
			if !first {
				fmt.Fprint(w, ",")
			}

			first = false

			fmt.Fprintf(w, "%q:%q", k, v)
		}

		fmt.Fprint(w, "}}")
	})

	return mux
}

// browser plays the operator: it fetches the start page, extracts the manifest
// form's action, and drives the two callbacks the real GitHub would.
type browser struct {
	t      *testing.T
	fake   *fakeGitHub
	client *http.Client
	// visits records the URLs the flow asked the operator to open.
	visits []string
}

func (b *browser) open(ctx context.Context, target string) error {
	b.visits = append(b.visits, target)

	// The first URL is the loopback start page; the second is GitHub's install
	// page, which the test answers by "completing" the install.
	if strings.Contains(target, "/installations/new") {
		b.fake.installed.Store(true)

		installedURL := strings.Replace(b.visits[0], "/", "/installed", 1)
		installedURL = strings.TrimSuffix(installedURL, "/") +
			"?installation_id=" + fmt.Sprint(b.fake.installationID) + "&setup_action=install"

		go b.get(ctx, installedURL)

		return nil
	}

	go b.driveRegistration(ctx, target)

	return nil
}

// driveRegistration fetches the self-submitting form and then plays GitHub's
// redirect back to /callback with a code and the state it was given.
func (b *browser) driveRegistration(ctx context.Context, startURL string) {
	body := b.get(ctx, startURL)

	action := extractFormAction(b.t, body)
	if action == "" {
		b.t.Errorf("start page carried no form action:\n%s", body)
		return
	}

	parsed, err := url.Parse(action)
	if err != nil {
		b.t.Errorf("parse form action: %v", err)
		return
	}

	state := parsed.Query().Get("state")
	if state == "" {
		b.t.Error("registration URL carried no state")
		return
	}

	base := strings.TrimSuffix(startURL, "/")
	b.get(ctx, base+"/callback?code=testcode&state="+url.QueryEscape(state))
}

func (b *browser) get(ctx context.Context, target string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		b.t.Errorf("build request: %v", err)
		return ""
	}

	resp, err := b.client.Do(req)
	if err != nil {
		// The flow closes its listener as soon as it is done, so a late request
		// failing is expected rather than a problem.
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	return string(body)
}

func extractFormAction(t *testing.T, page string) string {
	t.Helper()

	const marker = `action="`

	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}

	rest := page[i+len(marker):]

	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}

	return strings.ReplaceAll(rest[:j], "&amp;", "&")
}

// The whole handshake, end to end: manifest form, code exchange, install, and
// the installation id coming back. This is the path that gates every later
// phase's testing loop, so it is worth exercising rather than trusting.
func TestOnboardEndToEnd(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	result, err := Onboard(ctx, OnboardOptions{
		Org:         "acme",
		Name:        "billet",
		OpenBrowser: b.open,
		Log:         func(string, ...any) {},
		Client:      srv.Client(),
		InstallPoll: 20 * time.Millisecond,
		apiBase:     srv.URL,
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	if result.App.ID != fake.appID {
		t.Errorf("app id = %d, want %d", result.App.ID, fake.appID)
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want %d", result.Installation.ID, fake.installationID)
	}

	if result.App.PEM == "" {
		t.Error("no private key returned")
	}

	// The code is single-use; exchanging it twice would mean the flow retried
	// something it must not.
	if n := fake.conversions.Load(); n != 1 {
		t.Errorf("manifest conversions = %d, want exactly 1", n)
	}

	if len(b.visits) != 2 {
		t.Fatalf("operator was asked to visit %d URLs, want 2 (create, then install): %v",
			len(b.visits), b.visits)
	}

	if !strings.Contains(b.visits[1], "/installations/new") {
		t.Errorf("second visit should be the install page, got %s", b.visits[1])
	}
}

// A callback carrying the wrong state must be refused: it is the CSRF guard on
// the one endpoint an attacker could otherwise drive.
func TestOnboardRejectsStateMismatch(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	attacker := func(ctx context.Context, target string) error {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimSuffix(target, "/")+"/callback?code=evil&state=wrong", http.NoBody)

			resp, err := srv.Client().Do(req)
			if err == nil {
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("state mismatch answered %d, want 400", resp.StatusCode)
				}
			}
		}()

		return nil
	}

	_, err := Onboard(ctx, OnboardOptions{
		Org:         "acme",
		OpenBrowser: attacker,
		Log:         func(string, ...any) {},
		Client:      srv.Client(),
		InstallPoll: 20 * time.Millisecond,
		apiBase:     srv.URL,
	})
	if err == nil {
		t.Fatal("Onboard accepted a callback with a mismatched state")
	}

	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("expected a state-mismatch error, got: %v", err)
	}

	if n := fake.conversions.Load(); n != 0 {
		t.Errorf("the code was exchanged %d times despite the state mismatch", n)
	}
}

func TestOnboardRequiresOrgAndLog(t *testing.T) {
	if _, err := Onboard(t.Context(), OnboardOptions{Log: func(string, ...any) {}}); err == nil {
		t.Error("an empty org should be rejected")
	}

	if _, err := Onboard(t.Context(), OnboardOptions{Org: "acme"}); err == nil {
		t.Error("a missing Log should be rejected")
	}
}
