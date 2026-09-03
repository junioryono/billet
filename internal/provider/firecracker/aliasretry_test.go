package firecracker

import (
	"log/slog"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// THE ALIAS SPELLING MUST MATCH THE STORE'S, and it cannot be imported.
//
// The provider may not import the store, so `verified` is written in both places.
// If they drifted, every `@verified` reference would look like a pinned generation
// here -- and the re-resolve below would quietly stop happening, which is a
// failure nothing else would report.
func TestVerifiedAliasMatchesTheStoreSpelling(t *testing.T) {
	// The value ceph.Verified holds. Asserted as a literal because importing it is
	// exactly what the layering forbids.
	if VerifiedAlias != "verified" {
		t.Fatalf("the provider spells the alias %q and the store spells it \"verified\"; "+
			"every @verified reference would be treated as a pinned generation", VerifiedAlias)
	}
}

// A MOVING REFERENCE IS RE-RESOLVED; A PINNED ONE IS NOT.
func TestIsAliasDistinguishesAMovingReferenceFromAPinnedOne(t *testing.T) {
	for _, moving := range []string{
		"ubuntu-2404-x64@verified",
		"ubuntu-2404-x64",
	} {
		if !isAlias(moving) {
			t.Errorf("%q is a moving reference and was treated as pinned, so a generation "+
				"deleted under it would fail the job instead of resolving to the next", moving)
		}
	}

	// A REFERENCE THAT NAMED A GENERATION ASKED FOR ONE EXACT THING, and answering
	// with a different one is what naming a generation exists to prevent.
	for _, pinned := range []string{
		"ubuntu-2404-x64@g20260815033431",
		"ubuntu-2404-x64@g1",
	} {
		if isAlias(pinned) {
			t.Errorf("%q names a generation and was treated as an alias; a retry could "+
				"boot a different image than the one asked for", pinned)
		}
	}
}

// THE GENERATION AN ALIAS RESOLVED TO WAS DELETED BEFORE IT COULD BE CLONED, which
// is the window between resolving and cloning that no lock covers -- holding the
// publish lock across it would queue every launch behind image builds.
func TestCloneResolvedReResolvesWhenTheGenerationVanishesUnderAnAlias(t *testing.T) {
	disk := &fakeDisk{
		device:        "/dev/rbd0",
		resolved:      "ubuntu-2404-x64@g20260814145813",
		cloneGone:     1,
		resolvedAfter: "ubuntu-2404-x64@g20260814145813",
	}

	p := &Provider{disk: disk, log: slog.New(slog.DiscardHandler)}

	spec := provider.Spec{Name: "probe", Image: "ubuntu-2404-x64@g20260815033431"}

	device, err := p.cloneResolved(t.Context(), "ubuntu-2404-x64@verified", &spec)
	if err != nil {
		t.Fatalf("a generation deleted under an alias failed instead of resolving to the "+
			"next: %v", err)
	}

	if device != "/dev/rbd0" {
		t.Errorf("cloned to %q", device)
	}

	if spec.Image != "ubuntu-2404-x64@g20260814145813" {
		t.Errorf("the spec still names %q, so the launch would log a generation it did not "+
			"boot", spec.Image)
	}
}

// A PINNED REFERENCE IS NOT RETRIED, because it asked for one exact generation and
// answering with another is what naming a generation exists to prevent.
func TestCloneResolvedDoesNotReResolveAPinnedGeneration(t *testing.T) {
	disk := &fakeDisk{
		device:        "/dev/rbd0",
		resolved:      "ubuntu-2404-x64@g20260814145813",
		cloneGone:     1,
		resolvedAfter: "ubuntu-2404-x64@g20260814145813",
	}

	p := &Provider{disk: disk, log: slog.New(slog.DiscardHandler)}

	spec := provider.Spec{Name: "probe", Image: "ubuntu-2404-x64@g20260815033431"}

	if _, err := p.cloneResolved(t.Context(), "ubuntu-2404-x64@g20260815033431", &spec); err == nil {
		t.Fatal("a pinned generation that vanished was silently replaced with another")
	}
}

// THE RETRY IS BOUNDED. A reap removing every generation is not a race to paper
// over, and an unbounded loop would hold a job start open indefinitely.
func TestCloneResolvedGivesUpRatherThanLooping(t *testing.T) {
	disk := &fakeDisk{
		device:        "/dev/rbd0",
		resolved:      "ubuntu-2404-x64@g1",
		cloneGone:     99,
		resolvedAfter: "",
	}

	// Each attempt resolves to something new, so only the bound stops this.
	disk.resolvedAfter = "ubuntu-2404-x64@g2"

	p := &Provider{disk: disk, log: slog.New(slog.DiscardHandler)}

	spec := provider.Spec{Name: "probe", Image: "ubuntu-2404-x64@g1"}

	if _, err := p.cloneResolved(t.Context(), "ubuntu-2404-x64@verified", &spec); err == nil {
		t.Fatal("a generation that vanished on every attempt never gave up")
	}
}
