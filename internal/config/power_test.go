package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withPower is validConfig with a power block on its firecracker node.
func withPower(t *testing.T, block string) string {
	t.Helper()

	anchor := "    cache_pool: billet-cache\n"
	body := strings.Replace(validConfig, firecrackerNode,
		strings.Replace(firecrackerNode, anchor, anchor+block, 1), 1)
	if !strings.Contains(body, "power:") {
		t.Fatal("the node block in validConfig has changed, so these cases patch nothing")
	}

	return body
}

func TestAPowerBlockIsReadWithItsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withPower(t, "  power:\n    disable_boost: true\n    idle_max_freq_khz: 1500000\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Node.Power
	if p == nil || !p.DisableBoost || p.IdleMaxFreqKHz != 1500000 {
		t.Fatalf("power = %+v", p)
	}
	if d, err := p.IdleAfterDuration(); err != nil || d != DefaultPowerIdleAfter {
		t.Errorf("idle_after = %s, %v; want the %s default", d, err, DefaultPowerIdleAfter)
	}

	cfg, err = Load(writeConfig(t, withPower(t, "  power:\n    idle_after: 5m\n    disable_boost: true\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d, err := cfg.Node.Power.IdleAfterDuration(); err != nil || d != 5*time.Minute {
		t.Errorf("idle_after = %s, %v; want 5m", d, err)
	}

	// ABSENT IS OFF.
	cfg, err = Load(writeConfig(t, validConfig))
	if err != nil || cfg.Node.Power != nil {
		t.Fatalf("a node with no block has power %+v, %v", cfg.Node.Power, err)
	}
}

func TestAPowerBlockTheNodeCannotHonourIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body, want string
	}{
		{"an idle_after below the floor", withPower(t, "  power:\n    idle_after: 2s\n    disable_boost: true\n"),
			"below the 10s minimum"},
		{"an idle_after that is not a duration", withPower(t, "  power:\n    idle_after: soon\n    disable_boost: true\n"),
			"not a duration"},
		{"a block that changes nothing", withPower(t, "  power:\n    idle_after: 60s\n"), "changes nothing"},
		{"a negative ceiling", withPower(t, "  power:\n    idle_max_freq_khz: -1\n    disable_boost: true\n"),
			"idle_max_freq_khz is -1"},
		{"a cloud node", cloudConfig(t, "  max_memory: 256GiB\n", "  max_memory: 256GiB\n  power:\n    disable_boost: true\n"),
			"only a firecracker or docker node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := loadErr(t, tc.body); !strings.Contains(got, tc.want) {
				t.Fatalf("refused with %q, want it to say %q", got, tc.want)
			}
		})
	}
}

// ONLY A LINUX HOST'S OWN CPUs: docker and firecracker are admitted, a tart
// host (macOS, no cpufreq) and the remote backends are not.
func TestPowerIsAdmittedOnlyOnTheBackendsThatRunOnALinuxHost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		provider ProviderKind
		admitted bool
	}{
		{ProviderFirecracker, true},
		{ProviderDocker, true},
		{ProviderTart, false},
		{ProviderEC2, false},
		{ProviderCodeBuild, false},
	} {
		c := &Config{Node: &NodeConfig{Provider: tc.provider, Power: &NodePowerConfig{DisableBoost: true}}}
		got := errors.Join(c.validatePowerNode()...)
		switch {
		case tc.admitted && got != nil:
			t.Errorf("%s: refused: %v", tc.provider, got)
		case !tc.admitted && (got == nil || !strings.Contains(got.Error(), "node.power is set but")):
			t.Errorf("%s: answered %v, want a refusal naming node.power", tc.provider, got)
		}
	}
}
