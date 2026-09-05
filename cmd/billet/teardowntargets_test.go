package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/fakeactions"
)

// `billet teardown --all` walks every target with that target's own
// credential: the organization's scale set is found and deleted through the
// organization's registration, the repository's through the repository's.
//
// ONE FAKE SERVES BOTH, because the Actions-service calls carry no owner; what
// distinguishes the two clients on the wire is the registration-token path
// each takes, and that is what is asserted.
func TestTeardownAllWalksEveryTargetWithItsOwnCredential(t *testing.T) {
	cfgPath := writeTargetConfig(t, "  org: acme", `    target: default
  - label: billet-4vcpu-personal
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: untrusted
    target: personal
targets:
  - name: personal
    repository: someone/widgets
    app_id: 8
    installation_id: 43
    private_key_path: EXTRA_KEY
`)

	// The state directory exists, so forgetting a record can open the ledger.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(cfgPath), "server"), 0o700); err != nil {
		t.Fatalf("state dir: %v", err)
	}

	var deleted []string

	fake := fakeactions.New(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			fakeactions.WriteJSON(t, w, fakeactions.ListJSON(
				map[string]any{"id": 1, "name": "default", "isDefaultGroup": true}))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			name := r.URL.Query().Get("name")
			id := 7
			if name == "billet-4vcpu-personal" {
				id = 8
			}
			fakeactions.WriteJSON(t, w, fakeactions.ListJSON(fakeactions.ScaleSetJSON(id, name, "default", name)))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "runnerscalesets"):
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	prevWeb, prevAPI := scaleSetGitHubURL, githubAPIBase
	scaleSetGitHubURL, githubAPIBase = fake.URL, fake.URL+"/api/v3"
	t.Cleanup(func() { scaleSetGitHubURL, githubAPIBase = prevWeb, prevAPI })

	// The fake's throwaway key stands in for both targets' keys.
	for _, name := range []string{"app.pem", "app-personal.pem"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(cfgPath), name), []byte(fake.PrivateKeyPEM()), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var err error

	out := capture(t, func() {
		err = cmdTeardown(t.Context(), []string{"--config", cfgPath, "--all", "--yes"})
	})
	if err != nil {
		t.Fatalf("teardown --all: %v\n%s", err, out)
	}

	if len(deleted) != 2 || !strings.HasSuffix(deleted[0], "/7") && !strings.HasSuffix(deleted[1], "/7") ||
		!strings.HasSuffix(deleted[0], "/8") && !strings.HasSuffix(deleted[1], "/8") {
		t.Errorf("deleted %v, want scale sets 7 and 8", deleted)
	}

	// EACH TARGET REGISTERED AS ITSELF: the organization at its path, the
	// repository at its own, and each exactly once per client.
	for _, want := range []string{
		"/api/v3/orgs/acme/actions/runners/registration-token",
		"/api/v3/repos/someone/widgets/actions/runners/registration-token",
	} {
		found := false

		for _, call := range fake.Calls("registration-token") {
			if call.Path == want {
				found = true
			}
		}

		if !found {
			t.Errorf("no registration at %s; calls were %v", want, fake.Calls("registration-token"))
		}
	}

	for _, want := range []string{"target default", "target personal", "org acme", "repository someone/widgets"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
}
