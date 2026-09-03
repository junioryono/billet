package ceph

import (
	"testing"
	"time"
)

// A GENERATION'S NAME IS ITS CLOCK, and it is read in UTC.
//
// `rbd snap ls` prints a local-time string with no offset, so two nodes in different
// zones would disagree about the age of the same snapshot — and the question being
// asked is "is a rebuild due", whose wrong answer is a fleet that stops being
// rebuilt. billet names its own generations, so the name is the reliable clock.
func TestAGenerationsNameIsItsBuildTimeInUTC(t *testing.T) {
	t.Parallel()

	got, ok := ParseGeneration("g20260814145813")
	if !ok {
		t.Fatal("a generation billet published was not recognised as one")
	}

	want := time.Date(2026, 8, 14, 14, 58, 13, 0, time.UTC)
	if !got.Built.Equal(want) {
		t.Errorf("built at %v, want %v", got.Built, want)
	}

	// THE ZONE IS PART OF THE ASSERTION. Parsed in a local zone, the same string is a
	// different instant, and the difference is exactly the size of the offset — which
	// on a machine at UTC, where this was written, is zero and would hide the bug.
	if zone, offset := got.Built.Zone(); offset != 0 {
		t.Errorf("the build time was read as %s (offset %d), not UTC", zone, offset)
	}
}

// AND ANYTHING ELSE IS NOT A GENERATION BILLET MADE.
//
// A snapshot created by hand or under an older convention has an unknown age, and
// treating one as recent is how a rebuild stops being due and a fleet quietly
// expires. Refusing to parse is what makes the caller ask for a build instead.
func TestASnapshotBilletDidNotPublishIsNotAGeneration(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		"snap1",
		"backup-before-upgrade",
		"g",
		"g2026",
		"g20260814",           // a date with no time
		"g2026081414581",      // one digit short
		"g202608141458134",    // one digit long
		"20260814145813",      // no prefix
		"G20260814145813",     // the wrong case
		"g20261332145813",     // month 13, day 32
		"g20260814145813-old", // a suffix
	} {
		if _, ok := ParseGeneration(name); ok {
			t.Errorf("%q was accepted as a generation billet published", name)
		}
	}
}

// AND THE NEWEST IS BY BUILD TIME, NOT BY POSITION IN THE LIST.
//
// `rbd snap ls` returns creation order today, and depending on that would make this
// wrong the first time somebody removes and re-adds a snapshot — for a question
// whose wrong answer is "no rebuild is due".
func TestTheNewestGenerationIsTheLatestOneRatherThanTheLastListed(t *testing.T) {
	t.Parallel()

	newest := Generation{}
	found := false

	// Deliberately out of order, and with a non-generation among them.
	for _, name := range []string{
		"g20260101000000",
		"g20260814145813",
		"backup-before-upgrade",
		"g20260301120000",
	} {
		gen, ok := ParseGeneration(name)
		if !ok {
			continue
		}

		if !found || gen.Built.After(newest.Built) {
			newest, found = gen, true
		}
	}

	if !found {
		t.Fatal("no generation was recognised")
	}

	if newest.Name != "g20260814145813" {
		t.Errorf("the newest generation is %q; the latest BUILD is g20260814145813", newest.Name)
	}
}
