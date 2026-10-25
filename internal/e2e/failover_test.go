package e2e

import (
	"net"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/provider/simulated"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A NODE THAT NAMES EVERY CONTROLLER FOLLOWS THE DEPLOYMENT TO THE ONE SERVING.
// A control plane serves the node wire at one address and a node, naming that
// address and a second, registers there. The control plane stops; another,
// over the same ledger, serves at the second address, as a promoted standby
// does; and the same node process registers with it, its incarnation unchanged,
// without any address moving. The order is the assembly's own: the second
// serves the wire only once it is the controller.
func TestANodeFollowsTheDeploymentToTheControllerServingIt(t *testing.T) {
	dir := t.TempDir()
	ledgertest.Seed(t, dir)

	p := newPlane(t)

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

	targets := []app.Target{{Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"}, Client: client}}
	tiers := []config.Tier{{
		Label: testTier, Provider: config.ProviderSimulated, VCPU: 2, Memory: 2 * config.GiB,
		Disk: 10 * config.GiB, Image: "simulated", RunnerGroup: testGroup, GuestOS: config.GuestLinux,
		Trust: config.WorkloadTrusted, Workflows: []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
		Command: simulated.RunFor(simulatedRunsFor),
	}}
	steering := &app.Steering{Owner: harnessOwner}

	// THE SECOND CONTROLLER'S ADDRESS, known to the node before that controller
	// exists, as a standby's is.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve the second address: %v", err)
	}

	second := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the second address: %v", err)
	}

	first := openAssembled(t, harnessConfig(dir, 8, 16*config.GiB, tiers), targets, steering)
	deployment := first.ctl.Deployment()

	prov, err := simulated.New(deployment, simulated.OnHost(simulated.NewHost()))
	if err != nil {
		t.Fatalf("simulated.New: %v", err)
	}

	const host = "e2e-roaming"

	node := startNodeLoop(t, testLogger(t), "", nodeProcess{
		host: host, deployment: deployment, provider: prov, kind: config.ProviderSimulated,
		hurry: make(chan struct{}), controllers: []string{first.wire.Addr, second},
	})

	first.awaitRegistered(t, 1)

	if got := first.wire.Plane().CurrentIncarnationForTest(host); got != node.incarnation {
		t.Fatalf("the first controller holds incarnation %q, the node is %q", got, node.incarnation)
	}

	// THE FIRST GOES, and nothing listens at its address after.
	first.close()

	cfg := harnessConfig(dir, 8, 16*config.GiB, tiers)
	cfg.Server.Listen = second

	promoted := openAssembled(t, cfg, targets, steering)
	if promoted.wire.Addr != second {
		t.Fatalf("the second controller serves at %s, want the address the node was given (%s)",
			promoted.wire.Addr, second)
	}

	deadline := time.Now().Add(30 * time.Second)
	for promoted.wire.Plane().CurrentIncarnationForTest(host) != node.incarnation {
		if time.Now().After(deadline) {
			t.Fatalf("the node never registered with the controller at its second address")
		}

		time.Sleep(20 * time.Millisecond)
	}

	// AND POLLS IT, which is where its commands now come from: a registration
	// alone says the node reached it once, not that the node dials it.
	for promoted.wire.Plane().WaitersForTest(host) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the node registered with the second controller and never polled it")
		}

		time.Sleep(20 * time.Millisecond)
	}
}
