package main

import "testing"

// THE OLDEST RUNNER IS THE ONE WITH THE DEADLINE.
//
// A deployment can run several tiers on several images, and GitHub's thirty days
// belong to whichever is furthest behind. Answering with the first tier that
// happened to carry metadata would leave a stale tier expiring unwatched while the
// check stayed green about a current one.
//
// AND VERSIONS ARE NOT STRINGS. "2.9.0" sorts after "2.10.0" lexically and is older
// in fact, so a string comparison picks the wrong tier to worry about — silently,
// and in the direction that reports everything as fine.
func TestTheOlderRunnerIsCompared(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		a, b  string
		older bool
	}{
		{"2.336.0", "2.337.0", true},
		{"2.337.0", "2.336.0", false},
		{"2.336.0", "2.336.0", false},
		// THE ONE A STRING COMPARISON GETS BACKWARDS.
		{"2.9.0", "2.10.0", true},
		{"2.10.0", "2.9.0", false},
		{"3.0.0", "2.999.0", false},
		{"2.336.0", "2.336.1", true},
	} {
		if got := olderRunner(tc.a, tc.b); got != tc.older {
			t.Errorf("olderRunner(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.older)
		}
	}
}

// AND SOMETHING THAT IS NOT A VERSION DOES NOT PANIC OR CLAIM AN ORDER IT CANNOT
// KNOW. A generation's metadata is whatever was written there; this runs on a
// scheduled path where crashing is a worse answer than a stable guess.
func TestAnUnparseableRunnerVersionStillOrders(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ a, b string }{
		{"", "2.336.0"},
		{"unknown", "2.336.0"},
		{"2.x.0", "2.336.0"},
		{"2.336", "2.336.0"},
	} {
		// The assertion is that it answers at all, consistently, rather than what it
		// answers: there is no true order between a version and a non-version.
		if olderRunner(tc.a, tc.b) == olderRunner(tc.b, tc.a) && tc.a != tc.b {
			t.Errorf("olderRunner is not antisymmetric for %q and %q", tc.a, tc.b)
		}
	}
}
