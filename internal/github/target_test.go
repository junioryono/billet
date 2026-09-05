package github

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestThePermissionSetIsChosenByScopeAndOnlyThere parses this package and holds
// that permissionsFor is the ONLY reader of the two permission maps, so no code
// path can request both sets or the wrong one — and that the two sets share
// exactly metadata, which is the read GitHub requires of every App.
func TestThePermissionSetIsChosenByScopeAndOnlyThere(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	readers := map[string][]string{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}

				if id.Name == "organizationPermissions" || id.Name == "repositoryPermissions" {
					readers[id.Name] = append(readers[id.Name], fn.Name.Name)
				}

				return true
			})
		}
	}

	for _, name := range []string{"organizationPermissions", "repositoryPermissions"} {
		got := readers[name]
		if len(got) != 1 || got[0] != "permissionsFor" {
			t.Errorf("%s is read by %v; the only reader may be permissionsFor, so the set is chosen "+
				"by scope in one place", name, got)
		}
	}

	org, repo := Permissions(ScopeOrganization), Permissions(ScopeRepository)

	var shared []string

	for name := range org {
		if _, both := repo[name]; both {
			shared = append(shared, name)
		}
	}

	if !slices.Equal(shared, []string{"metadata"}) {
		t.Errorf("the two sets share %v, want exactly [metadata]", shared)
	}

	if repo["administration"] != "write" || len(repo) != 2 {
		t.Errorf("the repository set is %v, want metadata:read and administration:write", repo)
	}

	if _, leaked := org["administration"]; leaked {
		t.Error("the organization set requests administration, the repository permission")
	}
}

func TestRepositoryPermissionsAreValidatedAgainstTheRepositorySet(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		granted map[string]string
		want    []string
	}{
		"exact": {
			granted: map[string]string{"metadata": "read", "administration": "write"},
		},
		"the organization set on a repository": {
			granted: map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
			want: []string{
				"administration: want write, not granted",
				"organization_self_hosted_runners: granted write, but billet never requested it",
			},
		},
		"administration read only": {
			granted: map[string]string{"metadata": "read", "administration": "read"},
			want:    []string{"administration: want write, granted read"},
		},
		"contents added": {
			granted: map[string]string{"metadata": "read", "administration": "write", "contents": "read"},
			want:    []string{"contents: granted read, but billet never requested it"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			inst := &Installation{Permissions: tc.granted}

			if got := inst.PermissionMismatches(ScopeRepository); !slices.Equal(got, tc.want) {
				t.Errorf("PermissionMismatches(repository) = %v, want %v", got, tc.want)
			}
		})
	}

	// AND THE ORGANIZATION SET REFUSES ADMINISTRATION, the direction that
	// falsifies "billet cannot change your repositories".
	inst := &Installation{Permissions: map[string]string{"metadata": "read",
		"organization_self_hosted_runners": "write", "administration": "write"}}
	if got := inst.PermissionMismatches(ScopeOrganization); len(got) != 1 ||
		!strings.Contains(got[0], "administration: granted write, but billet never requested it") {
		t.Errorf("an organization installation holding administration was not refused: %v", got)
	}
}

func TestVerifyAsksTheTargetsOwnInstallationEndpoint(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		target Target
		path   string
		body   string
	}{
		"an organization": {
			target: OrganizationTarget("acme"),
			path:   "/orgs/acme/installation",
			body:   `{"id": 42, "account": {"login": "acme", "type": "Organization"}, "permissions": {"metadata": "read", "organization_self_hosted_runners": "write"}}`,
		},
		"a repository": {
			target: RepositoryTarget("some one", "wid gets"),
			path:   "/repos/some%20one/wid%20gets/installation",
			body:   `{"id": 42, "account": {"login": "some one", "type": "User"}, "permissions": {"metadata": "read", "administration": "write"}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var asked string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = r.URL.EscapedPath()
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			if _, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(), tc.target, 42); err != nil {
				t.Fatalf("verifyAppAt: %v", err)
			}

			if asked != tc.path {
				t.Errorf("GitHub was asked %q, want %q", asked, tc.path)
			}
		})
	}
}

func TestARepositoryInstallationIsHeldToTheRepositorySet(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusOK, `{"id": 42, "account": {"login": "someone", "type": "User"},
		"permissions": {"metadata": "read", "organization_self_hosted_runners": "write"}}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(),
		RepositoryTarget("someone", "widgets"), 42)
	if err == nil {
		t.Fatal("a repository installation holding the organization set was accepted")
	}

	// The review page follows the account's kind: a user's installations live
	// under /settings, with no owner in the path.
	if !strings.Contains(err.Error(), "administration: want write, not granted") ||
		!strings.Contains(err.Error(), webBase+"/settings/installations") {
		t.Errorf("the refusal does not name the mismatch and the user's settings page: %v", err)
	}
}

func TestANotInstalledRepositoryNamesTheRepository(t *testing.T) {
	t.Parallel()

	srv, _ := verifyFake(t, http.StatusNotFound, `{"message":"Not Found"}`)

	_, err := verifyAppAt(t.Context(), srv.Client(), srv.URL, 7, testAppKey(),
		RepositoryTarget("someone", "widgets"), 42)
	if err == nil || !strings.Contains(err.Error(), `not installed on repository "someone/widgets"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRegistrationURLFollowsTheOwnerKind(t *testing.T) {
	t.Parallel()

	if got, want := RegistrationURL("someone", OwnerUser, "st"), webBase+"/settings/apps/new?state=st"; got != want {
		t.Errorf("a user's form is %q, want %q", got, want)
	}

	if got, want := RegistrationURL("acme", OwnerOrganization, "st"),
		webBase+"/organizations/acme/settings/apps/new?state=st"; got != want {
		t.Errorf("an organization's form is %q, want %q", got, want)
	}

	if got, want := SettingsURL("someone", OwnerUser), webBase+"/settings/installations"; got != want {
		t.Errorf("a user's settings page is %q, want %q", got, want)
	}

	if got, want := SettingsURL("acme", OwnerOrganization), webBase+"/organizations/acme/settings/installations"; got != want {
		t.Errorf("an organization's settings page is %q, want %q", got, want)
	}
}

func TestResolveOwnerTypeReadsGitHubsAnswer(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		body   string
		want   OwnerType
		refuse string
	}{
		"a user":          {status: http.StatusOK, body: `{"login":"someone","type":"User"}`, want: OwnerUser},
		"an organization": {status: http.StatusOK, body: `{"login":"acme","type":"Organization"}`, want: OwnerOrganization},
		"nobody":          {status: http.StatusNotFound, body: `{"message":"Not Found"}`, refuse: "no account named"},
		"something else":  {status: http.StatusOK, body: `{"type":"Bot"}`, refuse: "neither a user nor an organization"},
		"an outage":       {status: http.StatusBadGateway, body: `bad`, refuse: "HTTP 502"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var asked string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = r.URL.EscapedPath()
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			got, err := resolveOwnerTypeAt(t.Context(), srv.Client(), srv.URL, "some one")

			if asked != "/users/some%20one" {
				t.Errorf("asked %q, want the public users endpoint", asked)
			}

			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("got %q, %v; want a refusal carrying %q", got, err, tc.refuse)
				}

				return
			}

			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestOnboardingARepositoryFollowsItsOwner drives the whole flow for a
// repository target twice — owned by a user, then by an organization — and the
// browser asserts the form each posts to and the permission set each manifest
// carries, while the fake answers the installation only at the repository's own
// endpoint.
func TestOnboardingARepositoryFollowsItsOwner(t *testing.T) {
	for _, kind := range []OwnerType{OwnerUser, OwnerOrganization} {
		t.Run(string(kind), func(t *testing.T) {
			fake := newFakeRepository(t, kind)

			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			b := &browser{t: t, fake: fake, client: srv.Client()}

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			result, err := Onboard(ctx, OnboardOptions{
				Target:       fake.target,
				Name:         "billet",
				OpenBrowser:  b.open,
				Log:          func(string, ...any) {},
				Client:       srv.Client(),
				InstallPoll:  20 * time.Millisecond,
				apiBase:      srv.URL,
				OnAppCreated: func(*App) error { return nil },
			})

			b.pending.Wait()

			if err != nil {
				t.Fatalf("Onboard: %v", err)
			}

			if result.Installation.ID != fake.installationID {
				t.Errorf("installation %d, want %d", result.Installation.ID, fake.installationID)
			}

			if got := result.Installation.Account.Type; got != string(kind) {
				t.Errorf("the installation is on a %q, want %q", got, kind)
			}
		})
	}
}

func TestOnboardingARepositoryRefusesWhenTheOwnerCannotBeResolved(t *testing.T) {
	// The fake knows one owner; the flow is pointed at another, so the lookup
	// answers 404 the way GitHub does for an account that does not exist.
	fake := newFakeRepository(t, OwnerUser)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	opened := false

	_, err := Onboard(t.Context(), OnboardOptions{
		Target:       RepositoryTarget("nobody", "widgets"),
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
		OpenBrowser: func(context.Context, string) error {
			opened = true

			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no account named") {
		t.Fatalf("expected the owner lookup's refusal, got %v", err)
	}

	// Refused BEFORE the browser, where refusing is free: nothing has been
	// registered and no key has been issued.
	if opened || fake.conversions.Load() != 0 {
		t.Error("the browser was opened, or a code exchanged, for an owner that does not exist")
	}
}

func TestRunnerGroupQuestionsAreRefusedForARepository(t *testing.T) {
	t.Parallel()

	key, _ := testKeyPKCS1(t)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a repository target reached GitHub for a runner-group question: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	c := newRunnerGroupPolicyClient(srv.Client(), srv.URL, RepositoryTarget("acme", "widgets"), 11, 22, key)
	c.token = "cached-token"
	c.expiresAt = time.Now().Add(time.Hour)

	if err := c.ValidateTrustedRunnerGroup(t.Context(), 7, nil); !errors.Is(err, ErrNoRunnerGroups) {
		t.Errorf("ValidateTrustedRunnerGroup: %v, want ErrNoRunnerGroups", err)
	}

	if err := c.ValidateRunnerGroupReach(t.Context(), 7); !errors.Is(err, ErrNoRunnerGroups) {
		t.Errorf("ValidateRunnerGroupReach: %v, want ErrNoRunnerGroups", err)
	}

	if _, _, err := c.FindRunnerGroupID(t.Context(), "billet"); !errors.Is(err, ErrNoRunnerGroups) {
		t.Errorf("FindRunnerGroupID: %v, want ErrNoRunnerGroups", err)
	}
}

func TestRunnerRecoveryListsTheRepositorysRunners(t *testing.T) {
	t.Parallel()

	key, _ := testKeyPKCS1(t)

	var asked string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widgets/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		fmt.Fprint(w, `{"total_count":1,"runners":[{"id":71,"name":"billet-l1","status":"online","busy":true,"ephemeral":true}]}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newRunnerGroupPolicyClient(srv.Client(), srv.URL, RepositoryTarget("acme", "widgets"), 11, 22, key)
	c.token = "cached-token"
	c.expiresAt = time.Now().Add(time.Hour)

	got, err := c.InspectScaleSetRunner(t.Context(), "billet-l1", 71)
	if err != nil {
		t.Fatalf("InspectScaleSetRunner: %v", err)
	}

	if asked != "/repos/acme/widgets/actions/runners" || !got.Present || !got.Busy {
		t.Errorf("asked %q and recovered %+v", asked, got)
	}

	if !strings.Contains(c.String(), `target:"acme/widgets"`) || strings.Contains(c.String(), "PRIVATE") {
		t.Errorf("the client renders as %s", c.String())
	}
}

func TestTargetPathsAndEndpoints(t *testing.T) {
	t.Parallel()

	org := OrganizationTarget("acme")
	repo := RepositoryTarget("some one", "wid/gets")

	if org.Path() != "acme" || org.Scope() != ScopeOrganization || org.IsZero() {
		t.Errorf("organization target: %+v", org)
	}

	if repo.Path() != "some one/wid/gets" || repo.Scope() != ScopeRepository {
		t.Errorf("repository target: %+v", repo)
	}

	if got := repo.installationEndpoint("https://api"); got != "https://api/repos/some%20one/wid%2Fgets/installation" {
		t.Errorf("repository installation endpoint: %s", got)
	}

	if got := org.runnerGroupsEndpoint("https://api"); got != "https://api/orgs/acme/actions/runner-groups" {
		t.Errorf("organization runner-group endpoint: %s", got)
	}

	if !(Target{}).IsZero() {
		t.Error("the zero target is not zero")
	}
}
