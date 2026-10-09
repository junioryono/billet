package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withMonitoring is validConfig with a monitoring block on its firecracker node.
func withMonitoring(t *testing.T, block string) string {
	t.Helper()

	anchor := "    cache_pool: billet-cache\n"
	body := strings.Replace(validConfig, firecrackerNode,
		strings.Replace(firecrackerNode, anchor, anchor+block, 1), 1)
	if !strings.Contains(body, "monitoring:") {
		t.Fatal("the node block in validConfig has changed, so these cases patch nothing")
	}

	return body
}

func TestAMonitoringBlockIsReadWithItsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, withMonitoring(t, "  monitoring:\n    rapl: true\n    idle_package_watts: 73.5\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := cfg.Node.Monitoring
	if m == nil || !m.RAPL || m.IdlePackageWatts != 73.5 {
		t.Fatalf("monitoring = %+v", m)
	}
	if d, err := m.IntervalDuration(); err != nil || d != time.Second {
		t.Errorf("interval = %s, %v; want the 1s default", d, err)
	}

	// ABSENT IS OFF.
	cfg, err = Load(writeConfig(t, validConfig))
	if err != nil || cfg.Node.Monitoring != nil {
		t.Fatalf("a node with no block has monitoring %+v, %v", cfg.Node.Monitoring, err)
	}
}

func TestAMonitoringBlockTheNodeCannotHonourIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"an interval below the floor", withMonitoring(t, "  monitoring:\n    interval: 100ms\n"), "outside"},
		{"an interval past RAPL's wrap", withMonitoring(t, "  monitoring:\n    interval: 5m\n"), "outside"},
		{"an interval that is not a duration", withMonitoring(t, "  monitoring:\n    interval: often\n"), "not a duration"},
		{"a baseline without rapl", withMonitoring(t, "  monitoring:\n    idle_package_watts: 73.5\n"), "rapl is not"},
		{"a baseline in milliwatts", withMonitoring(t, "  monitoring:\n    rapl: true\n    idle_package_watts: 73500\n"), "not a package power"},
		{"a negative baseline", withMonitoring(t, "  monitoring:\n    rapl: true\n    idle_package_watts: -1\n"), "not a package power"},
		{"a cloud node", cloudConfig(t, "  max_memory: 256GiB\n", "  max_memory: 256GiB\n  monitoring: {}\n"), "only firecracker, docker and tart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadErr(t, tc.body); !strings.Contains(got, tc.want) {
				t.Fatalf("refused with %q, want it to say %q", got, tc.want)
			}
		})
	}
}

// A TART NODE IS MEASURED BY EACH VM'S OWN PROCESS, so monitoring is admitted
// there and the Linux package counter and its baseline are not.
func TestATartNodeIsMonitoredWithoutRAPL(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    NodeMonitoringConfig
		want string
	}{
		{"the defaults", NodeMonitoringConfig{}, ""},
		{"an interval", NodeMonitoringConfig{Interval: "2s"}, ""},
		{"rapl", NodeMonitoringConfig{RAPL: true}, "macOS's own estimate"},
		{"a baseline", NodeMonitoringConfig{RAPL: true, IdlePackageWatts: 10}, "macOS's own estimate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Node: &NodeConfig{Provider: ProviderTart, Monitoring: &tc.m}}
			errs := c.validateMonitoringNode()
			got := errors.Join(errs...)
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("refused: %v", got)
			case tc.want != "" && (got == nil || !strings.Contains(got.Error(), tc.want)):
				t.Errorf("answered %v, want a refusal saying %q", got, tc.want)
			}
		})
	}
}

// PERF COUNTS A MICROVM'S vCPU THREADS, so only a firecracker node may ask for
// it, and absent is off.
func TestPerfIsFirecrackersAlone(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withMonitoring(t, "  monitoring:\n    perf: true\n")))
	if err != nil {
		t.Fatalf("perf on a firecracker node was refused: %v", err)
	}
	if !cfg.Node.Monitoring.Perf {
		t.Fatal("perf: true was read as off")
	}
	cfg, err = Load(writeConfig(t, withMonitoring(t, "  monitoring:\n    rapl: true\n")))
	if err != nil || cfg.Node.Monitoring.Perf {
		t.Fatalf("a block that does not name perf has perf %v, %v; want off", cfg.Node.Monitoring.Perf, err)
	}

	for _, provider := range []ProviderKind{ProviderDocker, ProviderTart} {
		c := &Config{Node: &NodeConfig{Provider: provider, Monitoring: &NodeMonitoringConfig{Perf: true}}}
		got := errors.Join(c.validateMonitoringNode()...)
		if got == nil || !strings.Contains(got.Error(), "only firecracker can be counted") {
			t.Errorf("perf on a %s node answered %v, want a refusal", provider, got)
		}
		c.Node.Monitoring.Perf = false
		if got := errors.Join(c.validateMonitoringNode()...); got != nil {
			t.Errorf("the same %s node without perf was refused: %v", provider, got)
		}
	}
}
