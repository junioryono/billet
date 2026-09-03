package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE ENTRY POINT ACTUALLY CALLS EVERY INSTALLER, and nothing else proved it.
//
// Deleting every call from install_toolcache would have left every other test
// green: the shared-file tests assert the functions are DEFINED, the delivery
// test asserts the outer entry point is INVOKED, and the gate tests build their
// own toolcache tree rather than running the installer. The build would then
// spend a paid builder and fail at the gate — or, against a base image that
// already had a toolcache, pass.
//
// RECORDING FAKES, sourced AFTER the real file so they shadow it. Running the
// real installers would download several GiB from four upstreams, which is not
// what a unit test is for; what is worth asserting is that each is reached, once,
// with the paths the contract names.
func TestTheEntryPointDispatchesEveryInstaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls")

	// The fakes record their name and arguments. install_toolcache itself is NOT
	// faked — it is the function under test.
	fakes := `
install_node_toolcache()   { printf 'node %s\n'   "$*" >>"` + log + `"; }
install_go_toolcache()     { printf 'go %s\n'     "$*" >>"` + log + `"; }
install_python_toolcache() { printf 'python %s\n' "$*" >>"` + log + `"; }
install_java_toolcache()   { printf 'java %s\n'   "$*" >>"` + log + `"; }
install_pypy_toolcache()   { printf 'pypy %s\n'   "$*" >>"` + log + `"; }
install_ruby_toolcache()   { printf 'ruby %s\n'   "$*" >>"` + log + `"; }
install_codeql_toolcache() { printf 'codeql %s\n' "$*" >>"` + log + `"; }
install_cmake()            { printf 'cmake %s\n'  "$*" >>"` + log + `"; }
install_powershell()       { printf 'pwsh %s\n'   "$*" >>"` + log + `"; }
install_dotnet()           { printf 'dotnet %s\n' "$*" >>"` + log + `"; }
install_clang_default()    { printf 'clang %s\n'  "$*" >>"` + log + `"; }
install_dotnet_tools()     { printf 'dntools %s\n' "$*" >>"` + log + `"; }
install_powershell_modules() { printf 'psmod %s\n' "$*" >>"` + log + `"; }
install_android()          { printf 'android %s\n' "$*" >>"` + log + `"; }
install_default_runtimes() { printf 'defaults %s\n' "$*" >>"` + log + `"; }
install_global_packages()  { printf 'globals %s\n'  "$*" >>"` + log + `"; }
billet_tc_run()            { printf 'run %s\n'    "$*" >>"` + log + `"; }
find()                     { :; }
`

	script := ". " + toolcacheAssetPath + "\n" + fakes + `
BILLET_TC_ARCH=x64 \
	BILLET_TC_ROOT="" \
	BILLET_TC_DIR="` + dir + `/tc" \
	BILLET_TC_IN_TARGET=/opt/hostedtoolcache \
	BILLET_TC_WORK="` + dir + `/work" \
	BILLET_TC_TOOLSET="` + toolsetPathForTest + `" \
	BILLET_TC_ENV_FILE="` + dir + `/env" \
	billet_install_toolcache
`

	out, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("billet_install_toolcache failed: %v\n%s", err, out)
	}

	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("nothing was recorded, so no installer was reached: %v", err)
	}

	got := string(recorded)

	for _, want := range []struct {
		tool string
		why  string
	}{
		{"node", "setup-node finds nothing without it"},
		{"go", "and setup-go"},
		{"python", "and setup-python"},
		{"java", "and setup-java, which also writes the JAVA_HOME variables"},
		{"pypy", "and setup-python's pypy- lines"},
		{"ruby", "and setup-ruby"},
		{"codeql", "and the codeql action, which re-downloads a bundle it cannot find"},
		// THE THREE THAT ARE NOT TOOLCACHE ENTRIES, and the reason this test grew.
		// A restore of mine put the installer FUNCTIONS back and not their calls;
		// every unit test passed, and a real build spent four minutes installing a
		// 5.2G toolcache before its own gate refused the image for a missing cmake.
		// Defined and never called is invisible to everything except the dispatch.
		{"cmake", "and any workflow that builds native code"},
		{"pwsh", "and every actions/*-powershell step"},
		{"dotnet", "and setup-dotnet, which downloads an SDK when it finds none"},
		{"clang", "or a bare `clang` resolves to nothing, because apt's versioned " +
			"packages are co-installable and none of them owns the unsuffixed name"},
		{"dntools", "and the declaration's .dotnet.tools had a Go type, a populated " +
			"field, no installer and no gate — parsed and never used, twice"},
		{"psmod", "and every declared PowerShell and Azure module"},
		{"android", "which is dispatched unconditionally and decides for itself, so " +
			"the licence gate lives in one place rather than at two call sites"},
		// AND THE TWO THAT MAKE THE REST REACHABLE. Without the default runtimes a
		// bare `node` resolves to nothing, and the globals install through them.
		{"defaults", "or a bare node resolves against an apt set that has none"},
		{"globals", "and every declared pipx, npm and gem package"},
	} {
		if !strings.Contains(got, want.tool+" ") {
			t.Errorf("install_%s_toolcache was never called — %s\nrecorded:\n%s",
				want.tool, want.why, got)
		}
	}

	// EACH ONE ONCE. A loop that called an installer twice would download and
	// extract twice, and the second pass would overwrite an entry the first had
	// completed.
	for _, tool := range []string{
		"node", "go", "python", "java", "pypy", "ruby", "codeql",
		"cmake", "pwsh", "dotnet", "defaults", "globals",
	} {
		if n := strings.Count(got, "\n"+tool+" ") + boolToInt(strings.HasPrefix(got, tool+" ")); n != 1 {
			t.Errorf("install_%s_toolcache was called %d times, want once\nrecorded:\n%s",
				tool, n, got)
		}
	}

	// AND THE ONES THAT NEED THE TARGET ROOT GET IT. python and java take it
	// explicitly — python by dynamic scoping until this branch fixed it — so a
	// call missing an argument would install into the wrong place.
	for _, tool := range []string{"python", "java", "pypy", "ruby", "codeql"} {
		for _, line := range strings.Split(got, "\n") {
			if !strings.HasPrefix(line, tool+" ") {
				continue
			}

			if len(strings.Fields(line)) < 2 {
				t.Errorf("install_%s_toolcache was called with no arguments; it needs the "+
					"target root and the toolcache directory, and reading either from a "+
					"caller's locals is the bug this branch removed", tool)
			}
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// toolsetPathForTest is the pinned declaration, relative to scripts/.
const toolsetPathForTest = "../internal/runnerimages/toolset-2404.json"
