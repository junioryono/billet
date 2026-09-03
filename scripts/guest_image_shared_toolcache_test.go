package scripts_test

import (
	"os/exec"
	"strings"
	"testing"
)

// THE SHARED INSTALLERS DEFINE EVERYTHING THE BUILD CALLS, and this sources the
// file rather than reading it.
//
// The four toolcache installers moved out of build-guest-image.sh so the EC2
// backend runs the same code instead of a second hand-written copy. `bash -n`
// proves each file parses; it says nothing about a function that moved and left
// its caller behind, which is the failure mode of a move like this.
func TestTheSharedToolcacheInstallersDefineWhatTheBuildCalls(t *testing.T) {
	t.Parallel()

	out, err := exec.CommandContext(t.Context(), "bash", "-c",
		". "+toolcacheAssetPath+" && declare -F | awk '{print $3}'").Output()
	if err != nil {
		t.Fatalf("sourcing %s failed: %v", toolcacheAssetPath, err)
	}

	defined := make(map[string]bool)
	for _, name := range strings.Fields(string(out)) {
		defined[name] = true
	}

	for _, want := range []struct {
		name string
		why  string
	}{
		{"billet_install_toolcache", "the one entry point both callers invoke"},
		{"billet_tc_run", "the chroot indirection that is the whole seam"},
		{"install_toolcache", "what the entry point calls"},
		{"install_node_toolcache", "one per tool, and each refuses a declared version it cannot resolve"},
		{"install_go_toolcache", "including the bare go1.26 line naming"},
		{"install_python_toolcache", "including the offline ensurepip"},
		{"install_java_toolcache", "including the JAVA_HOME_*_X64 writes"},
		{"java_toolcache_version", "SEMANTIC_VERSION, and the fourth component dropped"},
		{"fetch_verified", "no archive is extracted without a published checksum"},
		{"python_release_tag", "and the two helpers it needs"},
		{"python_release_checksum", ""},
		{"fetch_python_toolcache", ""},
		{"toolset_versions", "moved with them because only they read it"},
		{"read_toolset_versions", ""},
	} {
		if !defined[want.name] {
			t.Errorf("%s is not defined by the shared installers — %s", want.name, want.why)
		}
	}
}

// NO FUNCTION IS DEFINED IN BOTH FILES, which is the property the move exists to
// create.
//
// build-guest-image.sh sources the asset, so a name defined in both would be
// silently shadowed by whichever came last — two copies that drift, which is
// exactly what a second hand-written EC2 implementation would have been. The
// failure would be invisible: the build keeps working, on one of the two copies.
func TestNoToolcacheFunctionIsDefinedTwice(t *testing.T) {
	t.Parallel()

	script := functionNames(readScriptFile(t, "build-guest-image.sh"))
	asset := functionNames(readScriptFile(t, toolcacheAssetPath))

	for name := range script {
		if asset[name] {
			t.Errorf("%s is defined in build-guest-image.sh and in %s; the second definition "+
				"silently wins and the two drift", name, toolcacheAssetPath)
		}
	}

	if len(asset) == 0 {
		t.Fatalf("%s defines no functions, so the check above cannot fail", toolcacheAssetPath)
	}
}

// THE BUILD SOURCES THE ASSET, so the pair is one program.
//
// Without the dot, every installer is undefined at the call and the build dies
// after debootstrap — late, expensive, and only on a real run.
func TestTheBuildSourcesTheSharedInstallers(t *testing.T) {
	t.Parallel()

	source := readScriptFile(t, "build-guest-image.sh")

	if !strings.Contains(source, `. "$SCRIPT_DIR/../internal/runnerimages/install-toolcache.sh"`) {
		t.Fatal("build-guest-image.sh does not source the shared installers, so every " +
			"toolcache function is undefined at its call site")
	}

	// AND BEFORE main, since main is where they are called from. A dot inside a
	// function, or after main runs, defines them too late.
	dot := strings.Index(source, `. "$SCRIPT_DIR/../internal/runnerimages/install-toolcache.sh"`)
	mainAt := strings.Index(source, "\nmain() {")

	if mainAt >= 0 && dot > mainAt {
		t.Error("the shared installers are sourced after main is defined; they must be in " +
			"scope before it runs")
	}
}

func functionNames(source string) map[string]bool {
	names := make(map[string]bool)

	for _, line := range strings.Split(source, "\n") {
		name, rest, ok := strings.Cut(line, "() {")
		if !ok || rest != "" || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}

		names[name] = true
	}

	return names
}
