package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoLineResolutionCoversAnInitialRelease drives the build's own jq expression
// against fixture feeds.
//
// THE BUG THIS EXISTS FOR SURVIVED REVIEW AND A COMMIT MESSAGE CLAIMING IT WAS
// FIXED. go names an initial release "go1.26", not "go1.26.0", so a prefix match
// on "go1.26." misses exactly the release that exists on the day a line is new --
// and the `.0` normalization written to handle it could never fire, because
// nothing it could normalize was ever selected. Nothing caught that: the real
// feed always had patches, so every test and every manual check passed.
//
// FIXTURES, BECAUSE THE REAL FEED CANNOT PRODUCE THE CASE. Waiting for go to cut
// a new minor is not a test strategy.
func TestGoLineResolutionCoversAnInitialRelease(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		feed  string
		glob  string
		want  string
		empty bool
	}{
		{
			name: "a line with patches resolves to the newest patch",
			feed: `[{"version":"go1.26.7"},{"version":"go1.26.6"},{"version":"go1.25.14"}]`,
			glob: "1.26.*",
			want: "go1.26.7",
		},
		{
			// THE CASE THE PREFIX MATCH MISSED ENTIRELY.
			name: "a line with only its initial release still resolves",
			feed: `[{"version":"go1.26"},{"version":"go1.25.14"}]`,
			glob: "1.26.*",
			want: "go1.26",
		},
		{
			// AND THE INITIAL RELEASE MUST NOT WIN over a later patch. "go1.26"
			// sorts as [1,26] and "go1.26.1" as [1,26,1]; a shorter array sorts
			// first, so `last` must still be the patch.
			name: "a patch beats the initial release of the same line",
			feed: `[{"version":"go1.26"},{"version":"go1.26.1"}]`,
			glob: "1.26.*",
			want: "go1.26.1",
		},
		{
			// AND IT MUST NOT MATCH A NEIGHBOURING LINE. "go1.2" must never
			// satisfy the 1.26 line, which a sloppier prefix would allow.
			name:  "a shorter neighbouring line does not satisfy this one",
			feed:  `[{"version":"go1.2"},{"version":"go1.25.14"}]`,
			glob:  "1.26.*",
			empty: true,
		},
		{
			name:  "a line the feed does not publish resolves to nothing",
			feed:  `[{"version":"go1.25.14"}]`,
			glob:  "1.26.*",
			empty: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := resolveGoLine(t, tc.feed, tc.glob)

			if tc.empty {
				if got != "" {
					t.Errorf("resolved %q, want nothing; the build must refuse a declared "+
						"line the feed does not publish rather than skip it", got)
				}

				return
			}

			if got != tc.want {
				t.Errorf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheGoDirectoryIsAlwaysAFullSemver: @actions/tool-cache keeps only version
// directories that parse as an explicit version when resolving a range, so an
// entry named "1.26" is skipped rather than matched by `go-version: 1.26` -- it
// would sit on disk, complete and invisible.
func TestTheGoDirectoryIsAlwaysAFullSemver(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ version, want string }{
		{"go1.26", "1.26.0"},
		{"go1.26.7", "1.26.7"},
		{"go1.24.13", "1.24.13"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()

			script := "#!/usr/bin/env bash\nset -euo pipefail\nversion=" + tc.version + "\n" +
				goDirectoryNormalization(t) + "\nprintf '%s\\n' \"$bare\"\n"

			out, err := runHarness(t, script)
			if err != nil {
				t.Fatalf("normalizing %s: %v\n%s", tc.version, err, out)
			}

			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("go %s becomes directory %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func resolveGoLine(t *testing.T, feed, glob string) string {
	t.Helper()

	dir := t.TempDir()

	feedPath := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(feedPath, []byte(feed), 0o600); err != nil {
		t.Fatalf("write the feed: %v", err)
	}

	// THE EXPRESSION IS LIFTED OUT OF THE BUILD rather than copied here, so a
	// change to it that breaks this case fails this test instead of passing it.
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		"meta=$(cat " + feedPath + ")\nglob=" + glob + "\n" +
		goResolutionExpression(t) + "\nprintf '%s\\n' \"$version\"\n"

	out, err := runHarness(t, script)
	if err != nil {
		t.Fatalf("resolving %s: %v\n%s", glob, err, out)
	}

	return strings.TrimSpace(out)
}

// goResolutionExpression extracts the version-selection statement out of
// install_go_toolcache.
func goResolutionExpression(t *testing.T) string {
	t.Helper()

	source := readBuildScript(t)

	const begin = "\t\tversion=$(printf '%s' \"$meta\" |"

	start := strings.Index(source, begin)
	if start < 0 {
		t.Fatal("install_go_toolcache no longer resolves a version from the feed")
	}

	end := strings.Index(source[start:], "')\n")
	if end < 0 {
		t.Fatal("could not find the end of the go version resolution")
	}

	return source[start : start+end+3]
}

// goDirectoryNormalization extracts the case statement that turns a go release
// name into a toolcache directory name.
func goDirectoryNormalization(t *testing.T) string {
	t.Helper()

	source := readBuildScript(t)

	const begin = "\t\tlocal bare=\"${version#go}\"\n"

	start := strings.Index(source, begin)
	if start < 0 {
		t.Fatal("install_go_toolcache no longer derives a directory name from the version")
	}

	end := strings.Index(source[start:], "\t\tesac\n")
	if end < 0 {
		t.Fatal("could not find the end of the go directory normalization")
	}

	return strings.ReplaceAll(source[start:start+end+7], "local ", "")
}

// readBuildScript returns the build script AND the shared toolcache installers.
//
// THEY ARE ONE PROGRAM AT RUNTIME. build-guest-image.sh dots
// internal/runnerimages/install-toolcache.sh in near the top, so a test asking
// "does the build do X" has to look in both — and when the installers moved out,
// six tests went red for exactly that reason rather than because anything broke.
func readBuildScript(t *testing.T) string {
	t.Helper()

	return readScriptFile(t, "build-guest-image.sh") + "\n" +
		readScriptFile(t, toolcacheAssetPath)
}

// toolcacheAssetPath is the shared installer, relative to scripts/.
const toolcacheAssetPath = "../internal/runnerimages/install-toolcache.sh"

func readScriptFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}
