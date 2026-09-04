package wirecert

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
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

	bundle, err := ca.IssueNodeFor("epyc-1", MinIssuedLifetime)
	if err != nil {
		t.Fatalf("IssueNodeFor at the floor: %v", err)
	}

	cert := leafOf(t, bundle)

	want := before.Add(MinIssuedLifetime)
	if cert.NotAfter.Before(want.Add(-10*time.Second)) || cert.NotAfter.After(want.Add(10*time.Second)) {
		t.Errorf("NotAfter is %s, want about %s", cert.NotAfter, want)
	}

	// A FRESH SHORT LEAF IS NOT DUE. The hour of backdating is not life the
	// certificate has, and counting it made a short leaf due the moment it was
	// issued, so the node renewed onto the old authority before the rotation a
	// rehearsal was about to make.
	if left, due := RenewalDue(cert); due {
		t.Errorf("a freshly issued %s leaf is already due for renewal with %s left", MinIssuedLifetime, left)
	}

	if _, err := ca.IssueNodeFor("epyc-1", LeafLifetime); err != nil {
		t.Errorf("IssueNodeFor at the ceiling: %v", err)
	}

	// THE BOUNDS ARE THE AUTHORITY'S, not only the command's: an exported entry
	// point that trusts its caller is a second place the rule can be missing.
	for _, lifetime := range []time.Duration{
		MinIssuedLifetime - time.Second, LeafLifetime + time.Second, 0, -time.Minute,
	} {
		_, err := ca.IssueNodeFor("epyc-1", lifetime)
		if err == nil {
			t.Errorf("a lifetime of %s was accepted", lifetime)

			continue
		}

		if !strings.Contains(err.Error(), "outside") || !strings.Contains(err.Error(), MinIssuedLifetime.String()) {
			t.Errorf("the refusal of %s does not name the bound: %v", lifetime, err)
		}
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

// RenewalDue measures a leaf from its issue, not from the backdated NotBefore.
func TestRenewalDueIgnoresTheBackdatedHour(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// A twenty-minute leaf issued a minute ago, exactly as leafTemplate shapes it:
	// NotBefore an hour before issue.
	fresh := &x509.Certificate{
		NotBefore: now.Add(-time.Minute - ClockSkew),
		NotAfter:  now.Add(19 * time.Minute),
	}
	if _, due := RenewalDue(fresh); due {
		t.Error("a leaf issued a minute ago with nineteen minutes left is due; the backdated hour is being counted as life")
	}

	// The same leaf with five minutes left: inside its final third.
	late := &x509.Certificate{
		NotBefore: now.Add(-15*time.Minute - ClockSkew),
		NotAfter:  now.Add(5 * time.Minute),
	}
	if _, due := RenewalDue(late); !due {
		t.Error("a twenty-minute leaf with five minutes left is not due; renewal would be missed")
	}

	// A certificate nobody backdated is measured from its NotBefore as before.
	foreign := &x509.Certificate{
		NotBefore: now.Add(-10 * time.Minute),
		NotAfter:  now.Add(20 * time.Minute),
	}
	if _, due := RenewalDue(foreign); due {
		t.Error("a thirty-minute leaf with twenty left is due")
	}
}
