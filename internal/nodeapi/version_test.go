package nodeapi

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// THE HIGHEST VERSION BOTH BUILDS SPEAK, and nothing else.
//
// Picking the node's preference would put a new node's semantics on an old
// server; picking the server's would do the reverse. Both are the mismatch the
// version log records as producing silent wrong answers rather than errors, so
// the only safe choice is the top of the overlap.
func TestNegotiatePicksTheHighestVersionBothSpeak(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		node  Range
		self  Range
		want  int
		agree bool
	}{
		{"identical", Range{12, 13}, Range{12, 13}, 13, true},
		{"an older node against a bridge server", Range{12, 12}, Range{12, 13}, 12, true},
		{"a newer node against a bridge server", Range{12, 14}, Range{12, 13}, 13, true},
		{"overlapping at exactly one version", Range{10, 12}, Range{12, 14}, 12, true},
		{"the node is entirely too old", Range{9, 11}, Range{12, 13}, 0, false},
		{"the control plane is entirely too old", Range{14, 15}, Range{12, 13}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := Negotiate(tc.node, tc.self)

			if ok != tc.agree {
				t.Fatalf("Negotiate(%s, %s) agreed=%v, want %v", tc.node, tc.self, ok, tc.agree)
			}

			if got != tc.want {
				t.Errorf("Negotiate(%s, %s) = %d, want %d", tc.node, tc.self, got, tc.want)
			}
		})
	}
}

// AN ABSENT MINIMUM IS EXACTLY ONE VERSION, NEVER A FLOOR OF ZERO.
//
// A build from before the wire was a range sends only the version it speaks. It
// implements that one version and no other, so reading the missing minimum as
// zero records a range that build never had — and the report deciding when an
// old protocol may be RETIRED reads those numbers straight off the ledger. A
// zero there says a host is holding open versions nobody has ever run.
func TestAnAbsentMinimumMeansExactlyOneVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		min, max int
		want     Range
		declared bool
	}{
		{"a build that declared no floor", 0, 12, Range{12, 12}, true},
		{"a build that declared one", 12, 13, Range{12, 13}, true},
		{"a build that speaks exactly one version", 13, 13, Range{13, 13}, true},

		// CONTRADICTORY IS ITS OWN ANSWER, and this is the half the first version
		// got wrong: it normalised each of these to "speaks exactly max", so a peer
		// declaring a floor of 14 was served 12 — a version it had just said it
		// does not implement. Absent, valid and impossible are three facts, and
		// collapsing the third into the first admits the pairing MinVersion exists
		// to refuse.
		{"a floor above the ceiling is not a range", 14, 12, Range{}, false},
		{"a negative floor is not a floor", -3, 12, Range{}, false},
		{"no version at all is not a range", 0, 0, Range{}, false},
		{"a negative newest is not a range", 12, -1, Range{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, declared := DeclaredRange(tc.min, tc.max)

			if declared != tc.declared {
				t.Fatalf("DeclaredRange(%d, %d) declared=%v, want %v",
					tc.min, tc.max, declared, tc.declared)
			}

			if got != tc.want {
				t.Errorf("DeclaredRange(%d, %d) = %s, want %s", tc.min, tc.max, got, tc.want)
			}
		})
	}
}

// AND A CONTRADICTORY DECLARATION NEVER NEGOTIATES ANYTHING.
//
// Asserting `declared == false` above proves the classifier; it does not prove
// that the answer cannot still be used. This walks the pair the way the plane
// does and checks that what comes back is unusable rather than plausible.
func TestAContradictoryDeclarationCannotNegotiate(t *testing.T) {
	t.Parallel()

	node, declared := DeclaredRange(Version+1, MinVersion)
	if declared {
		t.Fatalf("a floor of %d with a newest of %d was accepted as a range",
			Version+1, MinVersion)
	}

	if _, ok := Negotiate(node, Self()); ok {
		t.Errorf("the empty range from a contradictory declaration still negotiated a "+
			"version against %s", Self())
	}
}

// THE FLOOR MUST NEVER REACH A VERSION THE VERSION LOG CALLS UNSAFE.
//
// MinVersion is a promise about SEMANTICS, not about decoding: version 11 and
// below decide a pooled runner's trust from the assignment that caused it to
// launch, and GitHub may hand that runner a different job. Lowering the floor
// past 12 to make an old fleet connect would be offering to be wrong, and it is
// exactly the change somebody makes under upgrade pressure.
func TestTheFloorDoesNotReachAVersionThatIsUnsafe(t *testing.T) {
	t.Parallel()

	if MinVersion < 12 {
		t.Errorf("MinVersion is %d; 12 is the oldest wire whose pool trust is decided "+
			"correctly, so a lower floor offers to speak a version this build knows is wrong",
			MinVersion)
	}

	if Version < MinVersion {
		t.Errorf("this build speaks %s, which is not a range", Self())
	}

	if !Self().Speaks(VersionNodeRelease) {
		t.Errorf("VersionNodeRelease is %d, outside the range this build speaks (%s); a "+
			"requirement keyed to a version nobody can negotiate can never be applied",
			VersionNodeRelease, Self())
	}

	if !Self().Speaks(VersionComputeBarrier) {
		t.Errorf("VersionComputeBarrier is %d, outside the range this build speaks (%s); a "+
			"command keyed to a version nobody can negotiate can never be sent",
			VersionComputeBarrier, Self())
	}

	if !Self().Speaks(VersionNodeWithdrawal) {
		t.Errorf("VersionNodeWithdrawal is %d, outside the range this build speaks (%s); a "+
			"message keyed to a version nobody can negotiate is never sent, and every clean "+
			"stop goes back to waiting out the silence window",
			VersionNodeWithdrawal, Self())
	}
}

// THE SAME NUMBER IN TWO PACKAGES, PINNED RATHER THAN TRUSTED.
//
// internal/alloc cannot import this package — the dependency already runs the
// other way — so the version at which a host can answer an inventory command is
// declared in both. They decide opposite halves of one rule: nodeapi's decides
// whether the command is SENT, alloc's decides whether a host that never gets it
// is reported as unprovable. Drift makes those disagree, and the shape of the
// disagreement is a host that is never asked and never reported, which reads
// exactly like a host that answered.
func TestBarrierVersionsAgree(t *testing.T) {
	t.Parallel()

	if VersionComputeBarrier != alloc.BarrierWireVersion {
		t.Errorf("nodeapi says a host can answer a barrier from wire %d and alloc says %d; "+
			"a host between the two is neither asked nor reported",
			VersionComputeBarrier, alloc.BarrierWireVersion)
	}
}
