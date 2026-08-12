package wirecert_test

import (
	"crypto/x509"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

const rotDeployment = "0123456789abcdef0123456789abcdef"

// A ROTATION IS AN OVERLAP, AND BOTH KINDS OF NODE KEEP WORKING THROUGH IT.
//
// This is the whole property. A node trusts the authority it was given, so the
// moment the control plane presents a certificate from a new one, every node
// that has not yet heard about it fails to verify the server and drops out —
// over the wire it would need in order to recover. There is no way back from
// that remotely.
//
// So during the overlap: the OLD authority signs what the server presents, the
// NEW one issues node certificates, and both are trusted for clients.
func TestARotationKeepsBothGenerationsWorking(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A node enrolled before the rotation.
	before, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	fresh, err := wirecert.Rotate(dir, rotDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if fresh.Fingerprint() == old.Fingerprint() {
		t.Fatal("rotating produced the same authority")
	}

	// A node enrolled after it.
	after, err := fresh.IssueNode("epyc-2")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	trust, err := wirecert.TrustBundle(dir, fresh)
	if err != nil {
		t.Fatalf("trust bundle: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		t.Fatal("the trust bundle could not be parsed")
	}

	// BOTH ARE RECOGNISED AS CLIENTS. This is what stops the pre-rotation fleet
	// being locked out the moment the new authority starts issuing.
	for name, bundle := range map[string]wirecert.Bundle{"before": before, "after": after} {
		leaf, lerr := wirecert.LeafOf(bundle)
		if lerr != nil {
			t.Fatalf("%s: parse: %v", name, lerr)
		}

		if _, verr := leaf.Verify(x509.VerifyOptions{
			Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); verr != nil {
			t.Errorf("a node enrolled %s the rotation is not recognised during the overlap: %v",
				name, verr)
		}
	}

	// AND THE SERVER STILL PRESENTS SOMETHING THE OLD FLEET TRUSTS. A node that
	// has not renewed knows only the old authority, so a serving certificate from
	// the new one would make the control plane unverifiable to it.
	serving, err := wirecert.ServingCA(dir, rotDeployment, fresh)
	if err != nil {
		t.Fatalf("serving ca: %v", err)
	}

	if serving.Fingerprint() != old.Fingerprint() {
		t.Errorf("during the overlap the server signs with %s; it must be the authority the "+
			"un-renewed fleet still trusts, %s", serving.Fingerprint(), old.Fingerprint())
	}

	oldOnly := x509.NewCertPool()
	if !oldOnly.AppendCertsFromPEM(old.CertPEM()) {
		t.Fatal("parse the old authority")
	}

	servingBundle, err := serving.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue serving: %v", err)
	}

	servingLeaf, err := wirecert.LeafOf(servingBundle)
	if err != nil {
		t.Fatalf("parse serving: %v", err)
	}

	if _, err := servingLeaf.Verify(x509.VerifyOptions{
		Roots: oldOnly, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("a node that has not renewed cannot verify the control plane during the "+
			"overlap, so it drops out over the wire it would need to recover: %v", err)
	}
}

// RETIRING ENDS IT, and only then does the old authority stop being trusted.
func TestRetiringDropsTheOldAuthority(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh, err := wirecert.Rotate(dir, rotDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if err := wirecert.Retire(dir); err != nil {
		t.Fatalf("retire: %v", err)
	}

	trust, err := wirecert.TrustBundle(dir, fresh)
	if err != nil {
		t.Fatalf("trust: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		t.Fatal("parse")
	}

	before, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	leaf, err := wirecert.LeafOf(before)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Error("a certificate from the retired authority is still accepted; retiring did nothing")
	}

	// And the server now presents the new one.
	serving, err := wirecert.ServingCA(dir, rotDeployment, fresh)
	if err != nil {
		t.Fatalf("serving: %v", err)
	}

	if serving.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("after retiring, the server still signs with %s", serving.Fingerprint())
	}
}

// A SECOND ROTATION WHILE ONE IS RUNNING IS REFUSED. There is one previous
// authority, so starting another would drop the one the un-renewed fleet still
// trusts — a rotation that locks out exactly the nodes it was meant to carry.
func TestASecondRotationIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := wirecert.Rotate(dir, rotDeployment); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := wirecert.Rotate(dir, rotDeployment); err == nil {
		t.Fatal("a second rotation started while the first was still running")
	}
}
