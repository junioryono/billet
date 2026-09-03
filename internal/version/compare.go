package version

import (
	"strconv"
	"strings"
)

// Compare orders two release tags.
//
// STRICT vX.Y.Z AND NOTHING ELSE, and the second answer is the one that matters.
// A development build reports "(devel)", a snapshot reports
// 0.0.0-SNAPSHOT-<sha> and an unstamped binary "(unknown)"; none of those is
// older or newer than a release, so ordering one would be a guess dressed as a
// verdict. ok is false whenever either side is not a release tag, and every
// caller that refuses or records on the strength of this answer does neither
// when it is false. That is what keeps a downgrade guard from refusing a
// developer's own build against a ledger it has every right to open.
//
// The result is negative when a is older than b, zero when they name the same
// release and positive when a is newer.
func Compare(a, b string) (int, bool) {
	left, ok := parseRelease(a)
	if !ok {
		return 0, false
	}

	right, ok := parseRelease(b)
	if !ok {
		return 0, false
	}

	for i := range left {
		switch {
		case left[i] < right[i]:
			return -1, true
		case left[i] > right[i]:
			return 1, true
		}
	}

	return 0, true
}

// IsRelease reports whether a string is a release tag Compare can order.
func IsRelease(v string) bool {
	_, ok := parseRelease(v)

	return ok
}

// parseRelease reads vX.Y.Z into its three numbers.
//
// THE SAME GRAMMAR THE CONFIG AND THE RELEASE SOURCE ACCEPT: a leading v, three
// decimal numbers with no leading zero, nothing after. A prerelease suffix is
// refused rather than ordered, because billet publishes none and an ordering
// rule for them would be one nothing ever tests.
func parseRelease(v string) ([3]int, bool) {
	var out [3]int

	rest, found := strings.CutPrefix(v, "v")
	if !found {
		return out, false
	}

	parts := strings.Split(rest, ".")
	if len(parts) != len(out) {
		return out, false
	}

	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return out, false
		}

		for _, r := range part {
			if r < '0' || r > '9' {
				return out, false
			}
		}

		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}

		out[i] = n
	}

	return out, true
}
