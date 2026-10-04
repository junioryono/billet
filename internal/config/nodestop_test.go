package config

import (
	"strings"
	"testing"
)

// A NODE DRAINS UNLESS IT SAYS OTHERWISE, AND SAYS ONLY ONE OTHER THING (#374).
// Absent and "drain" are the drain every node always did; "handoff" leaves the
// compute for the next node process; anything else is refused when the file is
// read, not at the stop that needed it.
func TestANodeDrainsOnStopUnlessItSaysHandoff(t *testing.T) {
	t.Parallel()

	for key, want := range map[string]bool{
		"drain_timeout: 6h": false,
		"stop: drain":       false,
		"stop: handoff":     true,
	} {
		cfg, err := Load(writeConfig(t, withNodeKey(key)))
		if err != nil {
			t.Fatalf("Load %q: %v", key, err)
		}
		got, err := cfg.Node.HandsOverOnStop()
		if err != nil || got != want {
			t.Errorf("%q: hands over = %v, %v; want %v", key, got, err, want)
		}
	}

	if _, err := Load(writeConfig(t, withNodeKey("stop: sideways"))); err == nil ||
		!strings.Contains(err.Error(), "node.stop") {
		t.Errorf("an unknown node.stop was accepted: %v", err)
	}
}
