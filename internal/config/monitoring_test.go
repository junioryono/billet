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

// FLOWS ARE A FIRECRACKER GUEST'S: only its address is leased on a billet
// bridge the host can read. Asked for on another backend, they are refused.
func TestFlowsAreRefusedWhereTheGuestsAddressIsUnknown(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withMonitoring(t, "  monitoring:\n    flows: true\n")))
	if err != nil {
		t.Fatalf("flows on a firecracker node were refused: %v", err)
	}

	if !cfg.Node.Monitoring.Flows {
		t.Error("flows: true was not read")
	}

	for _, provider := range []ProviderKind{ProviderDocker, ProviderTart} {
		c := &Config{Node: &NodeConfig{Provider: provider, Monitoring: &NodeMonitoringConfig{Flows: true}}}

		got := errors.Join(c.validateMonitoringNode()...)
		if got == nil || !strings.Contains(got.Error(), "node.monitoring.flows") {
			t.Errorf("%s: flows answered %v, want a refusal naming node.monitoring.flows", provider, got)
		}
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

// THE GUEST'S AGENT REPORTS THROUGH THE GUEST CACHE SERVICE, so
// node.monitoring.guest is read on a firecracker node that has node.cache, absent
// is off, and it is refused without node.cache or on any other backend, whose
// guest is never told to start the agent.
func TestGuestMonitoringNeedsTheGuestCacheServiceOnFirecracker(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withMonitoring(t, nodeCacheBlock+"  monitoring:\n    guest: true\n")))
	if err != nil {
		t.Fatalf("guest monitoring on a firecracker node with node.cache was refused: %v", err)
	}
	if !cfg.Node.Monitoring.Guest {
		t.Fatal("guest: true was read as off")
	}

	cfg, err = Load(writeConfig(t, withMonitoring(t, nodeCacheBlock+"  monitoring:\n    rapl: true\n")))
	if err != nil || cfg.Node.Monitoring.Guest {
		t.Fatalf("a block that does not name guest has guest %v, %v; want off", cfg.Node.Monitoring.Guest, err)
	}

	if got := loadErr(t, withMonitoring(t, "  monitoring:\n    guest: true\n")); !strings.Contains(got,
		"node.monitoring.guest is set but node.cache is not") {
		t.Errorf("guest monitoring without node.cache answered %q, want a refusal naming node.cache", got)
	}

	for _, provider := range []ProviderKind{ProviderDocker, ProviderTart} {
		c := &Config{Node: &NodeConfig{Provider: provider, Cache: &NodeCacheConfig{},
			Monitoring: &NodeMonitoringConfig{Guest: true}}}
		got := errors.Join(c.validateMonitoringNode()...)
		if got == nil || !strings.Contains(got.Error(), "only a firecracker guest is told") {
			t.Errorf("guest monitoring on a %s node answered %v, want a refusal", provider, got)
		}
		c.Node.Monitoring.Guest = false
		if got := errors.Join(c.validateMonitoringNode()...); got != nil {
			t.Errorf("the same %s node without guest monitoring was refused: %v", provider, got)
		}
	}
}

// A METER IS A HOST'S ENERGY SOURCE, on any backend measured from the host and
// the only one a Mac has: its base address alone, a switch a plug can have, a
// measured idle draw, and never beside RAPL.
func TestAMeterIsAdmittedOnlyWhenItCanBeRead(t *testing.T) {
	plug := func(m MeterConfig) *MeterConfig { return &m }
	good := MeterConfig{Shelly: "http://192.168.1.246", IdleWatts: 1.7}

	for _, tc := range []struct {
		name string
		m    NodeMonitoringConfig
		want string
	}{
		{"a plug", NodeMonitoringConfig{Meter: plug(good)}, ""},
		{"a plug's second switch", NodeMonitoringConfig{Meter: plug(MeterConfig{
			Shelly: "https://plug.example/", Switch: 1, IdleWatts: 1.7})}, ""},
		{"beside rapl", NodeMonitoringConfig{RAPL: true, Meter: plug(good)}, "has one source"},
		{"no address", NodeMonitoringConfig{Meter: plug(MeterConfig{IdleWatts: 1.7})}, "shelly is empty"},
		{"not http", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: "ftp://192.168.1.246",
			IdleWatts: 1.7})}, "not an http or https"},
		{"a bare host", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: "192.168.1.246",
			IdleWatts: 1.7})}, "not an http or https"},
		{"credentials", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: "http://admin:pw@192.168.1.246",
			IdleWatts: 1.7})}, "names more than the plug"},
		{"a path", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: "http://192.168.1.246/rpc",
			IdleWatts: 1.7})}, "names more than the plug"},
		{"a switch no plug has", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: good.Shelly,
			Switch: 16, IdleWatts: 1.7})}, "outside 0 to 15"},
		{"no idle draw", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: good.Shelly})},
			"not a host's idle power"},
		{"a large host's idle draw", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: good.Shelly,
			IdleWatts: 1700})}, ""},
		{"an idle draw past any host", NodeMonitoringConfig{Meter: plug(MeterConfig{Shelly: good.Shelly,
			IdleWatts: 2001})}, "not a host's idle power"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Node: &NodeConfig{Provider: ProviderTart, Monitoring: &tc.m}}
			got := errors.Join(c.validateMonitoringNode()...)
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("refused: %v", got)
			case tc.want != "" && (got == nil || !strings.Contains(got.Error(), tc.want)):
				t.Errorf("answered %v, want a refusal saying %q", got, tc.want)
			}
		})
	}
}

// A METER IS READ FROM billet.yaml as written on a firecracker node too.
func TestAMeterIsReadFromTheFile(t *testing.T) {
	cfg, err := Load(writeConfig(t, withMonitoring(t,
		"  monitoring:\n    meter:\n      shelly: http://192.168.1.245\n      idle_watts: 148\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m := cfg.Node.Monitoring.Meter; m == nil || m.Shelly != "http://192.168.1.245" || m.Switch != 0 ||
		m.IdleWatts != 148 {
		t.Fatalf("meter = %+v", m)
	}
}
