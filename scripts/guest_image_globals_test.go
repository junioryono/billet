package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A GLOBAL'S BINARY GOES WHERE THE PACKAGE MANAGER'S PREFIX POINTS, AND THE
// DEFAULT PREFIX IS THE TOOLCACHE.
//
// npm writes a global's executable into the prefix of the node that runs it, and
// billet's default node is a toolcache entry at <version>/<arch>/bin — a
// directory nothing puts on PATH. So `npm install -g grunt` reported success,
// installed correctly, and left an image with no `grunt` command. gem does the
// same with its own bin directory. A real build died on exactly that: the image
// gate refused `grunt` minutes after npm had said it was installed.
//
// WHAT THIS PINS IS THAT BILLET ASKS. That npm and gem honour these flags is
// their contract and is measured on the builder, not here; what a mutation must
// not survive is billet dropping the flag and going back to the default.
func TestGlobalsAreInstalledOntoPATH(t *testing.T) {
	t.Parallel()

	argv := runGlobals(t)

	for _, want := range []struct {
		what string
		args []string
		why  string
	}{
		{
			what: "npm",
			args: []string{"install", "-g", "--prefix", "/usr/local"},
			why: "without --prefix npm installs into the running node's own prefix, " +
				"which is a toolcache directory no PATH mentions",
		},
		{
			what: "gem",
			args: []string{"install", "--bindir", "/usr/local/bin"},
			why: "without --bindir a gem's executable lands in the ruby's own bin " +
				"directory, which is a toolcache directory no PATH mentions",
		},
	} {
		line := findInvocation(argv, "/usr/local/bin/"+want.what)
		if line == "" {
			t.Errorf("nothing invoked %s at all", want.what)
			continue
		}

		for _, arg := range want.args {
			if !hasWord(line, arg) {
				t.Errorf("the %s invocation is missing %q — %s\ngot: %s",
					want.what, arg, want.why, line)
			}
		}
	}
}

// AND EACH COMMAND IS PROVED TO EXIST BEFORE THE BUILD MOVES ON.
//
// The pipx section already did this; the npm section did not, which is why the
// only thing that noticed the missing prefix was the image gate — half an hour
// later, on a machine that had to be built before it could be asked.
func TestEveryDeclaredGlobalCommandIsProved(t *testing.T) {
	t.Parallel()

	recorded := strings.Join(runGlobals(t), "\n")

	// A NAME FROM THE DECLARATION, NOT A NAME THAT MATCHES ITSELF. `tsc` is
	// typescript's command, so a check derived from the package name would look
	// for `typescript` and pass against an image that has neither.
	for _, cmd := range []string{"grunt", "tsc", "yamllint"} {
		if !hasWord(recorded, "/usr/local/bin/"+cmd) {
			t.Errorf("nothing proves /usr/local/bin/%s exists; a package manager that "+
				"reports success and installs somewhere unreachable goes unnoticed "+
				"until the image gate", cmd)
		}
	}
}

// runGlobals executes install_global_packages with every command it dispatches
// recorded instead of run, and returns one line per invocation.
func runGlobals(t *testing.T) []string {
	t.Helper()

	toolset, err := filepath.Abs(toolsetPathForTest)
	if err != nil {
		t.Fatalf("resolve the pinned declaration: %v", err)
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "argv")

	// billet_tc_run IS THE SEAM, so replacing it records the whole dispatch
	// without a fake of any vendor's tool. Everything it is asked to run
	// SUCCEEDS: the point is which commands billet builds, and a failing probe
	// would stop the function before it reached the next section.
	body := "#!/usr/bin/env bash\nset -uo pipefail\n" +
		"BILLET_TC_TOOLSET=" + toolset + "\n" +
		"billet_tc_run() { printf '%s\\n' \"$*\" >>\"$BILLET_ARGV\"; return 0; }\n" +
		guestImageFunction(t, "install_global_packages") + "\n" +
		"install_global_packages\n"

	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Env = append(os.Environ(), "BILLET_ARGV="+log)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install_global_packages: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the recorded dispatch: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Fatalf("the function dispatched %d commands, so this proves nothing:\n%s",
			len(lines), raw)
	}

	return lines
}

// findInvocation returns the recorded line whose command is prog.
func findInvocation(argv []string, prog string) string {
	for _, line := range argv {
		if strings.HasPrefix(line, prog+" ") {
			return line
		}
	}

	return ""
}

// hasWord reports whether s contains word as a whole space-separated field — a
// substring match would let `--prefix /usr/local/lib` satisfy a check for
// `/usr/local`.
func hasWord(s, word string) bool {
	for _, field := range strings.Fields(s) {
		if field == word {
			return true
		}
	}

	return false
}
