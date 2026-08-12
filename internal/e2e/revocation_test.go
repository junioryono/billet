package e2e

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// revocations is an in-memory revocation list.
type revocations struct {
	mu      sync.Mutex
	serials map[string]bool
	err     error
}

func (r *revocations) CertRevoked(_ context.Context, serial string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return false, r.err
	}

	return r.serials[serial], nil
}

func (r *revocations) revoke(serial string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.serials == nil {
		r.serials = map[string]bool{}
	}

	r.serials[serial] = true
}

func (r *revocations) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.err = err
}

// revocableWire is an mTLS wire with a revocation list and a renewal signer.
func revocableWire(t *testing.T) (*wirecert.CA, string, *revocations) {
	t.Helper()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	list := &revocations{}

	plane := nodeplane.New(log, wireDeployment, time.Minute,
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))
	plane.SetPollWindowForTest(200 * time.Millisecond)

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, mtlsStore{}, alwaysMints{},
			nodeplane.RequireClientCert(),
			nodeplane.WithRevocations(list),
			nodeplane.WithRenewal(ca)))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return ca, srv.URL, list
}

// A REVOKED CERTIFICATE STOPS WORKING AT ONCE, not at its expiry.
//
// The wire's whole admission decision is that an operator issued this host a
// certificate, and it lasts a year. Without revocation a decommissioned machine
// — or one whose key leaked — could rejoin and be handed work, including a JIT
// credential that registers a runner against the organisation. The only remedy
// was rotating the CA, which invalidates every node at once.
//
// CHECKED ON EVERY REQUEST, not only at registration: a node holds one long poll
// open for the better part of a minute and re-registers rarely, so a check only
// at registration would leave a revoked host working until it happened to
// restart.
func TestARevokedCertificateIsRefused(t *testing.T) {
	t.Parallel()

	ca, base, list := revocableWire(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: wireDeployment,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("a freshly issued certificate was refused: %v", err)
	}

	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatalf("read the leaf: %v", err)
	}

	list.revoke(wirecert.Serial(leaf))

	// The SAME connection and the same certificate, now withdrawn.
	err = c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: wireDeployment,
		VCPU: 8, Memory: 32 * config.GiB,
	})
	if err == nil {
		t.Fatal("a revoked certificate still registered; the host an operator just " +
			"decommissioned can be handed work and a JIT credential")
	}

	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("the refusal does not say the certificate was revoked: %v", err)
	}
}

// AN UNREADABLE REVOCATION LIST REFUSES, rather than assuming nothing is
// revoked.
//
// Answering "not revoked" on a failed read makes a transient database fault
// equivalent to switching the check off — and it would do so silently, at
// exactly the moment somebody is relying on a revocation having taken effect.
func TestAnUnreadableRevocationListFailsClosed(t *testing.T) {
	t.Parallel()

	ca, base, list := revocableWire(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	list.fail(context.DeadlineExceeded)

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: wireDeployment,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err == nil {
		t.Fatal("a node was admitted while billet could not read its revocation list")
	}
}

// A NODE RENEWS ITSELF, and the renewal is usable.
//
// Without this a fleet enrolled on one afternoon expires on one afternoon a
// year later, all at once, and cannot recover on its own: renewal is
// authenticated by the certificate being renewed, so an expired node has to be
// re-enrolled by hand.
func TestANodeRenewsItsOwnCertificate(t *testing.T) {
	t.Parallel()

	ca, base, _ := revocableWire(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	certPEM, keyPEM, caPEM, err := c.Renew(t.Context(), "epyc-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	// USABLE, which is the only thing that matters: a renewal that produces a
	// certificate the control plane would reject is worse than none, because the
	// node overwrites a working identity with it.
	// THE AUTHORITY COMES BACK WITH IT, which is what lets a CA rotation reach a
	// node: during an overlap this is a bundle holding both.
	if len(caPEM) == 0 {
		t.Fatal("the renewal carried no authority, so a rotation could never reach this node")
	}

	renewed, err := wirecert.ClientTLS(wirecert.Bundle{
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM,
	})
	if err != nil {
		t.Fatalf("the renewed certificate does not verify against its own authority: %v", err)
	}

	c2, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: renewed})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c2.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderDocker, Deployment: wireDeployment,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("the renewed certificate was refused by the control plane: %v", err)
	}
}

// RENEWAL CANNOT RENAME. The subject in a CSR is whatever the requester typed;
// the authenticated identity is what the wire proved. Signing the former would
// let any node with a valid certificate mint one for any name it liked — every
// node able to impersonate every other, through the endpoint meant to keep them
// working.
func TestRenewalCannotMintAnotherNodesName(t *testing.T) {
	t.Parallel()

	ca, base, _ := revocableWire(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Asks for a certificate naming somebody else.
	certPEM, _, _, err := c.Renew(t.Context(), "mac-mini-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	got, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: certPEM})
	if err != nil {
		t.Fatalf("parse the signed certificate: %v", err)
	}

	if got.Subject.CommonName != "epyc-1" {
		t.Errorf("the control plane signed a certificate for %q on a connection authenticated "+
			"as %q; every node could then act as every other",
			got.Subject.CommonName, "epyc-1")
	}
}
