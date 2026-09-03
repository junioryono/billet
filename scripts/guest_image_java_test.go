package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheJavaToolcacheVersionMatchesEveryRealTemurinRelease uses the exact
// release-file contents of all five declared JDKs, read off a real build.
//
// THE FIRST VERSION OF THIS CODE FAILED ON ALL FIVE and was found by running the
// build, not by review. It anchored on `SEMANTIC="..."` while every Temurin
// release spells the field `SEMANTIC_VERSION`; upstream greps `^SEMANTIC`, which
// matches the longer name by prefix, and porting that as an anchored sed silently
// narrowed it to something that matches nothing.
//
// THE FOUR-COMPONENT CASE IS THE OTHER HALF. 17, 21 and 25 report a fourth
// numeric component, which is not a valid explicit version -- so those entries
// would sit on disk complete and invisible to every range request, which is the
// silent toolcache failure this project keeps finding.
func TestTheJavaToolcacheVersionMatchesEveryRealTemurinRelease(t *testing.T) {
	t.Parallel()

	// VERBATIM FROM /usr/lib/jvm/temurin-<v>-jdk-amd64/release on a real build.
	for _, tc := range []struct {
		jdk, release, want string
	}{
		{"8", `SEMANTIC_VERSION="8.0.504+1"`, "8.0.504-1"},
		{"11", `SEMANTIC_VERSION="11.0.32+9"`, "11.0.32-9"},
		{"17", `SEMANTIC_VERSION="17.0.20.1+1"`, "17.0.20-1"},
		{"21", `SEMANTIC_VERSION="21.0.12.1+1"`, "21.0.12-1"},
		{"25", `SEMANTIC_VERSION="25.0.4.1+1"`, "25.0.4-1"},
	} {
		t.Run("jdk"+tc.jdk, func(t *testing.T) {
			t.Parallel()

			got := javaToolcacheVersion(t, tc.release)

			if got != tc.want {
				t.Errorf("jdk %s release %q -> %q, want %q", tc.jdk, tc.release, got, tc.want)
			}

			// AND IT MUST PARSE AS AN EXPLICIT VERSION, which is the property the
			// exact-string comparison above is a proxy for. tool-cache keeps only
			// directories that do.
			if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(got) {
				t.Errorf("%q is not an explicit version, so tool-cache skips the entry and "+
					"every range request misses a JDK that is installed", got)
			}
		})
	}
}

// TestTheJavaVersionHandlesShapesTheRealFilesDoNotShow covers the padding
// branches, which no current Temurin release exercises.
//
// THEY ARE NOT DEAD CODE: upstream carries the same two, and a JDK reporting `8-1`
// rather than `8.0.504+1` is exactly what the `java -fullversion` fallback
// produces. Untested branches in a fallback path are how a fallback is discovered
// to be broken at the moment it is first needed.
func TestTheJavaVersionHandlesShapesTheRealFilesDoNotShow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, release, want string
	}{
		{"two components", `SEMANTIC_VERSION="8.0+1"`, "8.0.0-1"},
		{"one component", `SEMANTIC_VERSION="9+181"`, "9.0.0-181"},
		{"already three, no prerelease", `SEMANTIC_VERSION="11.0.32"`, "11.0.32"},
		// THE FIELD NAME UPSTREAM DOCUMENTS, in case a future release uses it.
		{"the short field name still works", `SEMANTIC="17.0.1+12"`, "17.0.1-12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := javaToolcacheVersion(t, tc.release); got != tc.want {
				t.Errorf("release %q -> %q, want %q", tc.release, got, tc.want)
			}
		})
	}
}

// TestAJDKThatNamesNoVersionIsRefused: naming the entry anything else would put a
// JDK in the toolcache under a version it is not.
func TestAJDKThatNamesNoVersionIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	release := filepath.Join(dir, "release")
	if err := os.WriteFile(release, []byte("JAVA_VERSION=\"1.8.0_504\"\n"), 0o600); err != nil {
		t.Fatalf("write the release file: %v", err)
	}

	script := "#!/usr/bin/env bash\nset -uo pipefail\n" +
		guestImageFunction(t, "java_toolcache_version") + "\n" +
		"java_toolcache_version " + release + " /nonexistent/java\nexit $?\n"

	out, err := runHarness(t, script)
	if err == nil {
		t.Fatalf("a release file with no semantic version and no runnable java was "+
			"accepted, yielding %q", strings.TrimSpace(out))
	}
}

func javaToolcacheVersion(t *testing.T, releaseBody string) string {
	t.Helper()

	dir := t.TempDir()

	release := filepath.Join(dir, "release")
	if err := os.WriteFile(release, []byte(releaseBody+"\n"), 0o600); err != nil {
		t.Fatalf("write the release file: %v", err)
	}

	// THE FUNCTION IS LIFTED OUT OF THE BUILD, so a change that breaks these cases
	// fails here rather than on the next hour-long build.
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		guestImageFunction(t, "java_toolcache_version") + "\n" +
		"java_toolcache_version " + release + " /nonexistent/java\n"

	out, err := runHarness(t, script)
	if err != nil {
		t.Fatalf("java_toolcache_version(%q): %v\n%s", releaseBody, err, out)
	}

	return strings.TrimSpace(out)
}
