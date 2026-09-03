package nodeplane

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A DIGEST BILLET CANNOT READ BECOMES NOTHING, NEVER SOMETHING ELSE.
//
// THIS IS THE OPPOSITE RULE TO safeRelease, AND THE DIFFERENCE IS WHAT THE TWO
// FIELDS DO. A release is a diagnostic: mangling an odd one into a printable
// token costs nothing. A digest is COMPARED — a host whose digest disagrees with
// the rollout's target is BLOCKED — so a mangled one would not be a harmless
// string, it would be a disagreement the node never claimed, and the host would
// be cordoned over a stray character.
//
// The safe reading of "not a digest" is the one billet already has for a host
// that cannot say: empty, which converges on the version and records that
// nothing proved it.
func TestADigestBilletCannotReadIsTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	valid := strings.Repeat("a", 64)

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"a real digest", valid, valid},
		{"nothing at all", "", ""},
		{"padded", "  " + valid + "  ", valid},

		// UPPERCASE IS THE SAME DIGEST. Refusing over how hex was rendered would
		// block a host for a formatting difference, which is the failure the
		// case-insensitive comparison elsewhere already refuses to make.
		{"uppercase", strings.Repeat("A", 64), valid},
		{"mixed case", strings.ToUpper(valid[:32]) + valid[32:], valid},

		// EVERY ONE OF THESE WOULD BE A DISAGREEMENT IF IT WERE REPAIRED RATHER
		// THAN REFUSED, and a disagreement blocks a host.
		{"too short", strings.Repeat("a", 63), ""},
		{"too long", strings.Repeat("a", 65), ""},
		{"not hex", strings.Repeat("z", 64), ""},
		{"one bad character", strings.Repeat("a", 63) + "!", ""},
		{"a newline in the middle", strings.Repeat("a", 32) + "\n" + strings.Repeat("a", 31), ""},
		{"a whole sentence", "this host was installed from somewhere else entirely", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := safeDigest(tc.in); got != tc.want {
				t.Errorf("safeDigest(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// AND WHAT IT RETURNS IS ALWAYS A DIGEST OR NOTHING.
//
// The property matters more than any single case: a rollout compares this value
// against the manifest it decided on, so anything that is neither equal to a
// digest nor empty is a host billet will cordon for a reason nobody chose.
func TestSafeDigestOnlyEverReturnsADigestOrNothing(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"", "abc", strings.Repeat("a", 64), strings.Repeat("A", 64),
		strings.Repeat("g", 64), strings.Repeat("a", 200),
		"\n\n\n", "  ", strings.Repeat("a", 63) + "\x00",
	} {
		got := safeDigest(in)
		if got == "" {
			continue
		}

		if len(got) != sha256HexLen {
			t.Errorf("safeDigest(%q) returned %d characters, which is not a digest",
				in, len(got))
		}

		for _, r := range got {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Errorf("safeDigest(%q) returned %q, which is not hex", in, got)

				break
			}
		}
	}
}

// A PAIRING THAT HAS NO DIGEST FIELD CANNOT SUPPLY ONE.
//
// The field exists from wire 16. Below that the protocol this registration
// settled on has no such field, so a value in it is not something that pairing
// can mean — and taking it anyway would let a caller speaking wire 12 supply
// evidence a rollout treats as proof, or as grounds to BLOCK a host. Neither is
// an outcome that protocol can ask for, and the rule the wire documents for every
// field introduced above MinVersion is that it is checked where it is READ.
//
// DROPPED RATHER THAN REFUSED, because the absence is a state everything
// downstream already reads correctly: a host that cannot say. Refusing would take
// a working node out of the fleet over a field that authorises no capacity, and
// the bridge runs one way — an older node is precisely what a control plane is
// expected to keep serving.
func TestADigestFromAPairingTooOldToHaveOneIsIgnored(t *testing.T) {
	t.Parallel()

	claimed := strings.Repeat("a", 64)

	for negotiated := nodeapi.MinVersion; negotiated < nodeapi.VersionNodeDigest; negotiated++ {
		if got := negotiatedDigest(claimed, negotiated); got != "" {
			t.Errorf("a registration negotiated at wire %d supplied digest %q",
				negotiated, got)
		}
	}

	// AND THE PAIRING THAT DOES HAVE THE FIELD KEEPS IT.
	if got := negotiatedDigest(claimed, nodeapi.VersionNodeDigest); got != claimed {
		t.Errorf("a registration at wire %d lost its digest: %q",
			nodeapi.VersionNodeDigest, got)
	}

	if got := negotiatedDigest(claimed, nodeapi.Version); got != claimed {
		t.Errorf("a registration at the newest wire lost its digest: %q", got)
	}
}
