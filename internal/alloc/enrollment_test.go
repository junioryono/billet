package alloc

import (
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
