package config

import (
	"strings"
	"testing"
	"time"
)

// A LOG IS KEPT ONLY WHERE THE AGENT THAT READS IT RUNS, and a setting that quietly did nothing
// would read as logs that exist. Both directions: a Linux Firecracker tier asks, and each other
// shape is refused.
func TestLogsKeepNeedsALinuxFirecrackerTier(t *testing.T) {
	t.Parallel()

	keep := &TierLogs{Keep: true}

	for name, tc := range map[string]struct {
		tier Tier
		ok   bool
	}{
		"firecracker":   {Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Logs: keep}, true},
		"no word":       {Tier{Provider: ProviderDocker, GuestOS: GuestLinux}, true},
		"off":           {Tier{Provider: ProviderDocker, GuestOS: GuestLinux, Logs: &TierLogs{}}, true},
		"docker":        {Tier{Provider: ProviderDocker, GuestOS: GuestLinux, Logs: keep}, false},
		"a macOS guest": {Tier{Provider: ProviderTart, GuestOS: GuestMacOS, Logs: keep}, false},
		"two backends":  {Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2}, GuestOS: GuestLinux, Logs: keep}, false},
	} {
		errs := tc.tier.validateLogs("tiers[0]")
		if (len(errs) == 0) != tc.ok {
			t.Errorf("%s: %v, want accepted %t", name, errs, tc.ok)
		}
		if len(errs) > 0 && !strings.Contains(errs[0].Error(), "logs.keep") {
			t.Errorf("%s: the refusal does not name the key: %v", name, errs[0])
		}
	}
}

// A TIER'S WORD REACHES A NODE IN WHAT THE TIER'S CACHE RESOLVES TO, and is off unless asked.
func TestATiersWordReachesItsResolvedCache(t *testing.T) {
	t.Parallel()

	tier := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux}
	if tier.EffectiveCache().KeepLogs {
		t.Error("a tier that did not ask keeps its logs")
	}

	tier.Logs = &TierLogs{Keep: true}
	if !tier.EffectiveCache().KeepLogs {
		t.Error("a tier that asked does not keep its logs")
	}
}

func TestLogLimitsHaveDefaultsAndBounds(t *testing.T) {
	t.Parallel()

	var (
		noServer *ServerLogsConfig
		noNode   *NodeLogsConfig
	)

	server, err := noServer.Limits()
	if err != nil || server.Retention != DefaultLogRetention || server.MaxBytes != DefaultLogIndexMaxBytes {
		t.Errorf("no server.logs is %+v, %v", server, err)
	}

	node, err := noNode.Limits()
	if err != nil || node.Retention != DefaultLogRetention || node.MaxBytes != DefaultNodeLogMaxBytes ||
		node.MaxJobBytes != DefaultNodeLogJobMaxBytes {
		t.Errorf("no node.logs is %+v, %v", node, err)
	}

	set, err := (&ServerLogsConfig{Retention: "48h", MaxBytes: 64 * MiB}).Limits()
	if err != nil || set.Retention != 48*time.Hour || set.MaxBytes != 64*MiB {
		t.Errorf("a set server.logs is %+v, %v", set, err)
	}

	for name, tc := range map[string]struct {
		server *ServerLogsConfig
		node   *NodeLogsConfig
		says   string
	}{
		"server retention":      {server: &ServerLogsConfig{Retention: "59m"}, says: "server.logs.retention"},
		"server words":          {server: &ServerLogsConfig{Retention: "14d"}, says: "not a duration"},
		"server bound":          {server: &ServerLogsConfig{MaxBytes: MiB}, says: "server.logs.max_bytes"},
		"node retention":        {node: &NodeLogsConfig{Retention: "59m"}, says: "node.logs.retention"},
		"node bound":            {node: &NodeLogsConfig{MaxBytes: MiB}, says: "node.logs.max_bytes"},
		"node job":              {node: &NodeLogsConfig{MaxJobBytes: KiB}, says: "node.logs.max_job_bytes"},
		"a job past the node's": {node: &NodeLogsConfig{MaxBytes: 64 * MiB, MaxJobBytes: 128 * MiB}, says: "evict"},
	} {
		var err error

		if tc.server != nil {
			_, err = tc.server.Limits()
		} else {
			_, err = tc.node.Limits()
		}

		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, tc.says)
		}
	}
}

// A BAD VALUE IS REFUSED WHEN THE FILE IS READ, not when the retention first runs.
func TestLogSettingsAreCheckedOnLoad(t *testing.T) {
	t.Parallel()

	if _, err := Load(writeConfig(t, withServerKey("logs: {retention: 59m}"))); err == nil ||
		!strings.Contains(err.Error(), "server.logs.retention") {
		t.Errorf("a retention under an hour on load: %v", err)
	}

	cfg, err := Load(writeConfig(t, withServerKey("logs: {retention: 48h, max_bytes: 1GiB}")))
	if err != nil {
		t.Fatalf("a valid server.logs: %v", err)
	}
	if got, err := cfg.Server.Logs.Limits(); err != nil || got.Retention != 48*time.Hour || got.MaxBytes != GiB {
		t.Errorf("a valid server.logs reads as %+v, %v", got, err)
	}
}
