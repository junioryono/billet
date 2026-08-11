package server

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

func TestDrainGraceDefaultsToTheLongestAJobUsuallyRuns(t *testing.T) {
	l := NewListener(nil, "tier", nil)
	if l.drainGrace != 6*time.Hour {
		t.Errorf("drain grace = %s, want 6h", l.drainGrace)
	}
}

func TestDrainGraceIsConfigurable(t *testing.T) {
	l := NewListener(nil, "tier", nil, WithDrainGrace(90*time.Minute))
	if l.drainGrace != 90*time.Minute {
		t.Errorf("drain grace = %s, want 90m", l.drainGrace)
	}
	if err := l.configError(); err != nil {
		t.Errorf("a valid drain grace was refused: %v", err)
	}
}

// The drain has its OWN ceiling, and this is the assertion that proves it is not
// quietly sharing the teardown's.
//
// maxGrace is an hour, because a teardown that takes an hour is already broken.
// A drain waits for a JOB, and jobs legitimately run longer than that — so
// reusing checkGrace here would refuse every honest value above 1h and force
// operators back onto the behaviour the drain replaced.
func TestADrainMayWaitFarLongerThanATeardownMay(t *testing.T) {
	if maxGrace >= 24*time.Hour {
		t.Fatalf("maxGrace is %s, so this test can no longer tell the two ceilings apart", maxGrace)
	}

	l := NewListener(nil, "tier", nil, WithDrainGrace(12*time.Hour))
	if err := l.configError(); err != nil {
		t.Fatalf("a 12h drain was refused, which means it is being checked against the teardown's ceiling: %v", err)
	}
	if l.drainGrace != 12*time.Hour {
		t.Errorf("drain grace = %s, want 12h", l.drainGrace)
	}

	// And the teardown's ceiling is unchanged: a 12h SHUTDOWN grace is still
	// refused. Raising the drain's ceiling must not raise everything else's.
	teardown := NewListener(nil, "tier", nil, WithShutdownGrace(12*time.Hour))
	if err := teardown.configError(); err == nil {
		t.Error("a 12h shutdown grace was accepted; the drain's ceiling leaked into the teardown's")
	}
}

func TestDrainGraceIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"zero drain grace", WithDrainGrace(0)},
		{"negative drain grace", WithDrainGrace(-time.Second)},
		{"a drain longer than any job", WithDrainGrace(25 * time.Hour)},
		{"an overflowing drain grace", WithDrainGrace(time.Duration(1) << 62)},
	} {
		l := NewListener(nil, "tier", nil, tc.opt)
		err := l.configError()
		if err == nil {
			t.Errorf("%s was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "drain grace") {
			t.Errorf("%s: error should name the field, got: %v", tc.name, err)
		}
	}
}

// A refused value must not be applied, or the listener runs on a number nobody
// configured. The existing graces behave this way and the drain has to match.
func TestARefusedDrainGraceLeavesTheDefaultInPlace(t *testing.T) {
	l := NewListener(nil, "tier", nil, WithDrainGrace(-time.Second))
	if l.drainGrace != 6*time.Hour {
		t.Errorf("a refused drain grace was applied: %s", l.drainGrace)
	}
}

// The whole point of the config key: what an operator writes in billet.yaml is
// what the listener waits.
//
// Started from a real ServerConfig rather than from a duration literal, so the
// whole chain is under test — the YAML value, the parse, the control-plane
// option, and the listener it configures. A test of the option alone passes
// happily while the value never leaves the config file.
func TestTheConfiguredDrainTimeoutReachesEveryListener(t *testing.T) {
	cfg := &config.ServerConfig{DrainTimeout: "90m"}

	d, err := cfg.DrainTimeoutDuration()
	if err != nil {
		t.Fatalf("DrainTimeoutDuration: %v", err)
	}

	s := New(nil, nil, nil, "owner", nil, WithDrainTimeout(d))

	l := NewListener(nil, "tier", nil, s.listenerOpts()...)
	if l.drainGrace != 90*time.Minute {
		t.Errorf("listener drain grace = %s, want the configured 90m", l.drainGrace)
	}
}

func TestListenersDefaultToTheDrainDefaultWhenUnconfigured(t *testing.T) {
	s := New(nil, nil, nil, "owner", nil)

	l := NewListener(nil, "tier", nil, s.listenerOpts()...)
	if l.drainGrace != 6*time.Hour {
		t.Errorf("listener drain grace = %s, want the 6h default", l.drainGrace)
	}
}
