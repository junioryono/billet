package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// netfilterRoot is a host root whose /proc/sys/net/netfilter holds values.
func netfilterRoot(t *testing.T, values map[string]string) (string, string) {
	t.Helper()

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

	return root, dir
}

// A SWITCH THAT IS OFF IS TURNED ON AND THE NODE STARTS: a reboot that applied
// the host's sysctl.d before the connection tracker loaded left both off on the
// reference node (2026-10-14), and refusing left it refusing until a person
// came.
func TestANodeAskedForFlowsTurnsOnWhatIsOff(t *testing.T) {
	t.Parallel()

	for name, values := range map[string]map[string]string{
		"accounting off": {"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "1\n"},
		"timestamps off": {"nf_conntrack_acct": "1\n", "nf_conntrack_timestamp": "0\n"},
		"both off":       {"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "0\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, dir := netfilterRoot(t, values)

			w, err := startFlows(t.Context(), root, time.Second)
			if err != nil || w == nil {
				t.Fatalf("startFlows = %v, %v; want it started with the switches turned on", w, err)
			}

			for _, file := range []string{"nf_conntrack_acct", "nf_conntrack_timestamp"} {
				got, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil || strings.TrimSpace(string(got)) != "1" {
					t.Errorf("%s reads %q (%v) after the node started, want 1", file, got, err)
				}
			}
		})
	}
}

// A HOST THAT CANNOT MEASURE FLOWS IS STILL REFUSED, naming the switch and how
// to set it: the connection tracker not loaded (no switch to turn on), a value
// that is neither 0 nor 1, and a switch the node could not turn on.
func TestANodeAskedForFlowsRefusesAHostThatCannotMeasureThem(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		values   map[string]string
		readOnly string
		want     string
	}{
		"conntrack absent":  {map[string]string{}, "", "sysctl -w net.netfilter.nf_conntrack_acct=1"},
		"timestamps absent": {map[string]string{"nf_conntrack_acct": "1\n"}, "", "sysctl -w"},
		"unreadable value":  {map[string]string{"nf_conntrack_acct": "2\n", "nf_conntrack_timestamp": "1\n"}, "", "sysctl -w"},
		"cannot turn on": {map[string]string{"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "1\n"},
			"nf_conntrack_acct", "turn on net.netfilter.nf_conntrack_acct"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, dir := netfilterRoot(t, tc.values)

			if tc.readOnly != "" {
				if os.Geteuid() == 0 {
					t.Skip("root writes a read-only file anyway")
				}

				if err := os.Chmod(filepath.Join(dir, tc.readOnly), 0o400); err != nil {
					t.Fatal(err)
				}
			}

			w, err := startFlows(t.Context(), root, time.Second)
			if err == nil || w != nil {
				t.Fatalf("started (%v, %v) on a host that cannot measure flows", w, err)
			}

			// EVERY REFUSAL NAMES THE REMEDY, a failed write included.
			for _, want := range []string{"node.monitoring.flows", tc.want,
				"sysctl -w net.netfilter.nf_conntrack_acct=1", "or remove flows"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}
