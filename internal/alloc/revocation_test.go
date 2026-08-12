package alloc

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// REVOKING A NODE TAKES BACK WHAT IT IS ACTUALLY HOLDING, which after a renewal
// is not the bundle in the operator's hands.
//
// `billet ca revoke` names one serial, read out of the file that was issued. A
// node renews itself, so months later that file describes a credential the
// machine stopped presenting long ago: revoking it succeeds, reports that the
// certificate will be refused on its next request, and changes nothing.
func TestRevokingANodeTakesBackEveryCredentialItHolds(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	for _, c := range []IssuedCert{
		{Serial: "aa", Node: "epyc-1", Source: CertIssued, NotAfter: future},
		{Serial: "bb", Node: "epyc-1", Source: CertRenewed, NotAfter: future},
		{Serial: "cc", Node: "mac-mini-1", Source: CertEnrolled, NotAfter: future},
	} {
		if err := a.RecordIssuedCert(t.Context(), c); err != nil {
			t.Fatalf("record %s: %v", c.Serial, err)
		}
	}

	revoked, err := a.RevokeNode(t.Context(), "epyc-1", "laptop stolen")
	if err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	if len(revoked) != 2 {
		t.Fatalf("revoked %d certificates for epyc-1, want both the issued one and the renewal",
			len(revoked))
	}

	for _, serial := range []string{"aa", "bb"} {
		gone, err := a.CertRevoked(t.Context(), serial)
		if err != nil {
			t.Fatalf("CertRevoked %s: %v", serial, err)
		}

		if !gone {
			t.Errorf("%s is still accepted after its node was revoked", serial)
		}
	}

	// ANOTHER MACHINE IS UNTOUCHED. This is a revocation, not a purge.
	other, err := a.CertRevoked(t.Context(), "cc")
	if err != nil {
		t.Fatalf("CertRevoked cc: %v", err)
	}

	if other {
		t.Error("revoking epyc-1 also revoked a certificate belonging to mac-mini-1")
	}
}

// A REPLACEMENT MACHINE UNDER THE SAME NAME STILL WORKS, which is the property
// that makes revoking by node safe rather than a ban on the name. It takes back
// the serials outstanding at that moment; one issued afterwards is not among
// them.
func TestACertificateIssuedAfterARevocationIsNotRevoked(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	if err := a.RecordIssuedCert(t.Context(),
		IssuedCert{Serial: "old", Node: "epyc-1", Source: CertIssued, NotAfter: future}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "rebuilt"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	// The machine is rebuilt and issued a fresh bundle under the same name.
	if err := a.RecordIssuedCert(t.Context(),
		IssuedCert{Serial: "new", Node: "epyc-1", Source: CertIssued, NotAfter: future}); err != nil {
		t.Fatalf("record the replacement: %v", err)
	}

	gone, err := a.CertRevoked(t.Context(), "new")
	if err != nil {
		t.Fatalf("CertRevoked: %v", err)
	}

	if gone {
		t.Error("the replacement machine's certificate was born revoked, so a node name can " +
			"never be reused after a revocation")
	}
}

// AN EXPIRED CREDENTIAL IS NOT SOMETHING TO TAKE BACK. It already refuses
// itself, and listing it would bury the ones that matter under a year of
// superseded renewals.
func TestRevokingANodeIgnoresExpiredCredentials(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	if err := a.RecordIssuedCert(t.Context(), IssuedCert{
		Serial: "stale", Node: "epyc-1", Source: CertRenewed,
		NotAfter: now.Add(-time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	revoked, err := a.RevokeNode(t.Context(), "epyc-1", "")
	if err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	if len(revoked) != 0 {
		t.Errorf("revoked %d expired certificate(s); an expired credential already refuses itself",
			len(revoked))
	}
}
