// Package version reports which billet this is.
//
// TWO SOURCES, IN ORDER, and neither is enough on its own.
//
// GoReleaser stamps the tag into the vars below with -ldflags. Go 1.24+ also
// records the VCS tag in the binary by itself, which covers everyone who built
// with a plain `go build` or installed with `go install module@v0.1.0`. Relying
// only on the second would be one build flag away from silently reporting
// "(devel)" in a release artifact — GoReleaser passes -trimpath and its own
// flags, and -buildvcs=false or a build from a tarball with no .git produces
// exactly that.
//
// It matters more than a version string usually does: billet sends this to
// GitHub as part of its client system info on every poll, so a deployment
// showing up in their telemetry is identifiable as billet rather than as
// nothing in particular.
package version

import (
	"runtime/debug"
	"strings"
)

// Injected by the release build:
//
//	-X github.com/junioryono/billet/internal/version.version={{.Version}}
//
// Left empty by an ordinary build, which is what makes the fallback below the
// normal path rather than the exceptional one.
var (
	version string
	commit  string
	date    string
)

// unknown is what billet reports when it genuinely does not know.
//
// NEVER AN EMPTY STRING. Empty is what a field that was never populated looks
// like, so it cannot be told apart from a bug in whatever is reading it — and
// one of the readers is GitHub's telemetry, where billet has no way to notice
// that it has been sending nothing.
const unknown = "(unknown)"

// Version is the release this binary was built from.
func Version() string {
	info, ok := debug.ReadBuildInfo()

	return resolve(version, info, ok)
}

// resolve picks the version from the two sources, in order.
//
// A SEPARATE FUNCTION SO THE FALLBACKS CAN BE TESTED. debug.ReadBuildInfo always
// succeeds inside a test binary and always reports "(devel)", so a test that
// called Version() directly could never reach the second or third branch — and a
// mutation that deleted either of them survived, which reads exactly like the
// code being unnecessary rather than untested.
func resolve(injected string, info *debug.BuildInfo, ok bool) string {
	if v := strings.TrimSpace(injected); v != "" {
		return v
	}

	if ok && info != nil {
		if v := strings.TrimSpace(info.Main.Version); v != "" {
			return v
		}
	}

	return unknown
}

// Revision is the commit this binary was built from, or "" if nothing recorded
// one.
func Revision() string {
	if c := strings.TrimSpace(commit); c != "" {
		return c
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}

	return ""
}

// Dirty reports whether the tree had uncommitted changes at build time.
func Dirty() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.modified" {
			return s.Value == "true"
		}
	}

	return false
}

// String is the one-line description a bug report will quote.
//
// Assembled from the parts that are actually present rather than from a fixed
// template, so an unstamped build prints something readable instead of a line of
// empty brackets and stray commas.
func String() string {
	v := Version()
	parts := []string{v}

	// NOT REPEATED. Go's own pseudo-version for an untagged build already embeds
	// the short revision and a +dirty suffix, so appending both again produces
	// "v0.0.0-20260811035856-83de6dda9f5b+dirty 83de6dda9f5b (dirty)" — three
	// statements of the same two facts, in a line an operator is meant to be able
	// to read at a glance.
	if rev := Revision(); rev != "" {
		short := rev
		if len(short) > 12 {
			short = short[:12]
		}

		if !strings.Contains(v, short) {
			parts = append(parts, short)
		}
	}

	if d := strings.TrimSpace(date); d != "" {
		parts = append(parts, d)
	}

	out := strings.Join(parts, " ")

	if Dirty() && !strings.Contains(out, "dirty") {
		out += " (dirty)"
	}

	return out
}
