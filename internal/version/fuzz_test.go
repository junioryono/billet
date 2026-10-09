package version

import (
	"cmp"
	"math"
	"strconv"
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

		// WHAT IS A RELEASE, judged by a reader of its own before anything
		// returns: a parse that refused everything agrees with every property
		// above and below.
		left, okLeft := release(a)
		right, okRight := release(b)

		if okLeft != isRelease {
			t.Fatalf("%q is a release by the grammar %v, but Canonical says %v", a, okLeft, isRelease)
		}

		if okAB != (okLeft && okRight) {
			t.Fatalf("Compare(%q, %q) could tell %v; by the grammar they are releases %v and %v",
				a, b, okAB, okLeft, okRight)
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

		// AND THE ORDER IS THE NUMBERS' ORDER, read from the inputs by that
		// reader: a Compare that answered backwards, zero for every pair, or
		// that dropped a component, agrees with every property above.
		if !okRight {
			return
		}

		if want := compareRelease(left, right); ab != want {
			t.Fatalf("Compare(%q, %q) = %d, but their numbers order %d", a, b, ab, want)
		}
	})
}

// release reads vX.Y.Z or X.Y.Z by the grammar alone: three decimal numbers,
// no leading zero, each no larger than this platform's int, which is what
// Compare can order. The numbers stay digit strings, so reading them shares
// nothing with parse.
func release(v string) ([3]string, bool) {
	var out [3]string

	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != len(out) {
		return out, false
	}

	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') || strings.Trim(part, "0123456789") != "" {
			return out, false
		}

		if len(part) > len(maxInt) || (len(part) == len(maxInt) && part > maxInt) {
			return out, false
		}

		out[i] = part
	}

	return out, true
}

// maxInt is the largest int on this platform, as digits.
var maxInt = strconv.Itoa(math.MaxInt)

// compareRelease orders two releases' digit strings: a longer number is the
// larger, and two of one length order as text.
func compareRelease(a, b [3]string) int {
	for i := range a {
		if c := cmp.Compare(len(a[i]), len(b[i])); c != 0 {
			return c
		}

		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}

	return 0
}
