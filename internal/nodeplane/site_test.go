package nodeplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// THE CONTROL PLANE IS THE AUTHORITY ON WHICH PLACES EXIST, and this is the only
// point where that can be enforced.
//
// A site is declared in the CONTROL PLANE's config; a node names one in ITS OWN
// config, on a different machine, in a file that has no reason to list the
// deployment's places. So the node cannot check itself — it does not know the
// answer — and validating the server's file only ever guards the server's own
// tiers. Every remote node's claim arrives here.
//
// What makes it worth refusing rather than accepting: a typo becomes a site of
// one machine, with a cache of its own that is always empty, and every job
// placed there runs cold while the fleet looks healthy. Nothing downstream can
// tell that apart from a genuine second location.
func TestANodeCannotRegisterIntoAnUndeclaredSite(t *testing.T) {
	t.Parallel()

	p := testPlaneWithSites(t, "home")

	_, err := p.Register(t.Context(), registerAt("hom"))
	if err == nil {
		t.Fatal("a node registered into a site the control plane never declared")
	}

	// PERMANENT, NOT AN OUTAGE. A node retries anything that might heal, and this
	// will not: the same node with the same config will be refused forever, so a
	// node that kept retrying would hammer the control plane and never say why.
	if !errors.Is(err, ErrRefused) {
		t.Errorf("an undeclared site is not a permanent refusal: %v", err)
	}

	if !strings.Contains(err.Error(), "hom") {
		t.Errorf("the refusal does not name the site that was claimed: %v", err)
	}
}

// A node that names a declared site is the ordinary case.
func TestANodeMayRegisterIntoADeclaredSite(t *testing.T) {
	t.Parallel()

	p := testPlaneWithSites(t, "home")

	if _, err := p.Register(t.Context(), registerAt("home")); err != nil {
		t.Fatalf("a node at a declared site was refused: %v", err)
	}
}

// SITES ARE OPTIONAL AND STAY OPTIONAL. A deployment with one place never writes
// the block, and every node in it is unsited — which must go on working exactly
// as it did before there was such a thing as a site.
func TestAnUnsitedNodeIsAcceptedWhereverSitesAreDeclared(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		sites []string
	}{
		{name: "no sites declared", sites: nil},
		{name: "sites declared, node names none", sites: []string{"home"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := testPlaneWithSites(t, tc.sites...)

			if _, err := p.Register(t.Context(), registerAt("")); err != nil {
				t.Fatalf("an unsited node was refused: %v", err)
			}
		})
	}
}

// A node claiming a site when the deployment declares none is the typo case
// again, from the other side: there is nothing it could correctly mean.
func TestANodeCannotClaimASiteWhereNoneAreDeclared(t *testing.T) {
	t.Parallel()

	p := testPlaneWithSites(t)

	_, err := p.Register(t.Context(), registerAt("home"))
	if err == nil {
		t.Fatal("a node claimed a site in a deployment that declares none")
	}

	if !errors.Is(err, ErrRefused) {
		t.Errorf("want a permanent refusal: %v", err)
	}
}

// testPlaneWithSites is a plane whose deployment declares these places.
func testPlaneWithSites(t *testing.T, sites ...string) *Plane {
	t.Helper()

	return testPlane(t, WithSites(sites))
}

// registerAt is a valid registration for the test deployment, at one site.
func registerAt(site string) nodeapi.RegisterRequest {
	return nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		Incarnation: "00000000000000000000000000000001",
		Site:        site,
		VCPU:        8,
		Memory:      32 * config.GiB,
	}
}
