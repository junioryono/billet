package wirecert

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func leafOf(t *testing.T, b Bundle) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode(b.CertPEM)
	if block == nil {
		t.Fatal("the bundle's certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the leaf: %v", err)
	}

	return cert
}

// IssueNodeFor is what a rotation rehearsal issues twelve-minute leaves with. It
// must honour the lifetime it is given, refuse a lifetime that could not be one,
// and stay under the authority's own expiry like every other leaf.
func TestIssueNodeForHonoursTheLifetime(t *testing.T) {
	t.Parallel()

	ca, err := LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	before := time.Now()

	bundle, err := ca.IssueNodeFor("epyc-1", 12*time.Minute)
	if err != nil {
		t.Fatalf("IssueNodeFor: %v", err)
	}

	cert := leafOf(t, bundle)

	want := before.Add(12 * time.Minute)
	if cert.NotAfter.Before(want.Add(-10*time.Second)) || cert.NotAfter.After(want.Add(10*time.Second)) {
		t.Errorf("NotAfter is %s, want about %s", cert.NotAfter, want)
	}

	if _, err := ca.IssueNodeFor("epyc-1", 0); err == nil {
		t.Error("a zero lifetime was accepted")
	}

	if _, err := ca.IssueNodeFor("epyc-1", -time.Minute); err == nil {
		t.Error("a negative lifetime was accepted")
	}

	// THE DEFAULT IS UNCHANGED: IssueNode is IssueNodeFor with LeafLifetime.
	full, err := ca.IssueNode("epyc-2")
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}

	if life := time.Until(leafOf(t, full).NotAfter); life < LeafLifetime-time.Minute {
		t.Errorf("IssueNode issued a %s leaf, want LeafLifetime (%s)", life, LeafLifetime)
	}
}
