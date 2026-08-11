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

// Every combination of what a build might know, without mutating a linker
// variable to get at it.
//
// Driven only through String(), the dirty flag could not be reached
// independently — a test binary is either dirty or it is not — so a mutation
// deleting the suffix survived, which reads the same as the suffix being
// pointless. These are the cases an operator actually sees.
func TestRender(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		version, revision, built string
		dirty                    bool
		want                     string
	}{
		{
			name:    "a release build",
			version: "v0.1.0", revision: "abc1234def5678", built: "2026-08-11T00:00:00Z",
			want: "v0.1.0 abc1234def56 2026-08-11T00:00:00Z",
		},
		{
			name:    "a dirty release build",
			version: "v0.1.0", revision: "abc1234def5678", built: "2026-08-11T00:00:00Z", dirty: true,
			want: "v0.1.0 abc1234def56 2026-08-11T00:00:00Z (dirty)",
		},
		{
			// Go's pseudo-version already carries the revision and the dirty flag.
			name:    "an untagged build does not repeat itself",
			version: "v0.0.0-20260811035856-83de6dda9f5b+dirty", revision: "83de6dda9f5b1234", dirty: true,
			want: "v0.0.0-20260811035856-83de6dda9f5b+dirty",
		},
		{
			name:    "nothing but a version",
			version: "(unknown)",
			want:    "(unknown)",
		},
		{
			name:    "a version and a dirty tree",
			version: "(devel)", dirty: true,
			want: "(devel) (dirty)",
		},
		{
			name:    "a revision with no date",
			version: "(devel)", revision: "abc1234def5678",
			want: "(devel) abc1234def56",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.version, tc.revision, tc.built, tc.dirty); got != tc.want {
				t.Errorf("render() = %q, want %q", got, tc.want)
			}
		})
	}
}

// String is what actually gets printed, so it must be readable whatever this
// binary happens to know about itself.
func TestStringIsAlwaysReadable(t *testing.T) {
	got := String()

	if strings.TrimSpace(got) == "" {
		t.Fatal("String() was blank")
	}

	if strings.Contains(got, "()") || strings.HasSuffix(strings.TrimSpace(got), ",") {
		t.Errorf("String() = %q, which reads like a template with holes in it", got)
	}
}
