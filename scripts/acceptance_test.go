package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/acceptance.sh has two properties worth a gate, and both are about what
// happens when the run FAILS — which is the only time either matters.
//
// EXECUTED RATHER THAN READ. A test that grepped for `trap` would pass against a
// trap installed after the command it is meant to cover, or one whose teardown
// never runs because of a `set -e` short-circuit two lines above it. These drive
// the real script with a stand-in billet and assert on what it actually invoked.

// fakeBillet writes a stand-in that records every invocation and can be told to
// fail one of them.
func fakeBillet(t *testing.T, dir, failOn string) (string, string) {
	t.Helper()

	log := filepath.Join(dir, "invocations")
	path := filepath.Join(dir, "billet")

	// EVERY ARGUMENT ON ITS OWN LINE, BRACKETED. Logging "$*" is the obvious
	// shape and cannot see the defect it is here for: "$*" joins the arguments
	// with spaces, so `--workspace /a b` records identically whether it arrived
	// as one argument or as two. Measured — the string-splitting mutation
	// SURVIVED against a "$*" log. Brackets make a boundary visible.
	script := "#!/bin/sh\n" +
		"{ printf 'ARGV %s\\n' \"$#\"; for a in \"$@\"; do printf '  <%s>\\n' \"$a\"; done; } >> " +
		shellQuote(log) + "\n" +
		"case \"$1 $2\" in\n"

	if failOn != "" {
		script += "  " + failOn + ") exit 7 ;;\n"
	}

	// `up` MUST CREATE THE WORKSPACE FILES, because the real `down` refuses a
	// workspace that holds no record — so a stand-in that created nothing would
	// make the teardown assertions pass for the wrong reason.
	script += "esac\n" +
		"case \"$1 $2\" in\n" +
		"  'acceptance up')\n" +
		"    while [ $# -gt 0 ]; do\n" +
		"      if [ \"$1\" = --workspace ]; then mkdir -p \"$2\"; : > \"$2/acceptance.json\"; fi\n" +
		"      shift\n" +
		"    done\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in billet: %v", err)
	}

	return path, log
}

func runAcceptanceScript(t *testing.T, billet, workspace string) (string, error) {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("..", "scripts", "acceptance.sh"))
	if err != nil {
		t.Fatalf("resolve the script: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", script,
		"--config", "billet.yaml",
		"--workspace", workspace,
		"--binary", billet)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// THE TEARDOWN RUNS WHEN THE RUN FAILS. This is the whole reason the script
// exists: an acceptance run launches billable compute, and a failure that
// returned without destroying it is the outcome worth engineering against.
func TestTheAcceptanceScriptTearsDownAfterAFailedRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workspace := filepath.Join(dir, "ws")

	billet, log := fakeBillet(t, dir, "'acceptance run'")

	out, err := runAcceptanceScript(t, billet, workspace)
	if err == nil {
		t.Fatalf("the script reported success after a failed run:\n%s", out)
	}

	invocations := readInvocations(t, log)

	if !containsPrefix(invocations, "acceptance down") {
		t.Errorf("the run failed and no teardown was invoked:\n%v\n\n%s", invocations, out)
	}

	// AND THE RUN'S OWN STATUS SURVIVES IT. A trap that exited with the
	// teardown's status would report a clean cleanup as a clean run.
	if code := exitCode(err); code != 7 {
		t.Errorf("the script exited %d; the failing run exited 7, and that is the status an "+
			"operator is acting on", code)
	}
}

// AND WHEN `up` ITSELF FAILS, NOTHING IS TORN DOWN — because there is nothing
// to tear down, and a `down` against a workspace `up` never wrote refuses.
// Installing the trap before `up` would turn every bad invocation of this script
// into a confusing second failure.
func TestTheAcceptanceScriptDoesNotTearDownWhatItNeverCreated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workspace := filepath.Join(dir, "ws")

	billet, log := fakeBillet(t, dir, "'acceptance up'")

	out, err := runAcceptanceScript(t, billet, workspace)
	if err == nil {
		t.Fatalf("the script reported success after a failed up:\n%s", out)
	}

	invocations := readInvocations(t, log)

	if containsPrefix(invocations, "acceptance down") {
		t.Errorf("a teardown ran against a workspace `up` never created:\n%v", invocations)
	}

	if containsPrefix(invocations, "acceptance run") {
		t.Errorf("the run started after `up` failed:\n%v", invocations)
	}
}

// EVERY ARGUMENT ARRIVES AS ONE ARGUMENT. The script builds its invocation from
// positional parameters rather than a string, because a string expanded unquoted
// turns a path with a space into two arguments — and for `--workspace` that
// means a run whose teardown is scoped to a directory nobody named.
func TestTheAcceptanceScriptPassesAPathWithASpaceAsOneArgument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workspace := filepath.Join(dir, "a workspace with spaces")

	billet, log := fakeBillet(t, dir, "")

	out, err := runAcceptanceScript(t, billet, workspace)
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}

	invocations := readInvocations(t, log)

	// THE VALUE AFTER THE FLAG, not a substring of the line. A split path still
	// appears in the joined text — measured: the string-splitting mutation
	// survived exactly that assertion — so what is asserted is that the argument
	// AFTER --workspace is the whole path.
	for _, want := range []string{"acceptance up", "acceptance down"} {
		found := false

		for _, call := range invocations {
			if call.Subcommand() != want {
				continue
			}

			found = true

			got, ok := call.ValueOf("--workspace")
			if !ok {
				t.Errorf("%q was invoked with no --workspace at all: %v", want, call.Args)

				continue
			}

			if got != workspace {
				t.Errorf("%q received --workspace %q, and the path is %q — it arrived split, "+
					"so the teardown is scoped to a directory nobody named",
					want, got, workspace)
			}
		}

		if !found {
			t.Errorf("%q was never invoked:\n%v", want, invocations)
		}
	}
}

func readInvocations(t *testing.T, log string) []invocation {
	t.Helper()

	body, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatalf("read the invocation log: %v", err)
	}

	// ONE INVOCATION PER ENTRY, arguments and all, so a caller can ask about the
	// SHAPE of a call rather than only about the words in it.
	var out []invocation

	var current *invocation

	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		switch {
		case strings.HasPrefix(line, "ARGV "):
			out = append(out, invocation{})
			current = &out[len(out)-1]
		case current != nil && strings.HasPrefix(strings.TrimSpace(line), "<"):
			arg := strings.TrimSpace(line)
			current.Args = append(current.Args, strings.TrimSuffix(strings.TrimPrefix(arg, "<"), ">"))
		}
	}

	return out
}

// invocation is one call to the stand-in, with its argument BOUNDARIES intact.
type invocation struct {
	Args []string
}

// Line is the invocation as a caller would read it, for a diagnostic.
func (i invocation) Line() string { return strings.Join(i.Args, " ") }

// Subcommand is the first two arguments, which is what identifies a call here.
func (i invocation) Subcommand() string {
	if len(i.Args) < 2 {
		return i.Line()
	}

	return i.Args[0] + " " + i.Args[1]
}

// ValueOf returns the argument after a flag, and whether the flag was present.
// THE ARGUMENT AFTER IT, not a substring of the whole line: that is the only way
// to tell one argument carrying a space from two arguments that happen to sit
// beside each other.
func (i invocation) ValueOf(flag string) (string, bool) {
	for n, arg := range i.Args {
		if arg == flag && n+1 < len(i.Args) {
			return i.Args[n+1], true
		}
	}

	return "", false
}

func containsPrefix(calls []invocation, subcommand string) bool {
	for _, call := range calls {
		if call.Subcommand() == subcommand {
			return true
		}
	}

	return false
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if ok := asExitError(err, &exit); ok {
		return exit.ExitCode()
	}

	return -1
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok { //nolint:errorlint // the script's own status, not a wrapped chain
		*target = e

		return true
	}

	return false
}
