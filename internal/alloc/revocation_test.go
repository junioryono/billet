package alloc

import (
	"errors"
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

// A REVOKED CERTIFICATE CANNOT RENEW ITS WAY OUT.
//
// Revocation checks the presented certificate at the start of a request and the
// signing happens milliseconds later, so a revocation committing in between took
// back a credential the machine had already stopped presenting — and reported
// success. Recording the child in the same transaction that asks about the
// parent makes the order decide.
func TestARenewalIsRefusedWhenItsParentHasBeenRevoked(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	if err := a.RecordIssuedCert(t.Context(),
		IssuedCert{Serial: "parent", Node: "epyc-1", Source: CertIssued, NotAfter: future}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The operator takes the machine's credentials back.
	if _, err := a.RevokeNode(t.Context(), "epyc-1", "compromised"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	// The renewal that was already in flight lands afterwards.
	err := a.RecordRenewedCert(t.Context(),
		IssuedCert{Serial: "child", Node: "epyc-1", Source: CertRenewed, NotAfter: future}, "parent", now)
	if !errors.Is(err, ErrParentRevoked) {
		t.Fatalf("a revoked certificate renewed itself into a credential nobody revoked: %v", err)
	}

	// And nothing was written, so the machine holds only what was taken back.
	live, err := a.LiveCertsFor(t.Context(), "epyc-1")
	if err != nil {
		t.Fatalf("LiveCertsFor: %v", err)
	}

	if len(live) != 0 {
		t.Errorf("epyc-1 still holds %d live certificate(s) after being revoked: %+v", len(live), live)
	}
}

// AND AN ORDINARY RENEWAL STILL WORKS, which is the direction that must not be
// broken by the check above.
func TestARenewalIsRecordedWhenItsParentIsGood(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	if err := a.RecordIssuedCert(t.Context(),
		IssuedCert{Serial: "parent", Node: "epyc-1", Source: CertIssued, NotAfter: future}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := a.RecordRenewedCert(t.Context(),
		IssuedCert{Serial: "child", Node: "epyc-1", Source: CertRenewed, NotAfter: future},
		"parent", now); err != nil {
		t.Fatalf("an ordinary renewal was refused: %v", err)
	}

	live, err := a.LiveCertsFor(t.Context(), "epyc-1")
	if err != nil {
		t.Fatalf("LiveCertsFor: %v", err)
	}

	if len(live) != 2 {
		t.Errorf("epyc-1 holds %d certificates, want the original and its renewal", len(live))
	}
}

// REVOKING A NODE REACHES CREDENTIALS BILLET NEVER RECORDED.
//
// Revocation by serial reaches only what was written down, and there are two
// ways for a working certificate to exist outside that set: a deployment
// upgraded from a version that did not record serials, and a name issued more
// than once before it did — the admission trail keeps one row per node and
// overwrites it, so the earlier certificate is unrecoverable by any backfill.
//
// A cutoff needs no list. Revoking records the moment, and any certificate for
// that name valid from before it is refused on sight.
func TestRevokingANodeRefusesCredentialsItNeverRecorded(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	// A certificate from before billet tracked serials: nothing recorded it, so
	// nothing can name it.
	unknown := now.Add(-30 * 24 * time.Hour)

	revoked, err := a.CertRevokedFor(t.Context(), "epyc-1", "never-seen", unknown)
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if revoked {
		t.Fatal("a certificate was refused before its node was revoked")
	}

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "stolen"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	revoked, err = a.CertRevokedFor(t.Context(), "epyc-1", "never-seen", unknown)
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if !revoked {
		t.Error("a certificate this deployment never recorded is still accepted after its " +
			"node was revoked; an upgraded deployment cannot take back what it cannot name")
	}

	// ANOTHER NODE IS UNTOUCHED: this is a revocation, not a purge.
	other, err := a.CertRevokedFor(t.Context(), "mac-mini-1", "never-seen", unknown)
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if other {
		t.Error("revoking epyc-1 also refused a certificate belonging to mac-mini-1")
	}
}

// A SECOND REVOCATION MOVES THE CUTOFF FORWARD, and the first one must not hold
// it back.
//
// The cutoff exists for credentials billet never recorded, so a stale one leaves
// exactly the class it was written for: revoke a node, let a replacement be
// issued -- which the cutoff correctly permits -- and revoke it again after that
// machine is compromised. If the upsert kept the first cutoff, every unrecorded
// certificate minted between the two revocations is still accepted, and the
// second revocation reports success over a credential it did not reach.
//
// Measured: an ON CONFLICT that kept node_revocations.revoked_before instead of
// taking excluded.revoked_before left every other test in this package green.
func TestASecondRevocationMovesTheCutoffForward(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "stolen"); err != nil {
		t.Fatalf("first RevokeNode: %v", err)
	}

	// A REPLACEMENT THE FIRST CUTOFF PERMITS, minted an hour later and never
	// recorded -- which is the only kind the cutoff can reach.
	replacement := now.Add(time.Hour)

	revoked, err := a.CertRevokedFor(t.Context(), "epyc-1", "never-seen", replacement)
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if revoked {
		t.Fatal("the replacement was refused by the first revocation, so this test cannot " +
			"observe whether the second one reaches it")
	}

	a.now = func() time.Time { return replacement.Add(time.Hour) }

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "stolen again"); err != nil {
		t.Fatalf("second RevokeNode: %v", err)
	}

	revoked, err = a.CertRevokedFor(t.Context(), "epyc-1", "never-seen", replacement)
	if err != nil {
		t.Fatalf("CertRevokedFor after the second revocation: %v", err)
	}

	if !revoked {
		t.Error("the second revocation left the first cutoff in place, so every unrecorded " +
			"certificate minted between them is still accepted")
	}
}

// AND A REPLACEMENT ISSUED AFTERWARDS STILL WORKS, which is what keeps the
// cutoff a revocation rather than a ban on the name.
//
// THE DATES HERE ARE THE ONES A REAL CERTIFICATE CARRIES, and that is the whole
// point. Every certificate billet issues is valid from an HOUR BEFORE it was
// minted, so a node whose clock is behind the control plane's does not reject
// what it was just handed. A replacement issued a minute after a revocation
// therefore has a NotBefore an hour BEFORE the cutoff — and reading NotBefore as
// the issuance moment refused it, turning a revocation into a permanent ban on
// the node name.
//
// The first version of this test chose a replacement date an hour in the FUTURE,
// which no certificate ever has, and passed against exactly that bug.
func TestACertificateIssuedAfterTheCutoffIsAccepted(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "rebuilt"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	// The machine is rebuilt a minute later, so its certificate is MINTED then —
	// and carries a NotBefore 59 minutes before the revocation.
	minted := now.Add(time.Minute)

	revoked, err := a.CertRevokedFor(t.Context(), "epyc-1", "fresh", minted)
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if revoked {
		t.Error("the replacement machine's certificate was born revoked, so a node name can " +
			"never be reused after a revocation")
	}

	// And one minted a minute BEFORE the revocation is still refused, which is
	// the direction the cutoff exists for.
	revoked, err = a.CertRevokedFor(t.Context(), "epyc-1", "old", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("CertRevokedFor: %v", err)
	}

	if !revoked {
		t.Error("a certificate minted before the revocation is still accepted")
	}
}

// THE CUTOFF ALONE STOPS A RENEWAL, with no serial involved.
//
// This is the case the cutoff exists for and the one the other test cannot
// isolate: RevokeNode both revokes the recorded serials AND installs the cutoff,
// so a parent that was recorded is stopped by the serial check whether or not
// the cutoff works at all. Removing the cutoff query leaves that test green.
//
// Here the parent was never recorded — the legacy credential the whole mechanism
// is for — so the serial check has nothing to match and only the cutoff can
// refuse it.
func TestAnUnrecordedParentCannotRenewPastTheCutoff(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	// Nothing recorded this parent: it predates billet tracking serials.
	if _, err := a.RevokeNode(t.Context(), "epyc-1", "compromised"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	err := a.RecordRenewedCert(t.Context(),
		IssuedCert{Serial: "child", Node: "epyc-1", Source: CertRenewed, NotAfter: future},
		"never-recorded", now.Add(-time.Minute))
	if !errors.Is(err, ErrParentRevoked) {
		t.Fatalf("a certificate this deployment never recorded renewed itself past its node's "+
			"revocation: %v", err)
	}

	// AND THE CHILD WAS NOT WRITTEN, so nothing is left holding a credential the
	// refusal was supposed to prevent.
	live, err := a.LiveCertsFor(t.Context(), "epyc-1")
	if err != nil {
		t.Fatalf("LiveCertsFor: %v", err)
	}

	for i := range live {
		if live[i].Serial == "child" {
			t.Error("the refused renewal was recorded anyway")
		}
	}
}

// AND THE BOUNDARY IS WHERE THE COMMENT SAYS IT IS. A parent minted in the
// revocation's own second is refused: a certificate cannot express anything
// finer, so that second is ambiguous, and the ambiguity resolves toward
// refusing. One minted in the next second renews normally.
func TestTheRenewalCutoffBoundaryIsAWholeSecond(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil,
		WithClock(func() time.Time { return now }))

	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)

	if _, err := a.RevokeNode(t.Context(), "epyc-1", "compromised"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}

	sameSecond := a.RecordRenewedCert(t.Context(),
		IssuedCert{Serial: "same", Node: "epyc-1", Source: CertRenewed, NotAfter: future},
		"unrecorded", now)
	if !errors.Is(sameSecond, ErrParentRevoked) {
		t.Errorf("a parent minted in the revocation's own second was allowed to renew; that "+
			"second cannot be told apart from the one before it: %v", sameSecond)
	}

	next := a.RecordRenewedCert(t.Context(),
		IssuedCert{Serial: "next", Node: "epyc-1", Source: CertRenewed, NotAfter: future},
		"reissued", now.Add(time.Second))
	if next != nil {
		t.Errorf("a certificate issued the second AFTER a revocation could not renew, so the "+
			"cutoff is a ban on the name rather than a revocation: %v", next)
	}
}
