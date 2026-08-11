package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// THE INJECTED VALUE WINS.
//
// GoReleaser stamps the tag through -ldflags. It also passes -trimpath and its
// own build flags, so relying on Go's VCS stamping alone would be one flag away
// from shipping a binary that reports "(devel)" — to operators, and to GitHub,
// which is told this string on every poll.
func TestTheInjectedValueWins(t *testing.T) {
	restore := version
	t.Cleanup(func() { version = restore })

	version = "v1.2.3"

	if got := Version(); got != "v1.2.3" {
		t.Errorf("Version() = %q, want the injected v1.2.3", got)
	}
}

// AND WITHOUT ONE IT FALLS BACK TO WHAT GO RECORDED.
//
// Go 1.24+ sets BuildInfo.Main.Version from the VCS tag, so a plain `go build`
// or a `go install module@v0.1.0` still reports something true. That is the case
// for anyone who did not go through GoReleaser.
func TestItFallsBackToTheBuildInfo(t *testing.T) {
	restore := version
	t.Cleanup(func() { version = restore })

	version = ""

	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info in this binary")
	}

	got := Version()

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		if got != info.Main.Version {
			t.Errorf("Version() = %q, want the build info %q", got, info.Main.Version)
		}

		return
	}

	// A test binary usually reports "(devel)". The contract is that SOMETHING
	// intelligible comes out — an empty version in the system info billet sends
	// GitHub is indistinguishable from a field that was never set.
	if got == "" {
		t.Error("Version() = \"\", which tells an operator nothing and tells GitHub less")
	}
}

// A version is never empty, and never blank. This is the assertion that matters
// for the field GitHub sees on every poll.
func TestAVersionIsNeverEmpty(t *testing.T) {
	restore := version
	t.Cleanup(func() { version = restore })

	for _, injected := range []string{"", "   ", "\t"} {
		version = injected

		if got := strings.TrimSpace(Version()); got == "" {
			t.Errorf("Version() = %q for injected %q", Version(), injected)
		}
	}
}

// The full string is what `billet version` prints and what a bug report quotes,
// so it carries the revision and the build date when it has them.
func TestTheFullStringCarriesWhatItKnows(t *testing.T) {
	restoreV, restoreC, restoreD := version, commit, date
	t.Cleanup(func() { version, commit, date = restoreV, restoreC, restoreD })

	version, commit, date = "v0.1.0", "abc1234", "2026-08-11T00:00:00Z"

	got := String()
	for _, want := range []string{"v0.1.0", "abc1234", "2026-08-11"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}

// A build with nothing stamped still prints something readable rather than a
// line of empty separators.
func TestTheFullStringOfAnUnstampedBuildIsStillReadable(t *testing.T) {
	restoreV, restoreC, restoreD := version, commit, date
	t.Cleanup(func() { version, commit, date = restoreV, restoreC, restoreD })

	version, commit, date = "", "", ""

	got := String()
	if strings.TrimSpace(got) == "" {
		t.Error("String() was blank for an unstamped build")
	}

	if strings.Contains(got, "()") || strings.HasSuffix(strings.TrimSpace(got), ",") {
		t.Errorf("String() = %q, which reads like a template with holes in it", got)
	}
}

// The line is read at a glance, so it must not say the same thing three times.
//
// Go's pseudo-version for an untagged build already embeds the short revision
// and a +dirty suffix. Appending both again produced
// "v0.0.0-20260811035856-83de6dda9f5b+dirty 83de6dda9f5b (dirty)".
func TestTheFullStringDoesNotRepeatItself(t *testing.T) {
	restoreV, restoreC, restoreD := version, commit, date
	t.Cleanup(func() { version, commit, date = restoreV, restoreC, restoreD })

	version, commit, date = "v0.0.0-20260811035856-83de6dda9f5b+dirty", "83de6dda9f5b", ""

	got := String()
	if strings.Count(got, "83de6dda9f5b") != 1 {
		t.Errorf("String() = %q, want the revision to appear once", got)
	}

	if strings.Count(got, "dirty") > 1 {
		t.Errorf("String() = %q, want dirty to be said once", got)
	}
}

// The fallback chain, exercised directly.
//
// debug.ReadBuildInfo always succeeds inside a test binary and always reports
// "(devel)", so calling Version() can never reach the second or third branch of
// this. Testing resolve with a synthetic BuildInfo is the only way to see them —
// and without it, deleting either branch survived every test, which looks
// identical to the branch being unnecessary.
func TestTheFallbackChain(t *testing.T) {
	tagged := &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}
	devel := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	blank := &debug.BuildInfo{Main: debug.Module{Version: ""}}

	for _, tc := range []struct {
		name     string
		injected string
		info     *debug.BuildInfo
		ok       bool
		want     string
	}{
		{"injected wins over build info", "v1.2.3", tagged, true, "v1.2.3"},
		{"build info when nothing is injected", "", tagged, true, "v9.9.9"},
		{"(devel) is still better than nothing", "", devel, true, "(devel)"},
		{"no build info at all", "", nil, false, unknown},
		{"build info with an empty version", "", blank, true, unknown},
		{"a blank injected value is not a value", "   ", tagged, true, "v9.9.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.injected, tc.info, tc.ok); got != tc.want {
				t.Errorf("resolve(%q, …) = %q, want %q", tc.injected, got, tc.want)
			}
		})
	}
}
