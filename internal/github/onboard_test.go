package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	// skipSetupCallback simulates an operator who closes the tab, leaving the
	// authenticated poller as the only route to the installation.
	skipSetupCallback bool
	setupCallbacks    atomic.Int32
	// getFailures counts requests that did not complete. A silently-failing
	// request is how the original version of this test passed without ever
	// reaching /installed.
	getFailures atomic.Int32
}

func (b *browser) open(ctx context.Context, target string) error {
	b.visits = append(b.visits, target)

	// The first URL is the loopback start page; the second is GitHub's install
	// page, which the test answers by "completing" the install.
	if strings.Contains(target, "/installations/new") {
		b.fake.installed.Store(true)

		if b.skipSetupCallback {
			// Exercise the poller alone: an operator who closed the tab, or
			// finished the install on another machine.
			return nil
		}

		// Built by trimming the trailing slash and appending, NOT by replacing the
		// first "/" — that replaces the one in "http://" and yields a hostless
		// URL whose request silently fails, letting the poller carry the test.
		installedURL := strings.TrimSuffix(b.visits[0], "/") + "/installed" +
			"?installation_id=" + fmt.Sprint(b.fake.installationID) + "&setup_action=install"

		b.setupCallbacks.Add(1)

		go b.get(ctx, installedURL)

		return nil
	}

	go b.driveRegistration(ctx, target)

	return nil
}

// driveRegistration fetches the self-submitting form, VALIDATES the manifest it
// carries against GitHub's documented schema, then plays GitHub's redirect back
// to /callback with a code and the state it was given.
//
// Validating here is the point. The earlier version only scraped the state and
// jumped straight to the callback, so the manifest was never inspected by
// anything — which is how a manifest missing the required hook_attributes.url
// passed a green test suite and would have failed on first contact with GitHub.
func (b *browser) driveRegistration(ctx context.Context, startURL string) {
	body := b.get(ctx, startURL)

	action := extractFormAction(b.t, body)
	if action == "" {
		b.t.Errorf("start page carried no form action:\n%s", body)
		return
	}

	b.validateManifest(extractManifest(b.t, body), strings.TrimSuffix(startURL, "/"))

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

// validateManifest enforces billet's manifest invariants.
//
// Only `url` and — when hook_attributes is present — `hook_attributes.url` are
// documented by GitHub as REQUIRED. Everything else here is billet's own
// requirement, held to deliberately: the callback URLs because onboarding
// depends on them, `public: false` and the OAuth fields because of what billet
// is for, and the whole hook_attributes object because GitHub documents
// `active` as defaulting to TRUE, so omitting the object is inferred to leave
// the webhook enabled. That inference is the conservative reading, not a
// promise from the parameter table.
//
// base is the loopback origin the onboarding server is listening on. It is
// passed in so the callback URLs can be asserted against what billet ACTUALLY
// serialized: this test drives /callback and /installed by constructing them
// from the same base, so tagging either field `json:"-"` left the suite green
// while GitHub would have had nowhere to redirect to.
func (b *browser) validateManifest(raw, base string) {
	b.t.Helper()

	if raw == "" {
		b.t.Error("the registration form carried no manifest")
		return
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		b.t.Errorf("manifest is not valid JSON: %v\n%s", err, raw)
		return
	}

	if s, ok := m["url"].(string); !ok || s == "" {
		b.t.Errorf("manifest.url is required by GitHub and is missing or not a string: %v", m["url"])
	}

	// Asserted against what billet serialized, not reconstructed from the same
	// base this test drives. Without these, tagging either field `json:"-"`
	// leaves the suite green while GitHub has nowhere to send the operator:
	// no redirect_url stalls Onboard until the manifest's one-hour deadline,
	// and no setup_url silently drops the fast installation path.
	for field, want := range map[string]string{
		"redirect_url": base + "/callback",
		"setup_url":    base + "/installed",
	} {
		if got, ok := m[field].(string); !ok || got != want {
			b.t.Errorf("manifest.%s = %v, want %q", field, m[field], want)
		}
	}

	// Registered for one organization's runners; installable by strangers is not
	// a thing billet should offer. `public` has no omitempty, so it must appear.
	if public, ok := m["public"].(bool); !ok || public {
		b.t.Errorf("manifest.public must be present and false, got %v", m["public"])
	}

	// Subscribing to events would contradict the no-webhook design.
	if events, present := m["default_events"]; present {
		if list, ok := events.([]any); !ok || len(list) > 0 {
			b.t.Errorf("manifest.default_events must be absent or empty, got %v", events)
		}
	}

	// request_oauth_on_install asks the installer to authorize the app as a
	// user, which billet has no OAuth flow for — and GitHub documents that
	// enabling it makes setup_url unavailable, which would quietly disable the
	// installation callback this same validator pins above. callback_urls
	// belongs to that flow too and must stay absent for the same reason.
	if v, present := m["request_oauth_on_install"]; present {
		if on, ok := v.(bool); !ok || on {
			b.t.Errorf("manifest.request_oauth_on_install must be absent or false, got %v", v)
		}
	}

	if urls, present := m["callback_urls"]; present {
		if list, ok := urls.([]any); !ok || len(list) > 0 {
			b.t.Errorf("manifest.callback_urls must be absent or empty; billet has no user OAuth flow, got %v", urls)
		}
	}

	// PRESENCE of the whole object is asserted, not just its contents. GitHub
	// documents hook_attributes.active as defaulting to TRUE, so a manifest that
	// omits the object entirely registers an ACTIVE webhook — and the previous
	// `if hook, ok := ...; ok` simply skipped the block, accepting exactly that.
	// billet's claim to need no inbound ingress rests on this.
	hook, ok := m["hook_attributes"].(map[string]any)
	if !ok {
		// Errorf and return, never Fatalf: this runs on the goroutine started by
		// driveRegistration, where FailNow would stop only that goroutine and let
		// the test go on to report some later, unrelated failure instead.
		b.t.Errorf("manifest.hook_attributes must be present and an object, got %v", m["hook_attributes"])

		return
	}

	// GitHub marks hook_attributes.url required whenever the object is present,
	// so an inactive hook still needs a URL.
	if s, ok := hook["url"].(string); !ok || s == "" {
		b.t.Error("manifest.hook_attributes is present but carries no url; GitHub rejects that")
	}

	// PRESENCE is asserted, not merely the value. `active, _ := ...(bool)` yields
	// false when the key is absent or the wrong type — which is the expected
	// value — so it passed without ever proving the manifest disables the
	// webhook.
	active, ok := hook["active"].(bool)

	switch {
	case !ok:
		b.t.Errorf("manifest.hook_attributes.active must be present and boolean, got %v", hook["active"])
	case active:
		b.t.Error("the webhook must be inactive: billet needs no inbound ingress")
	}

	perms, ok := m["default_permissions"].(map[string]any)
	if !ok {
		// Errorf, not Fatalf — see the hook_attributes note above: this runs on
		// driveRegistration's goroutine.
		b.t.Errorf("manifest carried no default_permissions: %s", raw)

		return
	}

	if len(perms) != len(permissions) {
		b.t.Errorf("manifest requests %d permissions, want %d: %v", len(perms), len(permissions), perms)
	}

	for name, want := range permissions {
		if got, ok := perms[name].(string); !ok || got != want {
			b.t.Errorf("manifest permission %q = %v, want %q", name, perms[name], want)
		}
	}

	// setup_on_update would point a future repository-access change at a loopback
	// port that stopped existing when onboarding finished. Absence is the
	// intended encoding; a present value must still be an explicit false rather
	// than any non-boolean that happens to read as one.
	if v, present := m["setup_on_update"]; present {
		off, ok := v.(bool)
		if !ok || off {
			b.t.Errorf("setup_on_update must be absent or explicitly false, got %v", v)
		}
	}
}

func (b *browser) get(ctx context.Context, target string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		b.t.Errorf("build request: %v", err)
		return ""
	}

	resp, err := b.client.Do(req)
	if err != nil {
		// Counted rather than ignored. Swallowing this is precisely how the
		// original test reported success while never reaching /installed; the
		// flow closes its listener when it finishes, so a late failure is
		// legitimate, but it must be visible to the assertions.
		b.getFailures.Add(1)

		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Errorf("read %s: %v", target, err)

		return ""
	}

	return string(body)
}

func extractFormAction(t *testing.T, page string) string {
	t.Helper()
	return unescapeHTML(extractAttr(page, `action="`))
}

// extractManifest pulls the manifest out of the hidden form field, undoing the
// HTML escaping html/template applied on the way in.
func extractManifest(t *testing.T, page string) string {
	t.Helper()
	return unescapeHTML(extractAttr(page, `name="manifest" value="`))
}

func extractAttr(page, marker string) string {
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}

	rest := page[i+len(marker):]

	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}

	return rest[:j]
}

func unescapeHTML(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&#34;", `"`,
		"&quot;", `"`,
		"&#39;", "'",
		"&lt;", "<",
		"&gt;", ">",
	)

	return r.Replace(s)
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

	var (
		persisted    []byte
		persistedAt  int
		installsSeen int
	)

	result, err := Onboard(ctx, OnboardOptions{
		Org:         "acme",
		Name:        "billet",
		OpenBrowser: b.open,
		Log:         func(string, ...any) {},
		Client:      srv.Client(),
		InstallPoll: 20 * time.Millisecond,
		apiBase:     srv.URL,
		OnAppCreated: func(app *App) error {
			persisted = []byte(app.PEM)
			// Record how many install pages had been opened when the key landed.
			// It must be zero: the key has to be durable BEFORE the installation
			// phase, since every failure there would otherwise orphan the app.
			persistedAt = installsSeen

			return nil
		},
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	installsSeen = len(b.visits)

	if len(persisted) == 0 {
		t.Fatal("OnAppCreated never received the private key")
	}

	if persistedAt != 0 {
		t.Errorf("key was persisted after %d install prompts; it must be saved before installation", persistedAt)
	}

	if result.App.ID != fake.appID {
		t.Errorf("app id = %d, want %d", result.App.ID, fake.appID)
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want %d", result.Installation.ID, fake.installationID)
	}

	// The code is single-use; exchanging it twice would mean the flow retried
	// something it must not.
	if n := fake.conversions.Load(); n != 1 {
		t.Errorf("manifest conversions = %d, want exactly 1", n)
	}

	// The setup callback must have actually been reached. Asserting this is what
	// stops the fast path silently regressing into "the poller carried it".
	if n := b.setupCallbacks.Load(); n != 1 {
		t.Errorf("setup callback fired %d times, want 1", n)
	}

	if n := b.getFailures.Load(); n != 0 {
		t.Errorf("%d browser requests failed; the callback URL is probably malformed", n)
	}

	if len(b.visits) != 2 {
		t.Fatalf("operator was asked to visit %d URLs, want 2 (create, then install): %v",
			len(b.visits), b.visits)
	}

	if !strings.Contains(b.visits[1], "/installations/new") {
		t.Errorf("second visit should be the install page, got %s", b.visits[1])
	}
}

// The poller alone must complete onboarding: an operator who closes the tab, or
// finishes the install on a different machine, still needs a working outcome.
func TestOnboardCompletesWithoutTheSetupCallback(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client(), skipSetupCallback: true}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want %d", result.Installation.ID, fake.installationID)
	}

	if n := b.setupCallbacks.Load(); n != 0 {
		t.Errorf("setup callback fired %d times; this test must exercise the poller alone", n)
	}
}

// A spoofed installation id must never reach the result. GitHub documents the
// setup-URL id as untrustworthy, and it ends up in billet.yaml deciding which
// installation runners register against.
func TestOnboardIgnoresSpoofedInstallationID(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}
	// The "attacker" claims a different id than the API will report.
	b.fake.installationID = 4242

	spoof := func(ctx context.Context, target string) error {
		if strings.Contains(target, "/installations/new") {
			fake.installed.Store(true)

			go b.get(ctx, strings.TrimSuffix(b.visits[0], "/")+"/installed?installation_id=999999999")

			return nil
		}

		return b.open(ctx, target)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  spoof,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	if result.Installation.ID == 999999999 {
		t.Fatal("the spoofed installation id from the setup callback was trusted")
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want the API's %d", result.Installation.ID, fake.installationID)
	}
}

// A permission the operator added between creating and installing the app must
// fail onboarding, not warn: billet would otherwise hold access it publicly
// claims not to have.
func TestOnboardFailsOnUnexpectedPermission(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.permissions["contents"] = "read"

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	saved := false

	_, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { saved = true; return nil },
	})
	if err == nil {
		t.Fatal("onboarding succeeded despite an unrequested `contents` permission")
	}

	if !strings.Contains(err.Error(), "permission mismatch") {
		t.Errorf("expected a permission-mismatch error, got: %v", err)
	}

	// The recovery URL has to WORK. Nothing pinned it, and it had been built by
	// appending "/installations/<id>" to a base already ending in
	// /installations — handing a 404 to an operator who is by definition
	// already stuck.
	want := "https://github.com/organizations/acme/settings/installations/909090"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("recovery URL is wrong.\n got: %v\nwant it to contain: %s", err, want)
	}

	// The key must still have been saved, or the failure is unrecoverable.
	if !saved {
		t.Error("the app key was not persisted before the failure; the app is now orphaned")
	}
}

// If credentials cannot be persisted, the flow must stop rather than proceed to
// installation and leave an app whose key was discarded.
func TestOnboardAbortsWhenCredentialsCannotBeSaved(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	_, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return errors.New("disk full") },
	})
	if err == nil {
		t.Fatal("onboarding continued despite failing to save credentials")
	}

	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("the underlying cause should be reported, got: %v", err)
	}

	// The operator must be told the app exists and needs deleting.
	if !strings.Contains(err.Error(), "delete it") {
		t.Errorf("the error should say the orphaned app must be deleted, got: %v", err)
	}

	// It must not have gone on to prompt for installation.
	if len(b.visits) != 1 {
		t.Errorf("flow prompted %d times; it should stop after the failed save", len(b.visits))
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

	// A hostile local process races the browser with a wrong state, then the
	// real registration proceeds. The forged request must be refused WITHOUT
	// ending the flow: this listens on loopback, so any unprivileged process can
	// reach it, and killing onboarding after GitHub has created the App but
	// before billet exchanges the one-time code orphans the App and its private
	// key. A local denial of service would become credential loss.
	browser := &browser{t: t, fake: fake, client: srv.Client()}

	var forged atomic.Int32

	attacked := func(ctx context.Context, target string) error {
		// Only the FIRST call is the loopback start page; the second is
		// GitHub's install URL, where /callback does not exist.
		if forged.Add(1) == 1 {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimSuffix(target, "/")+"/callback?code=evil&state=wrong", http.NoBody)
			if err != nil {
				return err
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				return err
			}

			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("forged state answered %d, want 400", resp.StatusCode)
			}
		}

		// The legitimate browser runs only after the forgery was refused.
		return browser.open(ctx, target)
	}

	app, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  attacked,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})

	if err != nil {
		t.Fatalf("a forged callback ended the flow: %v", err)
	}

	if app == nil || app.App == nil || app.App.ID == 0 {
		t.Fatal("onboarding produced no app despite the legitimate callback succeeding")
	}

	// Exactly one exchange: the forged code must never have been redeemed.
	if n := fake.conversions.Load(); n != 1 {
		t.Errorf("conversions = %d, want exactly 1 (the legitimate one)", n)
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
