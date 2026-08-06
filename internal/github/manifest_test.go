package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The manifest code buys the App's private key, and it is still live when the
// POST fails. net/http reports transport failures as *url.Error, whose Error()
// embeds the full URL — so a proxy misconfiguration or a DNS failure would print
// the code to stderr, where anyone reading a terminal scrollback, a systemd
// journal or a CI log could redeem it first.
func TestConvertManifestDoesNotLeakTheCodeOnTransportFailure(t *testing.T) {
	const code = "super-secret-one-time-code"

	// Accepting then closing gives a transport error rather than an HTTP status.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	base, client := srv.URL, srv.Client()

	srv.Close()

	_, err := convertManifestAt(t.Context(), client, base, code)
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("the one-time code appears in the error:\n%v", err)
	}

	// The underlying network error must survive, or an operator cannot tell a
	// proxy failure from a DNS one.
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("the transport error was discarded rather than redacted: %v", err)
	}
}

// Redacting only the OUTER *url.Error was not enough. http.Client.Do wraps
// whatever a RoundTripper returns, so a transport that itself produces a
// *url.Error leaves the inner one — carrying the live code — inside the
// retained Err.
func TestConvertManifestRedactsANestedURLError(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// Exactly the shape an instrumented transport produces.
		return nil, &url.Error{Op: "dial", URL: r.URL.String(), Err: errors.New("inner boom")}
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error from the failing transport")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("a nested url.Error leaked the one-time code:\n%v", err)
	}

	// The transport's own message must survive, or the operator loses the only
	// clue about what actually failed.
	if !strings.Contains(err.Error(), "inner boom") {
		t.Errorf("redaction discarded the transport's message: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The permission set is a security claim billet makes in its README and in the
// CLI's own output. Pin it, so widening it is a deliberate edit to a test rather
// than a quiet change to a map.
func TestPermissionsAreMinimal(t *testing.T) {
	want := map[string]string{
		"metadata":                         "read",
		"organization_self_hosted_runners": "write",
	}

	got := Permissions()

	if len(got) != len(want) {
		t.Fatalf("Permissions has %d entries, want %d: %v", len(got), len(want), got)
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("Permissions[%q] = %q, want %q", k, got[k], v)
		}
	}

	// Named explicitly: actions:read would expose workflow runs, logs and
	// artifacts, which is what makes "billet cannot read your code" false.
	for _, forbidden := range []string{"actions", "contents", "administration", "secrets"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("Permissions must not include %q", forbidden)
		}
	}
}

// Permissions must hand back a copy: it is a security claim, and a caller that
// can mutate it changes the manifest, the CLI's disclosure and the post-install
// validation in one go.
func TestPermissionsReturnsACopy(t *testing.T) {
	Permissions()["contents"] = "write"

	if _, leaked := Permissions()["contents"]; leaked {
		t.Fatal("mutating the returned map changed the canonical permission set")
	}
}

// GitHub marks hook_attributes.url required whenever the object is present, so
// an inactive hook still needs one. Omitting it rejects the whole registration.
func TestManifestCarriesAWebhookURLEvenThoughItIsInactive(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed")

	if m.HookAttributes.URL == "" {
		t.Error("hook_attributes.url is required by GitHub even when active is false")
	}

	if !strings.HasPrefix(m.HookAttributes.URL, "https://") {
		t.Errorf("hook url should be a stable https URL, got %q", m.HookAttributes.URL)
	}
}

// setup_on_update would send a later repository-access change to a loopback port
// that stopped existing when onboarding finished.
func TestManifestDoesNotAskForUpdateRedirects(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed")

	if m.SetupOnUpdate {
		t.Error("setup_on_update must be off: the setup URL is an ephemeral loopback listener")
	}
}

// billet long-polls; it must not register a webhook, because a webhook is what
// would force a deployment to accept inbound internet traffic.
func TestManifestDisablesWebhook(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed")

	if m.HookAttributes == nil {
		t.Fatal("manifest should carry hook_attributes so the webhook is explicitly off")
	}

	if m.HookAttributes.Active {
		t.Error("webhook must be inactive: billet needs no inbound ingress")
	}

	if m.Public {
		t.Error("an app scoped to one organization's runners must not be public")
	}
}

func TestManifestRoundTripsThroughJSON(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed")

	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// GitHub documents `url` as required; omitting it fails the registration with
	// a message that does not name the field.
	if decoded["url"] == "" || decoded["url"] == nil {
		t.Error("manifest must carry a url")
	}

	perms, ok := decoded["default_permissions"].(map[string]any)
	if !ok {
		t.Fatalf("default_permissions missing or wrong type: %T", decoded["default_permissions"])
	}

	if perms["organization_self_hosted_runners"] != "write" {
		t.Errorf("self-hosted runners permission = %v, want write", perms["organization_self_hosted_runners"])
	}
}

func TestRegistrationURLEscapesOrg(t *testing.T) {
	got := RegistrationURL("my org/evil", "st/ate+1")

	if strings.Contains(got, "my org") {
		t.Errorf("org was not escaped: %s", got)
	}

	if !strings.HasPrefix(got, "https://github.com/organizations/") {
		t.Errorf("unexpected prefix: %s", got)
	}

	if !strings.Contains(got, "settings/apps/new?state=") {
		t.Errorf("missing state parameter: %s", got)
	}
}

func TestConvertManifestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		if got := r.Header.Get("X-Github-Api-Version"); got == "" {
			t.Error("missing X-GitHub-Api-Version header")
		}

		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte(`{"id":42,"slug":"billet-acme","name":"billet","html_url":"https://github.com/apps/billet-acme","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n"}`)); err != nil {
			t.Errorf("write test response: %v", err)
		}
	}))
	defer srv.Close()

	app, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "code123")
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}

	if app.ID != 42 {
		t.Errorf("id = %d, want 42", app.ID)
	}

	if got := app.InstallURL(); got != "https://github.com/apps/billet-acme/installations/new" {
		t.Errorf("InstallURL = %q", got)
	}
}

// App is exactly the sort of value that ends up in a debug print or a wrapped
// error, and a plain %v would otherwise emit the App's private key.
func TestAppFormattingRedactsCredentials(t *testing.T) {
	app := App{
		ID:            42,
		Slug:          "billet-acme",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYMATERIAL\n",
		WebhookSecret: "SECRETWEBHOOK",
		ClientSecret:  "SECRETCLIENT",
	}

	secrets := []string{"SECRETKEYMATERIAL", "SECRETWEBHOOK", "SECRETCLIENT"}

	// Routed through `any` deliberately. Calling fmt.Sprintf on the concrete
	// type makes staticcheck and gocritic suggest app.String() instead — which
	// would test the method and not the FORMATTING PATH, and the formatting path
	// is the whole risk: somebody prints an App without thinking about it.
	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	// Every verb a caller might reach for, including the pointer forms (a
	// value-receiver method is also in *App's method set) and verbs that make no
	// sense for a struct. %d is the important one: fmt consults Stringer only
	// for %v/%s/%q/%x/%X, and any other verb formats the fields recursively —
	// printing the private key inside its own bad-verb diagnostic.
	for _, rendered := range []string{
		render("%v", app), render("%s", app), render("%#v", app), render("%q", app),
		render("%d", app), render("%x", app), render("%t", app), render("%+v", app),
		render("%v", &app), render("%s", &app), render("%#v", &app), render("%d", &app),
	} {
		for _, secret := range secrets {
			if strings.Contains(rendered, secret) {
				t.Errorf("formatting leaked %q:\n%s", secret, rendered)
			}
		}
	}

	// Still useful for diagnosis.
	if !strings.Contains(render("%v", app), "billet-acme") {
		t.Error("redaction removed the identifying fields too")
	}
}

// Onboard hands this struct back after the key is on disk. Nothing downstream
// needs a credential, and billet never persists the webhook or client secret —
// it registers an inactive webhook and implements no OAuth flow.
func TestForgetClearsEverySecret(t *testing.T) {
	app := &App{
		ID:            42,
		PEM:           "key",
		WebhookSecret: "hook",
		ClientID:      "Iv1.public",
		ClientSecret:  "secret",
	}

	app.Forget()

	switch {
	case app.PEM != "":
		t.Error("Forget left the private key")
	case app.WebhookSecret != "":
		t.Error("Forget left the webhook secret")
	case app.ClientSecret != "":
		t.Error("Forget left the client secret")
	}

	// ClientID is not a secret and is the value billet may later persist.
	if app.ClientID != "Iv1.public" {
		t.Errorf("Forget discarded the non-secret client id: %q", app.ClientID)
	}

	if app.ID != 42 {
		t.Errorf("Forget discarded the app id: %d", app.ID)
	}
}

// A response that parses but carries no id or key is unusable, and accepting it
// would surface much later as an inscrutable auth failure.
func TestConvertManifestRejectsIncompleteResponse(t *testing.T) {
	for name, body := range map[string]string{
		"no id":  `{"slug":"x","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n"}`,
		"no pem": `{"id":42,"slug":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				if _, err := w.Write([]byte(body)); err != nil {
					t.Errorf("write test response: %v", err)
				}
			}))
			defer srv.Close()

			if _, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "code123"); err == nil {
				t.Fatal("expected an error for an incomplete response")
			}
		})
	}
}

// GitHub's error body explains the failure far better than the status alone —
// notably the expired-code case, which is the one an operator actually hits.
func TestConvertManifestSurfacesGitHubMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		if _, err := w.Write([]byte(`{"message":"The code passed is incorrect or expired."}`)); err != nil {
			t.Errorf("write test response: %v", err)
		}
	}))
	defer srv.Close()

	_, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "stale")
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "incorrect or expired") {
		t.Errorf("error should carry GitHub's message, got: %v", err)
	}
}

func TestConvertManifestRejectsEmptyCode(t *testing.T) {
	if _, err := ConvertManifest(t.Context(), nil, ""); err == nil {
		t.Fatal("expected an error for an empty code")
	}
}
