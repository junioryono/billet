package e2e

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// THE CONTROL PLANE'S images.accept REACHES A NODE: a control plane assembled
// the way `billet server` assembles it, from a config accepting two components,
// answers a registration with them, so a license accepted once there completes
// every node's generations (#584).
func TestTheControlPlanesImageAcceptanceReachesANode(t *testing.T) {
	p := newPlane(t)
	dir := t.TempDir()
	ledgertest.Seed(t, dir)

	client, err := scaleset.New(scaleset.Config{
		Target:         billetgithub.OrganizationTarget("acme"),
		GitHubURL:      p.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     p.PrivateKeyPEM(),
		AppID:          12345,
		APIURL:         p.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	cfg := harnessConfig(dir, 8, 16*config.GiB, []config.Tier{{
		Label:       testTier,
		Provider:    config.ProviderDocker,
		VCPU:        2,
		Memory:      2 * config.GiB,
		Image:       testImage,
		RunnerGroup: testGroup,
		GuestOS:     config.GuestLinux,
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
	}})
	cfg.Images = &config.ImagesConfig{Accept: []string{"google-chrome", "android-sdk"}}

	c := openAssembled(t, cfg,
		[]app.Target{{Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"}, Client: client}},
		&app.Steering{Owner: harnessOwner})

	nc, err := app.NewNodeClient(&config.Config{Node: &config.NodeConfig{
		Name: "e2e-host", ServerAddr: "http://" + c.wire.Addr,
	}}, nil)
	if err != nil {
		t.Fatalf("app.NewNodeClient: %v", err)
	}

	if err := nc.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker,
		GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: c.ctl.Deployment(),
		VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	accepted, known := nc.ImageAcceptance()
	if want := []string{"android-sdk", "google-chrome"}; !known || !slices.Equal(accepted, want) {
		t.Fatalf("the registration's answer accepts %v (carried %v), want %v", accepted, known, want)
	}
}
