package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// toolcacheFixture builds a toolcache tree that satisfies every declared line,
// and a JDK for every declared java version, minus whatever a case takes away.
type toolcacheFixture struct {
	// omitNode leaves out the entry for this node glob entirely.
	omitNode string
	// unmarkedNode creates the entry without its x64.complete marker.
	unmarkedNode string
	// emptyNode creates the marker and no x64 directory beside it — the shape a
	// pathname-only check accepts and no job can use.
	emptyNode string
	// brokenNode creates everything and a bin/node that does not run.
	brokenNode string
	// jdkPath overrides where JAVA_HOME_* point; empty means a working JDK.
	jdkPath string
	// versionedJDKPath overrides only the JAVA_HOME_<v>_X64 entries, leaving the
	// default JAVA_HOME working — which is what isolates the per-version check
	// from the default one. With both pointing at the same broken path, removing
	// the per-version check changes nothing and its mutant survives.
	versionedJDKPath string
	// nonSemver names a declared line whose entry is built with a name
	// @actions/tool-cache will not parse -- a prerelease suffix, which is what a
	// vendor's own tag looks like. It still matches the gate's glob, so only a
	// version check refuses it.
	nonSemver string
	// neighbour names a declared line whose entry is built ONE LINE OVER: a bare
	// `3.9` becomes `3.90.1` rather than `3.9.19`. It exists for toolcachePattern,
	// whose comment says a bare line gets `.` before the star precisely so `3.9*`
	// cannot match `3.90` -- a rule that was documented and, until this case,
	// asserted nowhere. Mutating the pattern to `glob + "*"` was measured to
	// survive the whole suite.
	neighbour string
	// omitToolchain leaves one PATH toolchain out of the image entirely, which is
	// the only way to prove the gate's execution check fires: every other fixture
	// fault damages a toolcache entry, and cmake, pwsh and dotnet are not entries.
	omitToolchain string
	// omitCodeQLMarker leaves out the pinned-version file INSIDE the CodeQL entry
	// while leaving everything else complete. Every other fault knob damages node
	// or Java, and this requirement is the one thing in the gate that is not
	// uniform across tools -- so without it the check has no case at all.
	omitCodeQLMarker bool
	// unpublished names declared lines ("Ruby 4.0.*") the fixture neither installs
	// nor is expected to: it records them the way a build does, in the file the
	// gate consults. This is the Ruby 4.0 shape -- a version GitHub declares ahead
	// of its vendor having published anything for it.
	unpublished []string
	// recordedAs overrides what the record FILE says, which is otherwise exactly
	// the lines above. The two have to be settable apart, because the case worth
	// testing is a record that names something ADJACENT to what is missing: with
	// one knob the fixture would omit and excuse the same string every time, and
	// the exactness of the match -- the whole point of it -- would go unexercised.
	recordedAs []string
	// omitJavaEntry leaves out the Java_Temurin-Hotspot_jdk toolcache entry for
	// this feature version, with every JAVA_HOME variable still correct. That is
	// the state a failed symlink leaves behind, and the one lookup path the
	// variables above say nothing about.
	omitJavaEntry string
	// unmarkedJavaEntry and brokenJavaEntry leave the entry in place and break one
	// thing about it. omitJavaEntry cannot isolate either: with no entry at all,
	// removing the marker check OR the java-runs check still leaves nothing to
	// find, so both mutants survive it. These are what make each check its own
	// claim.
	unmarkedJavaEntry string
	brokenJavaEntry   string
}

func (f toolcacheFixture) build(t *testing.T, ts runnerimages.Toolset) (tcDir, envFile string) {
	t.Helper()

	root := t.TempDir()
	tcDir = filepath.Join(root, "hostedtoolcache")
	envFile = filepath.Join(root, "billet-image-env")

	for _, e := range ts.Toolcache {
		// THE TEST'S OWN ANSWER, NOT THE GATE'S. Deriving this from
		// toolcacheBinary makes the two agree by construction: change Ruby's entry
		// to `bin/rubyx` and the fixture builds `bin/rubyx`, the gate looks for
		// `bin/rubyx`, and the case that should have failed passes. A tool the
		// gate checks and this map does not know builds nothing and fails loudly,
		// which is the right way round -- adding a tool to the gate should require
		// saying where its runnable lives.
		binary := wantToolcacheBinary(e.Name)
		if binary == "" {
			continue
		}

		for _, glob := range e.Versions {
			if e.Name == "node" && glob == f.omitNode {
				continue
			}

			if slices.Contains(f.unpublished, e.Name+" "+glob) {
				continue
			}

			// A RESOLVED VERSION FOR EVERY SHAPE THE DECLARATION USES, not just
			// the `22.*` one. PyPy declares bare minors ("3.9"), CodeQL a bare
			// `*`, and a fixture that only handled the trailing-glob form would
			// build `3.920.0` and `*20.0` -- names the gate then rightly refuses,
			// which reads as a broken gate rather than a broken fixture.
			// THREE COMPONENTS, BECAUSE THAT IS WHAT A VENDOR SHIPS. The first
			// version appended a fixed `20.0` and produced `3.10.20.0` for
			// `3.10.*` -- a four-component name no vendor publishes, which made the
			// fixture disagree with reality rather than with the gate. It was the
			// version check that exposed it, by refusing entries the real build
			// never creates.
			resolved := strings.TrimSuffix(glob, "*")

			switch {
			case resolved == "":
				// CodeQL's bare `*`.
				resolved = "2.26.4"
			case strings.Count(resolved, ".") == 2:
				// `3.10.` -> `3.10.7`.
				resolved += "7"
			case strings.HasSuffix(resolved, "."):
				// `22.` -> `22.20.0`.
				resolved += "20.0"
			default:
				// A bare minor: `3.9` -> `3.9.19`.
				resolved += ".19"
			}
			if f.neighbour == e.Name+" "+glob {
				resolved = strings.TrimSuffix(glob, "*") + "0.1"
			}

			if f.nonSemver == e.Name+" "+glob {
				resolved += "-preview1"
			}

			entry := filepath.Join(tcDir, e.Name, resolved)

			if e.Name == "node" && glob == f.emptyNode {
				// The marker, and nothing beside it.
				if err := os.MkdirAll(entry, 0o755); err != nil {
					t.Fatalf("make %s: %v", entry, err)
				}

				writeFile(t, filepath.Join(entry, "x64.complete"), "", 0o600)

				continue
			}

			bin := filepath.Join(entry, "x64", filepath.Dir(binary))
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatalf("make %s: %v", bin, err)
			}

			body := "#!/bin/sh\nexit 0\n"
			if e.Name == "node" && glob == f.brokenNode {
				body = "#!/bin/sh\nexit 1\n"
			}

			writeFile(t, filepath.Join(entry, "x64", binary), body, 0o755)

			// THE MARKER CODEQL'S ACTION LOOKS FOR, INSIDE the entry. It is not the
			// same file as the x64.complete beside it, and an entry can carry one
			// and not the other -- which costs a download on every job while every
			// other check passes.
			if e.Name == "CodeQL" && !f.omitCodeQLMarker {
				writeFile(t, filepath.Join(entry, "x64", "pinned-version"), "", 0o600)
			}

			if e.Name == "node" && glob == f.unmarkedNode {
				continue
			}

			writeFile(t, filepath.Join(entry, "x64.complete"), "", 0o600)
		}
	}

	// ALWAYS, EVEN EMPTY, because that is what a build leaves: the installers
	// truncate this file at the start of every run, so an absent one means an
	// image built by something other than billet. Writing it only when there is
	// something to say would let a fixture pass against a gate that reads a
	// leftover record from a previous build.
	recorded := f.recordedAs
	if recorded == nil {
		recorded = f.unpublished
	}

	writeFile(t, filepath.Join(tcDir, unpublishedFile),
		strings.Join(recorded, "\n")+"\n", 0o600)

	jdk := f.jdkPath
	if jdk == "" {
		jdk = filepath.Join(root, "jvm")
		if err := os.MkdirAll(filepath.Join(jdk, "bin"), 0o755); err != nil {
			t.Fatalf("make the jvm: %v", err)
		}

		writeFile(t, filepath.Join(jdk, "bin", "java"), "#!/bin/sh\nexit 0\n", 0o755)
	}

	// THE ENTRIES setup-java RESOLVES, which are a different lookup path from the
	// variables below. The real build names them from the JDK's own release file
	// (8 becomes 8.0.504-1), so the fixture uses the same shape.
	for _, v := range ts.Java.Versions {
		if v == f.omitJavaEntry {
			continue
		}

		entry := filepath.Join(tcDir, "Java_Temurin-Hotspot_jdk", v+".0.1-1")

		if err := os.MkdirAll(filepath.Join(entry, "x64", "bin"), 0o755); err != nil {
			t.Fatalf("make %s: %v", entry, err)
		}

		body := "#!/bin/sh\nexit 0\n"
		if v == f.brokenJavaEntry {
			body = "#!/bin/sh\nexit 1\n"
		}

		writeFile(t, filepath.Join(entry, "x64", "bin", "java"), body, 0o755)

		if v == f.unmarkedJavaEntry {
			continue
		}

		writeFile(t, filepath.Join(entry, "x64.complete"), "", 0o600)
	}

	var env strings.Builder

	versioned := f.versionedJDKPath
	if versioned == "" {
		versioned = jdk
	}

	for _, v := range ts.Java.Versions {
		env.WriteString("JAVA_HOME_" + v + "_X64=" + versioned + "\n")
	}

	env.WriteString("JAVA_HOME=" + jdk + "\n")

	writeFile(t, envFile, env.String(), 0o600)

	return tcDir, envFile
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// THE GATE IS EXECUTED, against a toolcache tree broken one way at a time.
//
// The guest gate's first toolcache check walked `$TOOLCACHE/$tool/*` and passed
// if the count was non-zero, so deleting a promised version left it green. A test
// asserting this gate's TEXT would repeat that: the question is whether the
// emitted shell refuses an image missing something declared.
//
// AND "MISSING" IS MORE THAN ABSENT. The first version of this gate ran
// `ls -d <glob>/x64.complete`, which proves a pathname exists — not that the
// sibling `x64` directory is there, nor that the runtime inside can execute. A
// review caught that, and one of these cases is the fixture that demonstrated it.
func TestTheToolcacheGateRefusesAnUnusableEntry(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var nodeGlobs []string

	for _, e := range ts.Toolcache {
		if e.Name == "node" {
			nodeGlobs = e.Versions
		}
	}

	if len(nodeGlobs) == 0 {
		t.Skip("the declaration names no node lines")
	}

	gate := gateBlock(t, mustScript(t))

	for _, tc := range []struct {
		name    string
		fixture toolcacheFixture
		wantOK  bool
	}{
		{name: "everything declared is present and runs", wantOK: true},
		{
			name:    "a declared line is missing",
			fixture: toolcacheFixture{omitNode: nodeGlobs[0]},
		},
		{
			// AN ENTRY THE ACTION WILL NOT PARSE. tool-cache keeps only directories
			// that read as an explicit version when resolving a range, so a
			// prerelease suffix makes a complete, runnable entry invisible -- and
			// it satisfies the gate's glob, so nothing but a version check catches
			// it. The guest gate has always checked this; this one did not.
			name:    "an entry named something tool-cache cannot parse",
			fixture: toolcacheFixture{nonSemver: "Ruby 3.4.*"},
		},
		{
			// A TOOLCHAIN THE IMAGE DOES NOT ACTUALLY HAVE. cmake, pwsh and dotnet
			// are on PATH rather than under <tool>/<version>/<arch>, so the entry
			// checks cannot see them -- an image could ship without one and pass
			// every other assertion here.
			name:    "a path toolchain that is not installed",
			fixture: toolcacheFixture{omitToolchain: "cmake"},
		},
		{
			// A LINE ANSWERED BY ITS NEIGHBOUR. PyPy declares a bare `3.9`, and the
			// only thing stopping `3.90.1` from satisfying it is the `.` this gate
			// appends before the star. An entry for a different line is worse than
			// no entry: setup-python resolves it, the job runs on an interpreter it
			// did not ask for, and nothing anywhere reports a mismatch.
			name:    "an entry on a neighbouring line does not satisfy a bare minor",
			fixture: toolcacheFixture{neighbour: "PyPy 3.9"},
		},
		{
			// THE MARKER INSIDE THE ENTRY, which is not the one beside it. The
			// codeql action stats `x64/pinned-version`; @actions/tool-cache stats
			// `x64.complete`. An entry with the second and not the first passes
			// every other check here and costs a bundle download on every job --
			// the whole expense this bakes in to avoid.
			name:    "a codeql entry without the marker its action looks for",
			fixture: toolcacheFixture{omitCodeQLMarker: true},
		},
		{
			// THE RULE THIS GATE NARROWS RATHER THAN BREAKS. GitHub declares Ruby
			// 4.0.* and ruby-builder publishes nothing for it -- 919 assets, none
			// beginning `ruby-4`. Refusing outright would fail every build for a
			// version nobody can supply, so a line the vendor has not published is
			// installed by nobody and RECORDED by the installer, and the gate
			// accepts exactly what the record names.
			name:    "a line the vendor has not published is recorded as such",
			fixture: toolcacheFixture{unpublished: []string{"Ruby 4.0.*"}},
			wantOK:  true,
		},
		{
			// THE RECORD IS MATCHED WHOLE, which is what `-x` buys and nothing else
			// does. A record line that merely CONTAINS the declared line -- an
			// installer that annotated it, a line about a different tool that
			// happens to embed this one -- would excuse it under a plain `-F`, so
			// the fixture records the label with text after it and the gate must
			// still refuse. Written the other way round, with a record SHORTER than
			// the label, the case passes with or without `-x` and proves nothing:
			// that was the first version, and the mutation run is what caught it.
			name: "the record only contains the missing line rather than naming it",
			fixture: toolcacheFixture{
				unpublished: []string{"Ruby 4.0.*"},
				recordedAs:  []string{"Ruby 4.0.* (vendor has not published it)"},
			},
		},
		{
			// COMPLETE ON DISK AND INVISIBLE. tool-cache stats
			// `<version>/x64.complete` and treats a missing one as a
			// half-extracted download.
			name:    "a line is there without its completion marker",
			fixture: toolcacheFixture{unmarkedNode: nodeGlobs[0]},
		},
		{
			// THE CASE THE PATHNAME CHECK ACCEPTED. A marker with no `x64`
			// directory beside it: `ls -d .../x64.complete` succeeds and there is
			// nothing for a job to add to PATH.
			name:    "a marker with no arch directory beside it",
			fixture: toolcacheFixture{emptyNode: nodeGlobs[0]},
		},
		{
			// AND ONE WHOSE RUNTIME DOES NOT RUN. An interpreter needing a
			// library the image lacks extracts perfectly and fails on the first
			// job that uses it.
			name:    "a runtime that does not execute",
			fixture: toolcacheFixture{brokenNode: nodeGlobs[0]},
		},
		{
			// A JAVA_HOME NAMING NOTHING is worse than an absent one: every build
			// tool downstream trusts it instead of installing a JDK.
			name:    "JAVA_HOME names a directory that is not there",
			fixture: toolcacheFixture{jdkPath: "/usr/lib/jvm/nothing"},
		},
		{
			// THE PER-VERSION VARIABLES ARE CHECKED SEPARATELY, and the default
			// being fine is what makes this case about them. setup-java reads
			// JAVA_HOME_<version>_X64 to find a specific JDK; a workflow pinning a
			// toolchain by variable gets a path to nothing while a bare `java`
			// works, which is the confusing half of this failure.
			name:    "a per-version JAVA_HOME names nothing while the default works",
			fixture: toolcacheFixture{versionedJDKPath: "/usr/lib/jvm/nothing"},
		},
		{
			// AND THE TOOLCACHE ENTRY IS A THIRD PATH. Every JAVA_HOME variable is
			// correct here and the JDK runs; what is missing is the
			// Java_Temurin-Hotspot_jdk entry setup-java resolves for a version
			// RANGE. The install creates that as a symlink after the packages
			// land, so a failure there leaves exactly this state — and only a job
			// using setup-java by range would ever notice.
			name:    "a JDK with no toolcache entry",
			fixture: toolcacheFixture{omitJavaEntry: firstJavaVersion(t)},
		},
		{
			// THE ENTRY IS THERE AND UNFINISHED. tool-cache treats a missing
			// x64.complete as a half-extracted download, so this is present on
			// disk and invisible to every lookup.
			name:    "a java toolcache entry without its completion marker",
			fixture: toolcacheFixture{unmarkedJavaEntry: firstJavaVersion(t)},
		},
		{
			// AND ONE WHOSE java DOES NOT RUN. The entry is a symlink into
			// /usr/lib/jvm; a JDK the packages left broken satisfies the path and
			// fails the first job that resolves it.
			name:    "a java toolcache entry whose java does not run",
			fixture: toolcacheFixture{brokenJavaEntry: firstJavaVersion(t)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tcDir, envFile := tc.fixture.build(t, ts)

			runnable := strings.ReplaceAll(gate, toolcacheDir, tcDir)
			runnable = strings.ReplaceAll(runnable, imageEnvFile, envFile)

			// THE PATH TOOLCHAINS LIVE AT ABSOLUTE PATHS A TEST CANNOT WRITE.
			// cmake, pwsh and dotnet are checked by running them from /usr/local
			// and /usr/bin, so the fixture builds stand-ins and the gate is pointed
			// at those the same way it is pointed at a temporary toolcache.
			stubs := toolchainStubs(t, filepath.Dir(tcDir), tc.fixture)
			for path, dir := range stubs {
				runnable = strings.ReplaceAll(runnable, path, dir)
			}

			// AND THE ANDROID SDK, WHICH IS A TREE THE GATE STATS RATHER THAN A
			// COMMAND IT RUNS. Two paths: sdkmanager, and an ndk directory -- an SDK
			// with no NDK builds nothing native, and the environment points at one.
			runnable = strings.ReplaceAll(runnable, androidSDKRoot,
				androidFixture(t, filepath.Dir(tcDir)))

			// AND THE GLOBALS ARE FOUND THROUGH PATH, which no string rewrite can
			// redirect: the gate asks `command -v tsc`, not for an absolute path,
			// because a global's location is the package manager's business. So the
			// fixture puts its stand-ins on PATH the way an image would.
			runnable = "PATH=" + filepath.Join(filepath.Dir(tcDir), "toolchains") +
				":$PATH\nexport PATH\n" + runnable

			out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c",
				"set -eu\n"+runnable).CombinedOutput()

			// THE GATE'S OWN MESSAGE, not just its exit status. Every refusal names
			// the line it is about, and a test that reports only "exit status 1"
			// makes the reader re-run the shell by hand to find out which of two
			// dozen checks fired.
			if tc.wantOK && err != nil {
				t.Fatalf("the gate refused a complete toolcache, so every build would "+
					"fail: %v\n--- it said ---\n%s\n--- the tree was ---\n%s",
					err, out, treeOf(t, tcDir))
			}

			if !tc.wantOK && err == nil {
				t.Fatalf("the gate accepted an image a job cannot use\n--- gate ---\n%s",
					runnable)
			}

			_ = out
		})
	}
}

// gateBlock lifts the toolcache assertions out of the generated script.
func gateBlock(t *testing.T, script string) string {
	t.Helper()

	lines := strings.Split(script, "\n")

	// FROM THE FIRST HELPER'S DEFINITION, since the gate is now shell functions
	// plus a call per declared line rather than the whole block repeated per line.
	//
	// billet_tc, NOT billet_tc_require. The wrapper that derives the pattern and
	// the label is defined first and calls the other; slicing from the second
	// definition cut the first one off, and every call then failed with 127 —
	// which reads as a broken gate rather than as a harness that took half of it.
	start := -1

	for i, l := range lines {
		if l == "billet_tc() {" {
			start = i

			break
		}
	}

	if start < 0 {
		t.Fatal("the script asserts nothing about the toolcache it just installed")
	}

	// TO THE LAST CHECK, which is the default JAVA_HOME. It used to be a five-line
	// block ending in `fi`; the repeated shape is a function now, so the marker is
	// the call rather than the block's closing keyword.
	// AN EXACT LINE, NOT A SUBSTRING. "billet_env_java JAVA_HOME" is a prefix of
	// "billet_env_java JAVA_HOME_8_X64", so a contains-match finds the per-version
	// call first and reports the marker as ambiguous.
	end := -1

	for i, l := range lines {
		if l == "billet_env_java JAVA_HOME" {
			end = i
		}
	}

	if end < 0 {
		t.Fatal("the gate never checks the default JAVA_HOME")
	}

	return strings.Join(lines[start:end+1], "\n") + "\n"
}

// firstJavaVersion is the declaration's first java feature version, so a fixture
// naming one cannot drift from what the gate checks.
func firstJavaVersion(t *testing.T) string {
	t.Helper()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(ts.Java.Versions) == 0 {
		t.Fatal("the declaration names no java versions")
	}

	return ts.Java.Versions[0]
}

// treeOf lists the <tool>/<version> entries a fixture built.
//
// A GATE FAILURE NAMES THE LINE AND NOT THE TREE, so when the fixture is what is
// wrong the message sends the reader to the gate. Printing both is the difference
// between "which check fired" and "why it was right to fire".
//
// Two levels, read directly rather than walked: deeper is each runtime's own
// layout and would bury the answer, and a WalkDir that returns nil on every error
// is a swallowed failure in a function whose whole job is to explain one.
func treeOf(t *testing.T, root string) string {
	t.Helper()

	tools, err := os.ReadDir(root)
	if err != nil {
		return "could not read " + root + ": " + err.Error()
	}

	var b strings.Builder

	for _, tool := range tools {
		b.WriteString("  " + tool.Name() + "\n")

		if !tool.IsDir() {
			continue
		}

		versions, readErr := os.ReadDir(filepath.Join(root, tool.Name()))
		if readErr != nil {
			b.WriteString("    (unreadable: " + readErr.Error() + ")\n")

			continue
		}

		for _, v := range versions {
			b.WriteString("    " + v.Name() + "\n")
		}
	}

	return b.String()
}

// wantToolcacheBinary is the TEST's statement of where each tool's runnable
// lives, written from the vendors' archives rather than read from the gate.
//
// It exists to be a second opinion. A fixture that asks the code under test where
// to put a file can never disagree with it, so the assertion that the gate looks
// in the right place becomes unfalsifiable -- which is the exact shape of the
// vacuous tests this project keeps finding.
func wantToolcacheBinary(tool string) string {
	switch tool {
	case "node":
		return "bin/node"
	case "go":
		return "bin/go"
	case "Python":
		return "bin/python"
	case "PyPy":
		// The symlink the install creates, not the `pypy3` the tarball ships: a
		// workflow runs `python`.
		return "bin/python"
	case "Ruby":
		return "bin/ruby"
	case "CodeQL":
		// The bundle unpacks a `codeql/` directory into the entry rather than a
		// bare bin/.
		return "codeql/codeql"
	}

	return ""
}

// toolchainStubs builds the PATH tools the gate executes, and returns the
// rewrites that point it at them.
//
// A STAND-IN THAT RUNS, because what the gate asserts is that the command works --
// a stub that merely exists would pass a check written to catch a binary that does
// not. omitToolchain leaves one out, which is the case that proves the check fires.
// androidFixture builds the two paths the gate stats and returns the SDK root.
func androidFixture(t *testing.T, root string) string {
	t.Helper()

	sdk := filepath.Join(root, "android-sdk")

	for _, dir := range []string{
		filepath.Join(sdk, "ndk", "27.0.12077973"),
		filepath.Join(sdk, "cmdline-tools", "latest", "bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make the android fixture: %v", err)
		}
	}

	mgr := filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager")
	if err := os.WriteFile(mgr, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write the sdkmanager stub: %v", err)
	}

	return sdk
}

func toolchainStubs(t *testing.T, root string, f toolcacheFixture) map[string]string {
	t.Helper()

	bin := filepath.Join(root, "toolchains")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("make the toolchain directory: %v", err)
	}

	out := make(map[string]string, 3)

	// EVERY COMMAND THE GATE RUNS, whether it names an absolute path or asks PATH.
	// The globals come from the declaration rather than a list here, so a package
	// added upstream is covered without an edit -- and a fixture that had to be
	// edited for that would be a second declaration.
	// clang AND clang++ ARE HERE BECAUSE THE HOST HAS THEM. Without a stand-in the
	// gate ran the machine's own /usr/bin/clang, which exists on a Mac with Xcode
	// and on a GitHub runner, so the check passed without the fixture supplying
	// anything -- a test green for a reason that has nothing to do with the image.
	names := map[string]string{
		"/usr/local/bin/cmake": "cmake",
		"/usr/bin/pwsh":        "pwsh",
		"/usr/bin/dotnet":      "dotnet",
		"/usr/bin/clang":       "clang",
		"/usr/bin/clang++":     "clang++",
		"/usr/local/bin/node":  "node",
		"/usr/local/bin/npm":   "npm",
		"/usr/local/bin/ruby":  "ruby",
		"/usr/local/bin/gem":   "gem",
	}

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, e := range ts.Pipx {
		if e.Cmd != "" {
			names["\x00pipx:"+e.Cmd] = e.Cmd
		}
	}

	for _, e := range ts.NodeModules {
		if e.Command != "" {
			names["\x00node:"+e.Command] = e.Command
		}
	}

	for real, name := range names {
		stub := filepath.Join(bin, name)

		// A KEY BEGINNING WITH NUL IS A PATH-ONLY ENTRY. It exists so the stub is
		// created; there is nothing in the script to rewrite, because the gate finds
		// these through PATH.
		if !strings.HasPrefix(real, "\x00") {
			out[real] = stub
		}

		if f.omitToolchain == name {
			continue
		}

		writeFile(t, stub, "#!/bin/sh\nexit 0\n", 0o755)
	}

	return out
}
