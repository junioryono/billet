package scripts_test

import (
	"os"
	"strings"
	"testing"
)

// THE PASSTHROUGH'S INTERPRETER IS ONE A JOB CANNOT RECLAIM. A job freeing disk
// deletes $AGENT_TOOLSDIRECTORY, and with the passthrough on a toolcache Python
// the remapped results origin then refused every connection, failing artifact
// uploads (2026-10-04). Every assignment of the interpreter must name the system
// one.
func TestTheActionsPassthroughRunsOnTheSystemPython(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}

	assigned := 0
	for line := range strings.Lines(string(raw)) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		value, found := strings.CutPrefix(trimmed, "python_runtime=")
		if !found {
			continue
		}
		assigned++
		if value != "/usr/bin/python3" && value != `""` {
			t.Errorf("the passthrough's interpreter is chosen from somewhere a job can delete: %q", trimmed)
		}
	}
	if assigned == 0 {
		t.Fatal("the agent no longer assigns python_runtime; this test is not looking at it")
	}
	if !strings.Contains(string(raw), "python_runtime=/usr/bin/python3") {
		t.Error("the passthrough no longer runs on /usr/bin/python3")
	}
}
