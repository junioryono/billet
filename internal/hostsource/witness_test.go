package hostsource

import "testing"

// THE ZERO VERDICT IS COULD-NOT-TELL, which admits nothing and holds nothing.
func TestTheZeroVerdictIsCouldNotTell(t *testing.T) {
	t.Parallel()
	var t0 Testimony
	if t0.Verdict != VerdictUnknown || t0.Verdict.String() != "unknown" ||
		VerdictVouches.String() != "vouches" || VerdictContradicts.String() != "contradicts" {
		t.Fatalf("verdicts read %v %v %v", t0.Verdict, VerdictVouches, VerdictContradicts)
	}
}
