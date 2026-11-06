package hostsource

import (
	"slices"
	"testing"
)

// THE CONSOLE LINE A HOST WRITES IS THE ONE THE PLANE READS: wherever it sits in a
// boot's output, between terminal escapes and carriage returns, each fingerprint is
// read once, in the order written, and only for the order asked about. A line that
// names an order only as a prefix of another's is not that order's.
func TestAConsoleLineIsReadBackForItsOrderOnly(t *testing.T) {
	t.Parallel()
	console := "\x1b[32m[  OK  ]\x1b[0m Started cloud-final.\r\n" +
		ConsoleLine("p1", "SHA256:aB+/c9") + "\r\n" +
		"[  12.0] " + ConsoleLine("p12", "SHA256:other") + "\n" +
		ConsoleLine("p1", "SHA256:aB+/c9") + "\r\n" +
		ConsoleLine("p1", "SHA256:second") + "\n"
	if got := ConsoleFingerprints(console, "p1"); !slices.Equal(got, []string{"SHA256:aB+/c9", "SHA256:second"}) {
		t.Fatalf("p1 read %v", got)
	}
	if got := ConsoleFingerprints(console, "p12"); !slices.Equal(got, []string{"SHA256:other"}) {
		t.Fatalf("p12 read %v", got)
	}
	if got := ConsoleFingerprints(console, "p"); len(got) != 0 {
		t.Fatalf("an order that is only a prefix read %v", got)
	}
	if got := ConsoleFingerprints("nothing here", "p1"); got != nil {
		t.Fatalf("an empty console read %v", got)
	}
}

// THE ZERO VERDICT IS COULD-NOT-TELL, which admits nothing and holds nothing.
func TestTheZeroVerdictIsCouldNotTell(t *testing.T) {
	t.Parallel()
	var t0 Testimony
	if t0.Verdict != VerdictUnknown || t0.Verdict.String() != "unknown" ||
		VerdictVouches.String() != "vouches" || VerdictContradicts.String() != "contradicts" {
		t.Fatalf("verdicts read %v %v %v", t0.Verdict, VerdictVouches, VerdictContradicts)
	}
}
