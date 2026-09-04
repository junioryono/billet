package version

import "testing"

// THE ORDER IS NUMERIC, NOT LEXICAL. v0.10.0 is newer than v0.9.9, and a
// comparison that sorted the strings would call it older — which, fed to a
// downgrade guard, refuses the newest release as a downgrade.
func TestCompareOrdersReleasesNumerically(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"v0.4.0", "v0.4.0", 0},
		{"v0.4.0", "v0.4.1", -1},
		{"v0.4.1", "v0.4.0", 1},
		{"v0.4.9", "v0.5.0", -1},
		{"v0.9.9", "v0.10.0", -1},
		{"v0.10.0", "v0.9.9", 1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.0.0", "v0.0.1", -1},
	}

	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if !ok {
			t.Errorf("Compare(%q, %q) could not tell; both are release tags", c.a, c.b)

			continue
		}

		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ANYTHING THAT IS NOT A RELEASE TAG IS "COULD NOT TELL", NEVER A VERDICT.
//
// A developer's build, a snapshot and an unstamped binary each reach the same
// guards a release does, and each must be neither refused nor recorded on the
// strength of an order that does not exist for them.
func TestCompareRefusesToOrderWhatIsNotARelease(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		"", "(devel)", "(unknown)", "0.0.0-SNAPSHOT-abc1234", "latest", "main",
		"v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.3-rc1", "1.2.3", "v1.2.3 ",
		" v1.2.3", "vx.y.z", "v1..3", "v-1.2.3",
	} {
		if _, ok := Compare(v, "v1.2.3"); ok {
			t.Errorf("Compare(%q, v1.2.3) claimed to know the order", v)
		}

		if _, ok := Compare("v1.2.3", v); ok {
			t.Errorf("Compare(v1.2.3, %q) claimed to know the order", v)
		}

		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true", v)
		}
	}

	if !IsRelease("v0.4.0") {
		t.Error("IsRelease(v0.4.0) = false")
	}
}
