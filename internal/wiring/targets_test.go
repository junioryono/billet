package wiring

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/scaleset"
)

// BuildTargets keys both views by the target's config name and gives each the
// client that holds that target's credential, so a tier resolves to one
// credential whichever half of the control plane asks.
func TestBuildTargetsKeysBothViewsByTargetName(t *testing.T) {
	t.Parallel()

	a, b := &scaleset.Client{}, &scaleset.Client{}

	servers, jit := BuildTargets([]Target{
		{Config: config.GitHubTarget{Name: "default", Org: "acme"}, Client: a},
		{Config: config.GitHubTarget{Name: "personal", Repository: "someone/widgets"}, Client: b},
	})

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
