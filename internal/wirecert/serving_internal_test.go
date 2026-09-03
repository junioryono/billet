package wirecert

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"
)

const servingDeployment = "0123456789abcdef0123456789abcdef"

// TestAStartThatMissesAWholeRotationAndRetirementStartsOver covers the one
// interleaving the publication order does NOT bound.
//
// The order a rotation writes in decides what a reader can see INSIDE one
// rotation. It says nothing about a whole rotation AND its retirement passing
// between two of a reader's own reads: the current pair is read as generation A,
// A is then rotated to B and A retired, and the read of ca-previous.crt that
// follows finds nothing — so A comes back as issuing, presenting and trusted.
// That is a control plane serving the authority an operator has just retired,
// and unlike everything else in this area it is not fail-closed: it starts, it
// looks healthy, and the fleet that has renewed onto B cannot verify it.
//
// STAGED THROUGH THE HOOK, because the window is between two syscalls. A
// goroutine racing LoadServing would pass on every run that did not interleave,
// which is a test that agrees with the bug.
func TestAStartThatMissesAWholeRotationAndRetirementStartsOver(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadOrCreateCA(dir, servingDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	var fresh *CA

	// ONCE, so the retry has a settled directory to land on. Firing every time
	// would exhaust the bound and test that instead.
	fired := false

	onServingRead = func() {
		if fired {
			return
		}

		fired = true

		var err error
		if fresh, err = Rotate(dir, servingDeployment); err != nil {
			t.Errorf("rotate: %v", err)

			return
		}

		if err := Retire(dir, servingDeployment); err != nil {
			t.Errorf("retire: %v", err)
		}
	}

	t.Cleanup(func() { onServingRead = nil })

	authority, err := LoadServing(dir, servingDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if !fired {
		t.Fatal("the hook never ran, so nothing moved underneath the read and this test " +
			"proves nothing")
	}

	if authority.Issuing.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("it would issue from %s; the rotation and its retirement both completed "+
			"during this read, so the only authority on disk is %s",
			authority.Issuing.Fingerprint(), fresh.Fingerprint())
	}

	if authority.Presents.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("it would present %s, which the operator has just retired",
			authority.Presents.Fingerprint())
	}

	if authority.Rotating {
		t.Error("it reports a rotation running after that rotation was retired")
	}

	// AND WHAT IT PRESENTS IS IN WHAT IT TRUSTS. The mismatch is the failure this
	// is about; a fingerprint comparison alone would not name it.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(authority.Trust) {
		t.Fatal("the trust bundle could not be parsed")
	}

	served, err := authority.Presents.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue serving: %v", err)
	}

	leaf, err := LeafOf(served)
	if err != nil {
		t.Fatalf("parse serving: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("this control plane would present a certificate its own trust bundle cannot "+
			"verify: %v", err)
	}
}

// TestAStartThatMissesARetirementAloneStartsOver is the half the test above
// cannot reach.
//
// That one rotates before it retires, so ca.crt always changes — which is
// exactly the condition a confirmation that re-read ca.crt ALONE was checking,
// so it passed while the shorter race went straight through. A retirement
// touches only the previous pair: the current certificate is identical before
// and after, and a snapshot taken across one comes back reporting an overlap
// that is over, presenting an authority the operator has just dropped.
func TestAStartThatMissesARetirementAloneStartsOver(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadOrCreateCA(dir, servingDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh, err := Rotate(dir, servingDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	fired := false

	onServingRead = func() {
		if fired {
			return
		}

		fired = true

		if err := Retire(dir, servingDeployment); err != nil {
			t.Errorf("retire: %v", err)
		}
	}

	t.Cleanup(func() { onServingRead = nil })

	authority, err := LoadServing(dir, servingDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if !fired {
		t.Fatal("the hook never ran, so nothing moved underneath the read")
	}

	if authority.Rotating {
		t.Error("it reports an overlap that was retired while this read was running")
	}

	if authority.Presents.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("it would present %s, which was retired during this read; the only authority "+
			"left is %s", authority.Presents.Fingerprint(), fresh.Fingerprint())
	}

	// AND THE BUNDLE IS THE ONE A NODE ISSUED AFTERWARDS WILL GET. A server left
	// presenting the retired authority is unverifiable to exactly the node an
	// operator adds next, because `billet ca issue` reads the settled directory.
	if len(authority.Trust) != len(fresh.CertPEM()) {
		t.Errorf("the trust bundle still carries the retired authority: %d bytes against %d",
			len(authority.Trust), len(fresh.CertPEM()))
	}
}

// TestOnlyBilletsOwnOrganizationalUnitIsReadAsAGenerationClaim keeps a field
// anyone may write from deciding whether a private key is unlinked.
//
// The claim decides whether `billet ca retire` may unlink a private key, so
// "some string in the OU" must not be the same fact as "billet recorded a
// predecessor here" — an operator or a CA tool may put anything in that field.
//
// A DIRECT TEST OF THE PURE FUNCTION, unusually, because there is no way to
// reach these inputs through the callers: nothing in billet mints an authority
// with an OU it did not write, which is the point. The callers are driven by the
// retire tests one file over.
func TestOnlyBilletsOwnOrganizationalUnitIsReadAsAGenerationClaim(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		ou      []string
		claimed bool
		want    string
		refused bool
	}{
		{name: "nothing at all"},
		{name: "an unrelated unit", ou: []string{"Platform Engineering"}},
		{name: "a near miss", ou: []string{"replaces SHA256:abc"}},
		{
			name:    "billet's own",
			ou:      []string{replacesPrefix + "SHA256:abc"},
			claimed: true,
			want:    "SHA256:abc",
		},
		{
			name:    "billet's own beside an unrelated unit",
			ou:      []string{"Platform Engineering", replacesPrefix + "SHA256:abc"},
			claimed: true,
			want:    "SHA256:abc",
		},
		// ONE CERTIFICATE CANNOT HAVE REPLACED TWO AUTHORITIES, so picking one to
		// believe is exactly the guess this whole check exists to remove.
		{
			name:    "two claims",
			ou:      []string{replacesPrefix + "SHA256:abc", replacesPrefix + "SHA256:def"},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, claimed, err := replacedAuthority(&x509.Certificate{
				Subject: pkix.Name{OrganizationalUnit: tc.ou},
			})

			switch {
			case tc.refused && err == nil:
				t.Fatal("a certificate naming two predecessors was accepted")
			case !tc.refused && err != nil:
				t.Fatalf("replacedAuthority: %v", err)
			case tc.refused:
				return
			}

			if claimed != tc.claimed {
				t.Errorf("claimed = %t, want %t (from %v)", claimed, tc.claimed, tc.ou)
			}

			if got != tc.want {
				t.Errorf("fingerprint = %q, want %q", got, tc.want)
			}
		})
	}
}
