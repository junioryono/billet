package integration_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
)

// CONFIGURATION IS VALIDATED IN ONE PACKAGE AND CONSUMED IN ANOTHER, and the
// defects these tests exist for live in the gap.
//
// internal/config cannot import any other billet package — depguard enforces it,
// tests included — so nothing inside it can prove that the value it accepted is
// the value the runtime uses. Every check here therefore assembles the
// production pieces the way cmd/billet does and asserts the OPERATIONAL
// consequence: a node registers, or a request reaches GitHub naming the right
// organization.

const boundaryDeployment = "0123456789abcdef0123456789abcdef"

// writeConfig puts a config on disk so config.Load — not a hand-built struct —
// is what the test exercises.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// controlPlaneConfig is a deployment declaring sites, with a node in one of
// them. The `site:` values are placeholders the cases below substitute.
const controlPlaneConfig = `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 120
  max_memory: 480GiB
github:
  org: acme-boundary-27
  app_id: 12345
  installation_id: 67890
  private_key_path: /etc/billet/app.pem
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: /var/lib/billet/node
  site: NODE_SITE
  firecracker:
    kernel_image: /var/lib/billet/vmlinux
    bridge: br0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: billet-4vcpu-ubuntu-2404
    provider: firecracker
    site: TIER_SITE
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
sites:
  - name: DECLARED_SITE
    store: ceph
  - name: office
    store: ceph
`

func boundaryConfig(declared, tier, node string) string {
	body := strings.Replace(controlPlaneConfig, "DECLARED_SITE", declared, 1)
	body = strings.Replace(body, "TIER_SITE", tier, 1)

	return strings.Replace(body, "NODE_SITE", node, 1)
}

// registerAt is what `billet node` sends: the site string straight out of its
// own loaded configuration (cmd/billet's runNode), unmodified.
//
// The node NAME varies with the caller, because the interesting case is several
// hosts each in one of the declared places — reusing one name would make the
// second registration a re-registration of the first, which is a different thing
// to be proving.
func registerAt(node, site string, provider config.ProviderKind) nodeapi.RegisterRequest {
	return nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        node,
		Provider:    provider,
		Deployment:  boundaryDeployment,
		Incarnation: "00000000000000000000000000000001",
		Site:        site,
		VCPU:        8,
		Memory:      32 * config.GiB,
	}
}

// A SITE THAT VALIDATION ACCEPTS IS ONE A NODE CAN REGISTER INTO.
//
// This is the property the bug broke, and it is only visible across the two
// packages. validateSites built its authority set from a TRIMMED copy of each
// name and wrote nothing back, while nodeplane.WithSites keys the runtime map on
// the raw SiteConfig.Name and Register looks up the node's exact string. A
// deployment declaring " home " therefore authorised `tiers[].site: home` at
// load and then refused, permanently, every node reporting `home` — a tier that
// could never be placed, with nothing at startup saying why.
//
// The property covers both outcomes rather than asserting one: a configuration
// carrying that disagreement must not LOAD, and one that loads must not
// disagree. Asserting only the refusal would be satisfied by a config package
// that refuses everything.
func TestASiteThatValidationAcceptsIsOneANodeCanRegisterInto(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ declared, tier, node string }{
		"canonical throughout": {"home", "home", "office"},
		"a padded declaration": {`" home "`, "home", "office"},
		"a padded tier site":   {"home", `" home "`, "office"},
		"a padded node site":   {"home", "home", `" office "`},
		"padded on both sides": {`" home "`, `" home "`, "office"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(writeConfig(t, boundaryConfig(tc.declared, tc.tier, tc.node)))
			if err != nil {
				// Refused at load: the disagreement never reaches a control
				// plane, which is the other acceptable answer.
				return
			}

			// THE REAL WIRING, as runServer builds it (cmd/billet/main.go).
			plane := nodeplane.New(slog.New(slog.DiscardHandler), boundaryDeployment, time.Minute,
				nodeplane.WithSites(cfg.Sites))

			// Every site this configuration lets something refer to must be one a
			// node can actually join, using the exact string its own config
			// carries.
			claims := map[string]string{"node.site": cfg.Node.Site}
			for i := range cfg.Tiers {
				if cfg.Tiers[i].Site != "" {
					claims[fmt.Sprintf("tiers[%d].site", i)] = cfg.Tiers[i].Site
				}
			}

			host := 0

			for where, site := range claims {
				if site == "" {
					continue
				}

				host++

				if _, err := plane.Register(t.Context(),
					registerAt(fmt.Sprintf("host-%d", host), site,
						cfg.Node.Provider)); err != nil {
					t.Errorf("%s is %q, which config validation accepted, but a node reporting "+
						"it is refused at registration: %v", where, site, err)
				}
			}
		})
	}
}

// AND THE SPLIT IS DELIBERATE IN THE OTHER DIRECTION: a node's OWN file cannot
// judge its site, because sites are the control plane's to declare and a node's
// config has no reason to list them. So an undeclared name loads and is refused
// where the answer lives.
func TestAnUndeclaredNodeSiteLoadsLocallyAndIsRefusedAtTheControlPlane(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(writeConfig(t, boundaryConfig("home", "home", "hom")))
	if err != nil {
		t.Fatalf("a node naming a site its own file does not declare was refused at load: %v", err)
	}

	plane := nodeplane.New(slog.New(slog.DiscardHandler), boundaryDeployment, time.Minute,
		nodeplane.WithSites(cfg.Sites))

	_, err = plane.Register(t.Context(), registerAt("epyc-1", cfg.Node.Site, cfg.Node.Provider))
	if err == nil {
		t.Fatal("a node registered into a site the control plane never declared")
	}
	if !strings.Contains(err.Error(), "hom") {
		t.Errorf("the refusal does not name the site that was claimed: %v", err)
	}
}

// boundaryAppKey mints one RSA key for this file; the key's identity is
// irrelevant to what is being asserted and generation is the slow part.
var boundaryAppKey = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
})

// THE ORGANIZATION IN THE CONFIG IS THE ORGANIZATION GITHUB IS ASKED ABOUT.
//
// validateGitHub trimmed only to decide whether the value was empty, while every
// consumer used the raw string — concatenated unescaped into the scale-set
// client's config URL and PathEscape'd into the REST path. So the assertion has
// to be about what GitHub RECEIVES, not about the validator's local copy: this
// drives the real request builder and reads the path off the wire.
func TestTheConfiguredOrgIsTheOneGitHubIsAskedAbout(t *testing.T) {
	t.Parallel()

	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		if _, err := w.Write([]byte(`{"id": 67890, "permissions": {
			"metadata": "read", "organization_self_hosted_runners": "write"}}`)); err != nil {
			t.Errorf("write the fake installation response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	cfg, err := config.Load(writeConfig(t, boundaryConfig("home", "home", "office")))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// A DISTINCTIVE LITERAL, ASSERTED TWICE AGAINST THE LITERAL rather than
	// against cfg.GitHub.Org. Deriving the expected path from the loaded value
	// makes the assertion true by construction: with an org of "acme", a
	// production call that hardcoded "acme" would leave this green, and nothing
	// here would prove the scalar survived Load either.
	const org = "acme-boundary-27"

	if cfg.GitHub.Org != org {
		t.Fatalf("the loaded org is %q, not the %q the file declares", cfg.GitHub.Org, org)
	}

	if _, err := github.VerifyAppAt(t.Context(), srv.Client(), srv.URL,
		cfg.GitHub.AppID, boundaryAppKey(), github.OrganizationTarget(cfg.GitHub.Org),
		cfg.GitHub.InstallationID); err != nil {
		t.Fatalf("VerifyAppAt: %v", err)
	}

	if got != "/orgs/"+org+"/installation" {
		t.Errorf("GitHub was asked about %q, but the config names %q", got, org)
	}
}

// AND AN ORG THAT WOULD NAME SOMETHING ELSE NEVER GETS THAT FAR.
//
// `acme/api` is the sharp one: billet builds the client's config URL as
// "https://github.com/" + org unescaped, and the vendored client reads a second
// path segment as a REPOSITORY — so it resolves to a different scope entirely
// and reports nothing at all. Refusing at load is what keeps that from being a
// runtime mystery.
func TestAnOrgThatWouldNameSomethingElseIsRefusedBeforeItIsUsed(t *testing.T) {
	t.Parallel()

	// QUOTED IN THE FIXTURE, because an unquoted `org: acme # prod` is not this
	// case at all — YAML reads the tail as a comment and the value is `acme`,
	// which loads happily. That is the hazard initconfig's yamlScalar exists for,
	// and writing the fixture wrong here would have tested nothing.
	for name, org := range map[string]string{
		"a repository": `"acme/api"`,
		"a comment":    `"acme # prod"`,
		"padding":      `" acme "`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := strings.Replace(boundaryConfig("home", "home", "office"),
				"  org: acme-boundary-27\n", "  org: "+org+"\n", 1)

			if _, err := config.Load(writeConfig(t, body)); err == nil {
				t.Fatalf("Load accepted github.org %s", org)
			}
		})
	}
}

// AND A REPOSITORY TARGET IS ASKED ABOUT AT THE REPOSITORY ENDPOINT, escaped
// segment by segment — the scope config accepted is the scope the REST
// boundary uses, which is the whole of what a repository-scoped `github:`
// block has to mean.
func TestTheConfiguredRepositoryIsTheOneGitHubIsAskedAbout(t *testing.T) {
	t.Parallel()

	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		if _, err := w.Write([]byte(`{"id": 67890, "account": {"login": "owner-27", "type": "User"},
			"permissions": {"metadata": "read", "administration": "write"}}`)); err != nil {
			t.Errorf("write the fake installation response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	body := strings.Replace(boundaryConfig("home", "home", "office"),
		"  org: acme-boundary-27\n", "  repository: \"owner-27/wid gets\"\n", 1)

	cfg, err := config.Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	target := cfg.GitHubTargets()[0]
	if !target.IsRepository() || target.Path() != "owner-27/wid gets" {
		t.Fatalf("the loaded target is %+v, not the repository the file declares", target)
	}

	if _, err := github.VerifyAppAt(t.Context(), srv.Client(), srv.URL,
		cfg.GitHub.AppID, boundaryAppKey(), github.RepositoryTarget(target.Owner(), target.RepositoryName()),
		cfg.GitHub.InstallationID); err != nil {
		t.Fatalf("VerifyAppAt: %v", err)
	}

	if got != "/repos/owner-27/wid%20gets/installation" {
		t.Errorf("GitHub was asked %q, but the config names a repository", got)
	}
}
