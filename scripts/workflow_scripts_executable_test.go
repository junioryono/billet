package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// workflowScript is a script of this tree a workflow names by its path, with the word before
// it on the same line.
var workflowScript = regexp.MustCompile(`(?m)(?:([^\s;&|("'=]+)[ \t]+)?(?:\./)?(scripts/[A-Za-z0-9_./-]+\.(?:sh|py))\b`)

// interpreters run a script they are handed, which then needs no executable bit.
var interpreters = []string{"bash", "sh", "python", "python3", "source", "."}

// EVERY SCRIPT A WORKFLOW RUNS BY ITS PATH IS EXECUTABLE IN THE TREE. A script committed
// without its executable bit passes every test that runs it through bash, then fails the one
// step that calls it by path: write-guest-channel.sh left the guest channel unadvanced after a
// release it had just published (2026-11-08). One handed to an interpreter needs no bit.
func TestEveryScriptAWorkflowRunsByPathIsExecutable(t *testing.T) {
	t.Parallel()

	workflows, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	if len(workflows) == 0 {
		t.Fatal("no workflows found, so nothing was checked")
	}

	var byPath []string

	for _, wf := range workflows {
		raw, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range workflowScript.FindAllStringSubmatch(string(raw), -1) {
			if slices.Contains(interpreters, m[1]) || slices.Contains(byPath, m[2]) {
				continue
			}

			byPath = append(byPath, m[2])
		}
	}

	// THE PATTERN IS HELD TO WHAT IT MUST FIND, so a pattern that matched nothing could not
	// pass by checking nothing.
	for _, must := range []string{"scripts/write-guest-channel.sh", "scripts/carry-guest-set.sh"} {
		if !slices.Contains(byPath, must) {
			t.Fatalf("the workflows run %s by its path, and the pattern found only %v", must, byPath)
		}
	}

	for _, script := range byPath {
		info, err := os.Stat(filepath.Join("..", script))
		if err != nil {
			t.Errorf("a workflow runs %s, which does not exist: %v", script, err)

			continue
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("a workflow runs %s by its path, and it is not executable (mode %o); run "+
				"`git update-index --chmod=+x %s`", script, info.Mode().Perm(), script)
		}
	}
}
