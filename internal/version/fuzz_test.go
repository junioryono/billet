package version

import (
	"slices"
	"strings"
	"testing"
)

// RELEASES ORDER CONSISTENTLY, BY THEIR NUMBERS, OR NOT AT ALL. For any two
// strings: whether an order exists does not depend on which comes first, and
// when it does the two orders are opposite and are the order of the three
// numbers read independently; a release equals itself and its other spelling;
// Same agrees with itself either way round; and Canonical and IsRelease agree
// with Compare about what is a release.
func FuzzVersionCompare(f *testing.F) {
	for _, seed := range [][2]string{
		{"v0.12.3", "0.12.3"}, {"v0.9.1", "v0.10.0"}, {"v1.2.3", "v1.2.4"}, {"v2.0.0", "v1.99.99"},
		{"(devel)", "v1.0.0"}, {"", ""}, {"v01.2.3", "v1.2.3"}, {"v1.2.3-rc.1", "v1.2.3"},
		{"v99999999999999999999.0.0", "v1.0.0"}, {"vv1.2.3", "v1.2.3"}, {"1.2", "1.2.3.4"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		ab, okAB := Compare(a, b)
		ba, okBA := Compare(b, a)

		if okAB != okBA {
			t.Fatalf("Compare(%q, %q) could tell %v and the reverse %v", a, b, okAB, okBA)
		}

		if okAB && ab != -ba {
			t.Fatalf("Compare(%q, %q) = %d but the reverse is %d", a, b, ab, ba)
		}

		if Same(a, b) != Same(b, a) {
			t.Fatalf("Same(%q, %q) is not symmetric", a, b)
		}

		self, okSelf := Compare(a, a)
		if okSelf && self != 0 {
			t.Fatalf("Compare(%q, itself) = %d", a, self)
		}

		canonical, isRelease := Canonical(a)
		if isRelease != okSelf {
			t.Fatalf("Canonical(%q) says release %v; Compare says it orders %v", a, isRelease, okSelf)
		}

		if !isRelease {
			return
		}

		// THE TWO SPELLINGS OF ONE RELEASE ARE ONE RELEASE, and the canonical
		// spelling is a tag.
		if order, ok := Compare(a, canonical); !ok || order != 0 || !Same(a, canonical) {
			t.Fatalf("%q and its canonical %q are not the same release", a, canonical)
		}

		if !IsRelease(canonical) {
			t.Fatalf("Canonical(%q) = %q, which is not a release tag", a, canonical)
		}

		// AND THE ORDER IS THE NUMBERS' ORDER, read independently of parse: a
		// Compare that answered backwards, or zero for every pair, agrees with
		// every property above.
		other, isOther := Canonical(b)
		if !isOther {
			return
		}

		if want := slices.Compare(numbers(t, canonical), numbers(t, other)); ab != want {
			t.Fatalf("Compare(%q, %q) = %d, but their numbers order %d", a, b, ab, want)
		}
	})
}

// numbers reads the three numbers of a canonical vX.Y.Z by hand.
func numbers(t *testing.T, canonical string) []int {
	t.Helper()

	var out []int

	for part := range strings.SplitSeq(strings.TrimPrefix(canonical, "v"), ".") {
		n := 0

		for _, r := range part {
			n = n*10 + int(r-'0')
		}

		out = append(out, n)
	}

	if len(out) != 3 {
		t.Fatalf("%q is not three numbers", canonical)
	}

	return out
}
