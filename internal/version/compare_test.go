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
		"v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.3-rc1", "1.2.3-rc1", "v1.2.3 ",
		" v1.2.3", "vx.y.z", "v1..3", "v-1.2.3", "1.2", "01.2.3",
		"v1.2.99999999999999999999999999999", "99999999999999999999999999999.0.0",
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

		if _, ok := Canonical(v); ok {
			t.Errorf("Canonical(%q) claimed a tag", v)
		}

		if Same(v, "v1.2.3") {
			t.Errorf("Same(%q, v1.2.3) = true", v)
		}
	}

	if !IsRelease("v0.4.0") {
		t.Error("IsRelease(v0.4.0) = false")
	}
}

// THE BARE FORM ORDERS, BECAUSE THAT IS WHAT EVERY RELEASE BINARY SAID IT WAS.
//
// GoReleaser stamps {{.Version}}, the tag without its v, and releases v0.6.0 to
// v0.9.1 reported "0.9.1" to every guard that compared it with a channel's
// "v0.9.1". Compare called that "not a release" and every downgrade guard on a
// release binary was could-not-tell: a stale rollout to v0.9.0 downgraded a
// control plane running 0.9.1 (2026-09-05). These are those exact pairs.
func TestCompareOrdersTheBareFormReleaseBinariesReported(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"v0.9.0", "0.9.1", -1},
		{"0.9.1", "v0.9.0", 1},
		{"0.9.0", "v0.9.1", -1},
		{"0.8.0", "v0.9.1", -1},
		{"0.9.1", "0.9.1", 0},
		{"0.9.1", "v0.9.1", 0},
		{"0.10.0", "0.9.9", 1},
	}

	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if !ok {
			t.Errorf("Compare(%q, %q) could not tell; both name a release", c.a, c.b)

			continue
		}

		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// A TAG AND ITS BARE FORM ARE ONE RELEASE, and a bare form is not a tag until it
// is spelled as one: IsRelease is the grammar a pin and the watermark accept, and
// "0.9.1" written into a config names a tag GitHub does not have.
func TestTheTwoSpellingsNameOneReleaseAndOnlyTheTagIsATag(t *testing.T) {
	t.Parallel()

	if !Same("0.9.1", "v0.9.1") || !Same("v0.9.1", "0.9.1") || !Same("v0.9.1", "v0.9.1") {
		t.Error("Same did not read the two spellings as one release")
	}

	if Same("0.9.1", "v0.9.0") || Same("(devel)", "v0.9.1") {
		t.Error("Same joined two different releases, or a non-release to one")
	}

	if !Same("(devel)", "(devel)") {
		t.Error("Same denied an identical non-release string is itself")
	}

	if IsRelease("0.9.1") {
		t.Error("IsRelease accepted the bare form as a tag")
	}

	for in, want := range map[string]string{"0.9.1": "v0.9.1", "v0.9.1": "v0.9.1", "0.10.0": "v0.10.0"} {
		got, ok := Canonical(in)
		if !ok || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
