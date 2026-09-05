package version

import (
	"strconv"
	"strings"
)

// Compare orders two releases: -1, 0 or 1, and whether it could tell.
//
// COULD-NOT-TELL IS A VERDICT OF ITS OWN, and a caller that collapses it into
// either answer is wrong: a developer's build, a snapshot and an unstamped binary
// reach every guard a release does, and none of them may be refused or waved
// through on the strength of an order that does not exist for them.
//
// BOTH SPELLINGS OF A RELEASE ORDER, WITH AND WITHOUT THE LEADING v. A release
// binary stamps its version as GoReleaser's {{.Version}}, which is the tag
// without the v, and every release from v0.6.0 to v0.9.1 reported itself that
// way: in `billet version`, in its upgrade journals, in the release a node
// registers with, and to every guard that compared it with a channel's vX.Y.Z.
// Compare refused the bare form as "not a release", so on a release binary each
// of those guards was could-not-tell and fell through — checkDowngrade, the
// timer's older-target refusal, the starter's own, and the ledger's release
// watermark — and a stale rollout downgraded a control plane (2026-09-05). The
// canonical spelling is the tag, and Version() now speaks it; the bare form
// stays orderable because journals, node registrations and rollouts written by
// the earlier releases still carry it.
func Compare(a, b string) (int, bool) {
	left, ok := parse(a, false)
	if !ok {
		return 0, false
	}

	right, ok := parse(b, false)
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

// Same reports whether two strings name one release, in either spelling. Two
// strings that cannot be ordered are the same only when they are identical, so
// "(devel)" is itself and nothing else.
func Same(a, b string) bool {
	if a == b {
		return true
	}

	order, ok := Compare(a, b)

	return ok && order == 0
}

// Canonical spells a release as its tag, vX.Y.Z, whichever way it arrived, and
// reports whether the string was a release at all.
func Canonical(v string) (string, bool) {
	nums, ok := parse(v, false)
	if !ok {
		return "", false
	}

	return "v" + strconv.Itoa(nums[0]) + "." + strconv.Itoa(nums[1]) + "." +
		strconv.Itoa(nums[2]), true
}

// IsRelease reports whether a string is a release TAG: the grammar the config,
// the release source and the ledger's watermark accept. The bare form is not a
// tag, because a pin written without the v names a release GitHub has no tag
// for; Canonical is how a bare form becomes one.
func IsRelease(v string) bool {
	_, ok := parse(v, true)

	return ok
}

// parse reads vX.Y.Z, or X.Y.Z unless the v is required, into its three
// numbers.
//
// THE SAME GRAMMAR THE CONFIG AND THE RELEASE SOURCE ACCEPT: three decimal
// numbers with no leading zero, nothing after. A prerelease suffix is refused
// rather than ordered, because billet publishes none and an ordering rule for
// them would be one nothing ever tests.
func parse(v string, requireV bool) ([3]int, bool) {
	var out [3]int

	rest, found := strings.CutPrefix(v, "v")
	if !found && requireV {
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

		// CHECKED, because a component long enough to overflow would otherwise
		// come back as some unrelated number with ok=true, and be recorded or
		// ordered as one.
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}

		out[i] = n
	}

	return out, true
}
