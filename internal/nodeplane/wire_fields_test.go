package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// WHAT THE NODE SAID HAS TO REACH THE LEDGER, and nothing was proving it did.
//
// The registration path is node config → HTTP body → plane → registrar, and the
// only thing asserted across it was the node's NAME. A serialiser that dropped
// the capacity, a plane that forgot to copy a field into the registration, or a
// json tag that never matched would all have passed — and the failure is silent
// downstream: a host recorded as contributing nothing joins the fleet, is never
// chosen, and produces no error anywhere.
func TestWhatTheNodeReportsArrivesAtTheLedger(t *testing.T) {
	t.Parallel()

	const (
		wantVCPU   = 96
		wantMemory = 384 * config.GiB
		wantSite   = "home"
	)

	reg := &fakeRegistrar{}
	wantShapes := []config.EC2InstanceType{{
		Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
		PriceUSDPerHour: 340_000,
	}}

	_, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg),
		nodeplane.WithSites([]config.SiteConfig{{Name: wantSite, Store: config.SiteStoreEBSS3}}))

	c := dial(t, base)

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider:   config.ProviderEC2,
		Deployment: deployment,
		Site:       wantSite,
		VCPU:       wantVCPU,
		Memory:     wantMemory,
		EC2Shapes:  wantShapes,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	got := reg.lastRegistration()

	if got.VCPU != wantVCPU {
		t.Errorf("vcpu reached the ledger as %d, want %d", got.VCPU, wantVCPU)
	}

	if got.Memory != wantMemory {
		t.Errorf("memory reached the ledger as %s, want %s", got.Memory, wantMemory)
	}

	if got.Site != wantSite {
		t.Errorf("site reached the ledger as %q, want %q", got.Site, wantSite)
	}

	if got.Provider != config.ProviderEC2 {
		t.Errorf("provider reached the ledger as %q, want %q", got.Provider, config.ProviderEC2)
	}

	if len(got.EC2Shapes) != 1 || got.EC2Shapes[0] != wantShapes[0] {
		t.Errorf("EC2 shapes reached the ledger as %+v, want %+v", got.EC2Shapes, wantShapes)
	}

	// THE ROLLOUT FACTS TRAVEL THE SAME PATH AND ARE SILENT IN THE SAME WAY. A
	// serialiser that dropped them, or a plane that recorded its own preference
	// instead of what was negotiated, produces a `billet status` that reports the
	// whole fleet converged while it is not — and an operator retires a protocol
	// their nodes still need.
	if got.WireVersion != nodeapi.Version {
		t.Errorf("the negotiated protocol reached the ledger as %d, want %d",
			got.WireVersion, nodeapi.Version)
	}

	if got.WireMin != nodeapi.MinVersion || got.WireMax != nodeapi.Version {
		t.Errorf("the node's range reached the ledger as %d-%d, want %s",
			got.WireMin, got.WireMax, nodeapi.Self())
	}

	// NOT COMPARED AGAINST A LITERAL. The node fills this from its own build, and
	// a test naming a version string would only assert what it had just been told;
	// what must hold is that SOMETHING crossed, since an empty value here is
	// exactly what a dropped field looks like.
	if got.Release == "" {
		t.Error("the node's release never reached the ledger, so `billet status` cannot " +
			"name which build a host is running")
	}
}

// THE HTTP ENTRY POINT REFUSES BEFORE IT MUTATES TOO, and it has its OWN
// beginRegistration call.
//
// Fixing the in-process path and leaving this one is the shape of bug this
// package keeps producing: two entry points, one of them corrected. The
// mutation that matters is the inventory — beginRegistration clears it, only a
// SUCCESSFUL registration restores it, and a completion may settle a lease from
// absence only while it is known. So a request billet REJECTS could leave a live
// host's capacity charged for compute that is provably gone.
func TestARefusedRegistrationOverTheWireLeavesTheInventoryAlone(t *testing.T) {
	t.Parallel()

	p, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(&fakeRegistrar{}))

	c := dial(t, base)

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider:       config.ProviderDocker,
		Deployment:     deployment,
		VCPU:           testNodeVCPU,
		Memory:         testNodeMemory,
		Instances:      []string{"l1"},
		InventoryKnown: true,
	}); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	if !p.ReconciledForTest("n1") {
		t.Fatal("the seed registration did not vouch for its inventory, so this test cannot " +
			"observe a refusal clearing it")
	}

	// A DECLARATION NO REAL BUILD SENDS, posted as raw JSON because the client
	// cannot be made to produce one — which is exactly why the server has to
	// refuse it rather than trusting its peer to be billet.
	body := fmt.Sprintf(
		`{"version":%d,"min_version":%d,"release":"v1","node":"n1","provider":"docker",`+
			`"deployment":%q,"vcpu":8,"memory":34359738368,"incarnation":"impostor"}`,
		nodeapi.MinVersion, nodeapi.Version+1, deployment)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/register", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build a malformed registration: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post a malformed registration: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a declaration that is not a range answered %s, want a permanent refusal",
			resp.Status)
	}

	if !p.ReconciledForTest("n1") {
		t.Error("a registration refused over the wire cleared the live host's inventory; " +
			"until it registers again no completion can settle a lease from absence")
	}
}

// A PAIRING THAT HAS NO DIGEST FIELD CANNOT PUT ONE IN THE LEDGER.
//
// The field exists from wire 16. Below that the protocol this registration
// settled on has no such field, so a value in it is not something that pairing
// can mean — and taking it anyway lets a caller declaring wire 12 supply evidence
// a rollout treats as PROOF, or as grounds to BLOCK a host. Neither is an outcome
// that protocol can ask for.
//
// DRIVEN THROUGH REGISTRATION, NOT THROUGH THE HELPER THAT DECIDES IT. Asserting
// on negotiatedDigest alone proves the comparison and says nothing about whether
// the registration path consults it — and a version of this that persisted the
// field unchanged would pass such a test while doing the thing it forbids. The
// body is hand-built because no billet client will send an old version with a new
// field, which is precisely the shape being defended against.
func TestADigestFromAnOlderPairingNeverReachesTheLedger(t *testing.T) {
	t.Parallel()

	claimed := strings.Repeat("a", 64)

	for _, tc := range []struct {
		name    string
		version int
		want    string
	}{
		{"a pairing older than the field", nodeapi.VersionNodeDigest - 1, ""},
		{"the pairing that introduced it", nodeapi.VersionNodeDigest, claimed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := &fakeRegistrar{}
			_, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg))

			body, err := json.Marshal(nodeapi.RegisterRequest{
				Version:         tc.version,
				MinVersion:      nodeapi.MinVersion,
				Release:         "v0.4.0",
				InstalledDigest: claimed,
				Node:            "n1",
				Provider:        config.ProviderDocker,
				Deployment:      deployment,
				VCPU:            8,
				Memory:          32 * config.GiB,
			})
			if err != nil {
				t.Fatalf("render the registration: %v", err)
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				base+"/v1/register", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}

			req.Header.Set("Content-Type", "application/json")

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("register: %v", err)
			}

			defer func() { _ = res.Body.Close() }()

			if res.StatusCode != http.StatusOK {
				t.Fatalf("the registration was refused with %d", res.StatusCode)
			}

			if got := reg.lastRegistration().Digest; got != tc.want {
				t.Errorf("a registration declaring wire %d put digest %q in the ledger, "+
					"want %q", tc.version, got, tc.want)
			}
		})
	}
}
