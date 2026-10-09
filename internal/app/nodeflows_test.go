package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A NODE ASKED FOR FLOWS ON A HOST THAT CANNOT MEASURE THEM REFUSES TO START,
// naming the switch and how to set it, for a switch that is off and for one
// that could not be read alike.
func TestANodeAskedForFlowsRefusesAHostThatCannotMeasureThem(t *testing.T) {
	t.Parallel()

	for name, values := range map[string]map[string]string{
		"accounting off":    {"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "1\n"},
		"timestamps off":    {"nf_conntrack_acct": "1\n", "nf_conntrack_timestamp": "0\n"},
		"conntrack absent":  {},
		"timestamps absent": {"nf_conntrack_acct": "1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			dir := filepath.Join(root, "proc", "sys", "net", "netfilter")

			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}

			for file, v := range values {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(v), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			w, err := startFlows(t.Context(), root, time.Second)
			if err == nil || w != nil {
				t.Fatalf("started (%v, %v) on a host that cannot measure flows", w, err)
			}

			for _, want := range []string{"node.monitoring.flows", "sysctl -w net.netfilter.nf_conntrack_acct=1"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}
