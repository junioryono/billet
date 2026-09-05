package wiring

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
)

// clientFor builds a real scale-set client for a target, so the assembly's
// check that a client serves the target its config names is exercised against
// what the constructor records rather than a zero value.
func clientFor(t *testing.T, target billetgithub.Target) *scaleset.Client {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	c, err := scaleset.New(scaleset.Config{
		Target:         target,
		ClientID:       "1",
		InstallationID: 1,
		AppID:          1,
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		})),
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	return c
}

// BuildTargets keys both views by the target's config name and gives each the
// client that holds that target's credential, so a tier resolves to one
// credential whichever half of the control plane asks.
func TestBuildTargetsKeysBothViewsByTargetName(t *testing.T) {
	t.Parallel()

	a := clientFor(t, billetgithub.OrganizationTarget("acme"))
	b := clientFor(t, billetgithub.RepositoryTarget("someone", "widgets"))

	servers, jit, err := BuildTargets([]Target{
		{Config: config.GitHubTarget{Name: "default", Org: "acme"}, Client: a},
		{Config: config.GitHubTarget{Name: "personal", Repository: "someone/widgets"}, Client: b},
	})
	if err != nil {
		t.Fatalf("BuildTargets: %v", err)
	}

	if len(servers) != 2 || servers[0].Config.Name != "default" || servers[1].Config.Name != "personal" {
		t.Fatalf("server targets = %+v", servers)
	}

	// The provisioner wraps exactly the target's client, not the other's.
	if prov, ok := servers[1].Provisioner.(Provisioner); !ok || prov.Client != b {
		t.Errorf("the repository target's provisioner does not hold its own client: %+v", servers[1].Provisioner)
	}

	if prov, ok := servers[0].Provisioner.(Provisioner); !ok || prov.Client != a {
		t.Errorf("the organization target's provisioner does not hold its own client: %+v", servers[0].Provisioner)
	}

	if src, ok := jit["personal"].(NodeJIT); !ok || src.Client != b {
		t.Errorf("the plane's source for the repository target is %+v", jit["personal"])
	}

	if src, ok := jit["default"].(NodeJIT); !ok || src.Client != a {
		t.Errorf("the plane's source for the organization target is %+v", jit["default"])
	}

	if len(jit) != 2 {
		t.Errorf("the plane got %d sources, want 2", len(jit))
	}
}

// AN ASSEMBLY THAT COULD SPLIT ONE NAME ACROSS TWO CREDENTIALS IS REFUSED. The
// server keeps a slice and would resolve the first entry of a name, the plane
// keeps a map and would hold the last, so a duplicate would reconcile a tier
// through one App and mint its registrations through another; a client built
// for another owner than the one its config names is the same failure with
// one entry.
func TestBuildTargetsRefusesWhatWouldSplitACredential(t *testing.T) {
	t.Parallel()

	acme := clientFor(t, billetgithub.OrganizationTarget("acme"))
	beta := clientFor(t, billetgithub.OrganizationTarget("beta"))

	for name, tc := range map[string]struct {
		targets []Target
		want    string
	}{
		"two targets named alike": {
			targets: []Target{
				{Config: config.GitHubTarget{Name: "default", Org: "acme"}, Client: acme},
				{Config: config.GitHubTarget{Name: "default", Org: "beta"}, Client: beta},
			},
			want: `two targets named "default"`,
		},
		"a client on another owner": {
			targets: []Target{
				{Config: config.GitHubTarget{Name: "default", Org: "acme"}, Client: beta},
			},
			want: "is acme in the config and beta on its client",
		},
		"no name": {
			targets: []Target{{Config: config.GitHubTarget{Org: "acme"}, Client: acme}},
			want:    "no name",
		},
		"no client": {
			targets: []Target{{Config: config.GitHubTarget{Name: "default", Org: "acme"}}},
			want:    "has no client",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			servers, jit, err := BuildTargets(tc.targets)
			if err == nil {
				t.Fatalf("BuildTargets accepted %s: servers=%+v jit=%v", name, servers, jit)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason: %v (want %q)", err, tc.want)
			}

			if servers != nil || jit != nil {
				t.Errorf("a refused assembly still handed out views: %+v %v", servers, jit)
			}
		})
	}
}
