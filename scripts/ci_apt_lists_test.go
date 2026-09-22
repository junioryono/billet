package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE HOST ROLE'S GATES REFRESH apt BEFORE THEY CONVERGE, UNCONDITIONALLY.
//
// The fleet's runner ships with /var/lib/apt/lists empty: the guest image build
// clears it (measured on a fleet guest 2026-09-22: no lists before an update, 57
// after one). The role's converges install real packages, so they need an update
// first, and for as long as the image lacked python3-apt that update happened as
// a side effect of the step installing it. The image then gained python3-apt,
// the install step took its early exit, the update went with it, and every group
// whose converge installs a package failed with `No package matching
// 'ceph-common'`. Nothing about the image was wrong; the job had been relying on
// a step for something it was not for.
//
// So the refresh is its own step, exactly the refresh and nothing that could skip
// it, ahead of every step that installs a package or runs a gate, and it checks
// that package lists are present afterwards. That check cannot tell fresh lists
// from stale ones; on a runner that starts with none, present means fetched.
func TestTheHostLifecycleJobRefreshesAptBeforeItsFirstGate(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}

	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join("..", ".github", "workflows", "ci.yml"))), &workflow); err != nil {
		t.Fatalf("ci.yml is not valid yaml: %v", err)
	}

	job, ok := workflow.Jobs["host-lifecycle"]
	if !ok {
		t.Fatal("ci.yml has no host-lifecycle job")
	}

	refresh, firstUse := -1, -1

	for i, step := range job.Steps {
		if firstUse < 0 && usesAptOrAGate(step.Run) {
			firstUse = i
		}

		if refresh < 0 && refreshesUnconditionally(step) {
			refresh = i
		}
	}

	if firstUse < 0 {
		t.Fatal("the host-lifecycle job installs nothing and runs no make target, so this test is looking at the wrong job")
	}

	if refresh < 0 {
		t.Fatalf("no step of the host-lifecycle job is exactly the apt refresh (the source rewrite, `sudo apt-get " +
			"update`, the InRelease check); the runner ships with no package lists, so the role's converges " +
			"cannot install anything")
	}

	// STRICTLY BEFORE: a refresh step is never also a user, so an equal index can
	// only mean the order check is looking at the wrong thing.
	if refresh >= firstUse {
		t.Errorf("the apt refresh is step %d (%q) and the first step that needs package lists is step %d (%q); "+
			"it runs against empty lists", refresh, job.Steps[refresh].Name, firstUse, job.Steps[firstUse].Name)
	}
}

// refreshesUnconditionally reports a step that is EXACTLY the refresh: no `if:`,
// no continue-on-error, and a script whose commands are the source rewrite, the
// update and the presence check, in that order, and nothing else.
//
// EQUALITY, NOT A SEARCH, because every searching version of this could be
// satisfied by a script that refreshes nothing: an early exit before the update
// (the shape the side effect had), an exit between the update and the check, or
// both wrapped in a heredoc that is never executed. A step that is exactly these
// commands cannot hide any of that.
func refreshesUnconditionally(step workflowStep) bool {
	if step.If != "" || step.ContinueOnError {
		return false
	}

	var commands []string

	for line := range strings.SplitSeq(step.Run, "\n") {
		if strings.TrimSpace(line) != "" {
			commands = append(commands, line)
		}
	}

	return len(commands) == 3 &&
		strings.HasPrefix(commands[0], "sudo sed -i ") &&
		strings.HasSuffix(commands[0], " /etc/apt/sources.list.d/ubuntu.sources") &&
		commands[1] == "sudo apt-get update" &&
		commands[2] == "ls /var/lib/apt/lists/*InRelease >/dev/null"
}

// usesAptOrAGate reports a step that installs a package or runs a make target,
// which is everything that needs the package lists.
func usesAptOrAGate(script string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "make ") || strings.Contains(line, "apt-get install") ||
			strings.Contains(line, "apt install") {
			return true
		}
	}

	return false
}

// THE PREDICATE IS ITSELF A GATE, so the shapes it must refuse are tested: the
// step that hid the update behind python3-apt's early exit, a conditional step,
// one that swallows its failure, one without the check or with it in the wrong
// place, and the ones a review used to fool the searching version: an exit
// between the update and the check, and both wrapped in a heredoc that never runs.
func TestRefreshesUnconditionallyRefusesTheShapesThatSkipTheUpdate(t *testing.T) {
	t.Parallel()

	const (
		rewrite = "sudo sed -i -e 's,^URIs: x$,URIs: y,' /etc/apt/sources.list.d/ubuntu.sources\n"
		update  = "sudo apt-get update\n"
		check   = "ls /var/lib/apt/lists/*InRelease >/dev/null\n"
		good    = rewrite + update + check
	)

	for _, tc := range []struct {
		name string
		step workflowStep
		want bool
	}{
		{"the refresh", workflowStep{Run: good}, true},
		{
			"behind an early exit",
			workflowStep{Run: "[ \"$(dpkg-query -W python3-apt)\" = installed ] && { echo installed; exit 0; }\n" + good},
			false,
		},
		{"conditional", workflowStep{If: "matrix.group == 'roles'", Run: good}, false},
		{"allowed to fail", workflowStep{Run: good, ContinueOnError: true}, false},
		{"no index check", workflowStep{Run: rewrite + update}, false},
		{"the check in a comment", workflowStep{Run: rewrite + update + "# " + check}, false},
		{"the check before the update", workflowStep{Run: rewrite + check + update}, false},
		{"an exit between the update and the check", workflowStep{Run: rewrite + update + "exit 0\n" + check}, false},
		{"a heredoc that never runs", workflowStep{Run: ": <<'NOT_EXECUTED'\n" + good + "NOT_EXECUTED\n"}, false},
		{"the refresh and a gate in one step", workflowStep{Run: good + "make alert-lifecycle\n"}, false},
	} {
		if got := refreshesUnconditionally(tc.step); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
