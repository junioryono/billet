package scripts_test

import (
	"os/exec"
	"strings"
	"testing"
)

// THE SHARED INSTALLERS DEFINE EVERYTHING THE EC2 BUILD CALLS, and this sources
// the file rather than reading it.
//
// The four toolcache installers moved out of build-guest-image.sh so the EC2
// backend ran the same code instead of a second hand-written copy; the guest
// image now gets its toolcache from GitHub's own build, and EC2 still runs these. `bash -n`
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
// A name defined in both is two copies that drift, which is exactly what a
// second hand-written EC2 implementation would have been, and the failure is
// invisible: each caller keeps working, on its own copy.
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
