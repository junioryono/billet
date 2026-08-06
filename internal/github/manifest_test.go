package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
