package scaleset

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	billetgithub "github.com/junioryono/billet/internal/github"
)

// repositoryConfig points billet's client at the fake as a repository target.
func (f *fakeActions) repositoryConfig(t *testing.T) Config {
	t.Helper()

	cfg := f.config(t)
	cfg.Target = billetgithub.RepositoryTarget("acme", "widgets")

	return cfg
}

// A repository target registers at the REPOSITORY registration-token path, and
// that is asserted on the wire: the vendored client decides its scope from the
// config URL's path, so billet building that URL with one segment too few
// would register against an organization named "acme" and report nothing.
func TestARepositoryTargetRegistersAtTheRepositoryPath(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": DefaultRunnerGroup, "isDefaultGroup": true}))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON())
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, scaleSetJSON(7, "billet-4vcpu"))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.repositoryConfig(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := client.Target().Path(); got != "acme/widgets" {
		t.Errorf("the client names target %q", got)
	}

	if _, err := client.EnsureScaleSet(t.Context(), "billet-4vcpu", "", []string{"billet-4vcpu"}); err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}

	tokens := fake.calls("registration-token")
	if len(tokens) == 0 {
		t.Fatal("billet never asked for a registration token")
	}

	// The whole path, not a fragment. /orgs/acme/... would also contain
	// "registration-token", and it is exactly the wrong scope.
	const want = "/api/v3/repos/acme/widgets/actions/runners/registration-token"
	if tokens[0].Path != want {
		t.Errorf("registered at %q, want %q", tokens[0].Path, want)
	}
}

// AND THE ORGANIZATION FORM STILL LANDS ON THE ORGANIZATION PATH, so the test
// above cannot pass by every target registering at /repos/.
func TestAnOrganizationTargetRegistersAtTheOrganizationPath(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(7, "billet-4vcpu")))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := client.Describe(t.Context(), "billet-4vcpu", "billet"); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	tokens := fake.calls("registration-token")
	if len(tokens) == 0 || tokens[0].Path != "/api/v3/orgs/acme/actions/runners/registration-token" {
		t.Errorf("registered at %v, want the organization path", tokens)
	}
}

func TestARepositoryTargetRefusesANamedRunnerGroupBeforeAsking(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a named group on a repository target reached the service: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	client, err := New(fake.repositoryConfig(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.EnsureScaleSet(t.Context(), "billet-4vcpu", "billet", nil); !errors.Is(err, billetgithub.ErrNoRunnerGroups) {
		t.Errorf("EnsureScaleSet: %v, want ErrNoRunnerGroups", err)
	}

	if _, _, err := client.Describe(t.Context(), "billet-4vcpu", "billet"); !errors.Is(err, billetgithub.ErrNoRunnerGroups) {
		t.Errorf("Describe: %v, want ErrNoRunnerGroups", err)
	}

	if _, err := client.DeleteScaleSet(t.Context(), "billet-4vcpu", "billet", nil, false); !errors.Is(err, billetgithub.ErrNoRunnerGroups) {
		t.Errorf("DeleteScaleSet: %v, want ErrNoRunnerGroups", err)
	}

	if err := client.ValidateTrustedRunnerGroup(t.Context(), "billet", nil); !errors.Is(err, billetgithub.ErrNoRunnerGroups) {
		t.Errorf("ValidateTrustedRunnerGroup: %v, want ErrNoRunnerGroups", err)
	}

	// The default group, named or implied, is the one place a repository's scale
	// set can be, and it goes through to the service.
	if fake.calls("registration-token") != nil {
		t.Error("a refused call still fetched a registration token")
	}
}

func TestAClientNeedsATarget(t *testing.T) {
	fake := newFakeActions(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })

	cfg := fake.config(t)
	cfg.Target = billetgithub.Target{}

	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "no target") {
		t.Errorf("New with no target: %v", err)
	}
}
