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

// AND ONLY A FIRECRACKER NODE MAY HAND OVER. Its VMMs outlive the service and its
// next process adopts them; no other backend has been shown to do both, and a
// Mac's launch agent could not even be asked to drain instead.
func TestOnlyAFirecrackerNodeMayHandOver(t *testing.T) {
	t.Parallel()

	docker := `
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  stop: handoff
`
	if _, err := Load(writeConfig(t, docker)); err == nil ||
		!strings.Contains(err.Error(), "node.stop: handoff needs node.provider: firecracker") {
		t.Errorf("a docker node set to hand over was accepted: %v", err)
	}
	if _, err := Load(writeConfig(t, strings.Replace(docker, "  stop: handoff\n", "  stop: drain\n", 1))); err != nil {
		t.Errorf("a docker node that drains was refused: %v", err)
	}
}
