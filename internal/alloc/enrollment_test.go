package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// ISSUING OVER AN EXISTING CLAIM SAYS SO.
//
// The wire refuses a second key asking under a name the first one claimed, and
// `billet ca issue` does not — deliberately, because refusing would leave a name
// unusable after a machine is rebuilt, and an operator running this command is
// making a decision rather than stumbling into one.
//
// What it must not be is silent. The displaced key's certificate is still valid
// and still names that node, so the fingerprint an operator compared yesterday
// now describes a machine that is no longer the one billet means. Nothing else
// in the system would ever mention it.
func TestIssuingOverAnExistingEnrollmentReportsWhatItDisplaced(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	displaced, err := a.RecordIssued(t.Context(), "epyc-1", "SHA256:first", "cert-1")
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	if displaced != "" {
		t.Errorf("a first issue displaced %q", displaced)
	}

	// The same machine, re-issued: nothing was taken from anybody.
	displaced, err = a.RecordIssued(t.Context(), "epyc-1", "SHA256:first", "cert-1b")
	if err != nil {
		t.Fatalf("re-record: %v", err)
	}

	if displaced != "" {
		t.Errorf("re-issuing to the same key reported %q as displaced", displaced)
	}

	// A different key under the same name is the case that has to be reported.
	displaced, err = a.RecordIssued(t.Context(), "epyc-1", "SHA256:second", "cert-2")
	if err != nil {
		t.Fatalf("displacing record: %v", err)
	}

	if displaced != "SHA256:first" {
		t.Errorf("issuing under a claimed name reported %q displaced, want SHA256:first; the "+
			"operator is never told the key they compared has been retired", displaced)
	}
}

// A DENIED NAME IS FREE AGAIN, and until now nothing was.
//
// The name is claimed by the first key to ask, which is the property approval
// depends on: an operator who compared a fingerprint yesterday must not find
// themselves approving a different machine today under a name they already
// trust. But the conflict applied to a DENIED row too, and denying was the only
// tool an operator had — so a name, once asked for, could never be used by any
// other key.
//
// That is not a hypothetical. The enrolling process holds its private key in
// memory while it waits for a human; a reboot or a Ctrl-C loses the key and
// leaves the row. The machine retries, generates a new key, and is refused
// forever. There was no way back short of editing the database by hand.
func TestADeniedEnrollmentFreesTheNameForAnotherKey(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:lost", "csr-1"); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// The key is gone with the process that made it, so the operator denies it.
	if err := a.DecideEnrollment(t.Context(), "epyc-1", "SHA256:lost", EnrollDenied, ""); err != nil {
		t.Fatalf("deny: %v", err)
	}

	// The machine comes back with a new key.
	rec, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:fresh", "csr-2")
	if err != nil {
		t.Fatalf("a denied name could not be claimed by another key, so it is unusable "+
			"forever: %v", err)
	}

	if rec.State != EnrollPending {
		t.Errorf("the new request is %q, not pending, so no operator will ever see it", rec.State)
	}

	if rec.Fingerprint != "SHA256:fresh" {
		t.Errorf("the pending request still shows the lost key %q", rec.Fingerprint)
	}
}

// AND A PENDING OR APPROVED NAME IS STILL HELD, which is the property that makes
// approval mean anything.
// A NAME RECLAIMED FROM A DENIAL CARRIES NO DECISION.
//
// The row goes back to pending and the operator's list reads decided_at beside
// the state. Leaving the old timestamp there describes a request as decided when
// nothing has decided it, which is the one field somebody triaging a pending list
// uses to tell a fresh request from a stale one.
func TestReclaimingADeniedNameClearsTheDecision(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "aa:bb", "csr-1"); err != nil {
		t.Fatalf("RequestEnrollment: %v", err)
	}

	if err := a.DecideEnrollment(t.Context(), "epyc-1", "aa:bb", EnrollDenied, ""); err != nil {
		t.Fatalf("DecideEnrollment: %v", err)
	}

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "cc:dd", "csr-2"); err != nil {
		t.Fatalf("re-request after a denial: %v", err)
	}

	e, found, err := a.LookupEnrollment(t.Context(), "epyc-1")
	if err != nil || !found {
		t.Fatalf("LookupEnrollment: %v (found %v)", err, found)
	}

	if e.State != EnrollPending {
		t.Fatalf("the reclaimed row is %s, want %s", e.State, EnrollPending)
	}

	if e.DecidedAt != "" {
		t.Errorf("a pending request carries decided_at %q; nothing has decided it",
			e.DecidedAt)
	}

	if e.CertPEM != "" {
		t.Errorf("a pending request carries a certificate %q from the row it replaced",
			e.CertPEM)
	}
}

func TestAClaimedNameStillRefusesASecondKey(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:first", "csr-1"); err != nil {
		t.Fatalf("first request: %v", err)
	}

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:second", "csr-2"); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("a pending name accepted a second key: %v", err)
	}

	if err := a.DecideEnrollment(t.Context(), "epyc-1", "SHA256:first", EnrollApproved, "cert"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	if _, err := a.RequestEnrollment(t.Context(), "epyc-1", "SHA256:second", "csr-2"); !errors.Is(err, ErrEnrollmentConflict) {
		t.Fatalf("an approved name accepted a second key, so a machine an operator already "+
			"trusts can be displaced by anything that asks: %v", err)
	}
}
