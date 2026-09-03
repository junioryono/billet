package config

import (
	"strings"
	"testing"
	"time"
)

// withServerKey inserts a key into the server section of validConfig.
//
// Anchored on state_dir rather than on the section header so an inserted key
// cannot land in the wrong section if the fixture is reordered.
func withServerKey(line string) string {
	return strings.Replace(validConfig,
		"  state_dir: /var/lib/billet/server",
		"  state_dir: /var/lib/billet/server\n  "+line, 1)
}

func withNodeKey(line string) string {
	return strings.Replace(validConfig,
		"  state_dir: /var/lib/billet/node",
		"  state_dir: /var/lib/billet/node\n  "+line, 1)
}

// serverDrain and nodeDrain are checked helpers.
//
// These return a duration ALONGSIDE an error, so `d, _ := …` would let every
// assertion below pass when the call fails — the vacuous-assertion trap that has
// already bitten this repository seven times.
func serverDrain(t *testing.T, c *Config) time.Duration {
	t.Helper()
	d, err := c.Server.DrainTimeoutDuration()
	if err != nil {
		t.Fatalf("server drain timeout: %v", err)
	}
	return d
}

func nodeDrain(t *testing.T, c *Config) time.Duration {
	t.Helper()
	d, err := c.Node.DrainTimeoutDuration()
	if err != nil {
		t.Fatalf("node drain timeout: %v", err)
	}
	return d
}

// A drain waits for running jobs, so its ceiling is "how long may a job run".
// Absent, that is six hours: GitHub's jobs.<id>.timeout-minutes defaults to 360.
func TestDrainTimeoutDefaultsToTheLongestAJobUsuallyRuns(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := serverDrain(t, cfg); got != 6*time.Hour {
		t.Errorf("server drain timeout = %s, want 6h", got)
	}
	if got := nodeDrain(t, cfg); got != 6*time.Hour {
		t.Errorf("node drain timeout = %s, want 6h", got)
	}
}

// The default is a DEFAULT and not a policy, which matters more here than it
// looks. node.max_custody's comment records that "self-hosted runners are
// routinely configured past GitHub's six-hour default", so an operator whose
// jobs run longer must be able to say so — otherwise a restart kills exactly the
// long job the drain exists to protect.
func TestDrainTimeoutIsConfigurablePerRole(t *testing.T) {
	cfg, err := Load(writeConfig(t, withServerKey("drain_timeout: 90m")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := serverDrain(t, cfg); got != 90*time.Minute {
		t.Errorf("server drain timeout = %s, want 90m", got)
	}
	// Separate keys: a node and a control plane are restarted for different
	// reasons and need not wait the same amount of time.
	if got := nodeDrain(t, cfg); got != 6*time.Hour {
		t.Errorf("setting the server's timeout changed the node's: %s", got)
	}

	cfg, err = Load(writeConfig(t, withNodeKey("drain_timeout: 30m")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := nodeDrain(t, cfg); got != 30*time.Minute {
		t.Errorf("node drain timeout = %s, want 30m", got)
	}
	if got := serverDrain(t, cfg); got != 6*time.Hour {
		t.Errorf("setting the node's timeout changed the server's: %s", got)
	}
}

// A LONG VALUE IS ACCEPTED, AND IT HAS TO BE LONGER THAN THE CEILING THAT WAS
// REMOVED.
//
// This asserted 24h, which the old `d > maxDrainTimeout` check ACCEPTED — so
// restoring that ceiling would leave the test green while the thing it exists to
// prove was gone. A week is past any bound billet ever had, on both keys, because
// the value now only decides when billet starts reporting a long drain.
func TestDrainTimeoutAcceptsALongButDeliberateWait(t *testing.T) {
	const aWeek = 168 * time.Hour

	cfg, err := Load(writeConfig(t, withServerKey("drain_timeout: 168h")))
	if err != nil {
		t.Fatalf("Load rejected a week: %v", err)
	}
	if got := serverDrain(t, cfg); got != aWeek {
		t.Errorf("server drain timeout = %s, want %s", got, aWeek)
	}

	// THE NODE'S KEY TOO. Both drains are unbounded and both read this value as a
	// reporting threshold, and a ceiling restored on only one of them would leave
	// the other's test green.
	nodeCfg, err := Load(writeConfig(t, withNodeKey("drain_timeout: 168h")))
	if err != nil {
		t.Fatalf("Load rejected a week on the node's key: %v", err)
	}

	if got := nodeDrain(t, nodeCfg); got != aWeek {
		t.Errorf("node drain timeout = %s, want %s", got, aWeek)
	}
}

// Each case asserts the DIAGNOSTIC, not merely that an error occurred: counting
// errors stays green when every one of them is the wrong error.
func TestDrainTimeoutIsValidated(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		// Zero is refused rather than read as "use the default". An operator who
		// writes 0s means something, and silently reporting after six hours
		// instead is the kind of quiet divergence between the file and the
		// behaviour that this validation exists to prevent. The message must name
		// what does stop the wait, or refusing it is only an obstacle.
		//
		// THERE IS NO CEILING TO TEST. A 24h maximum was right while this bounded
		// how long billet waited before DESTROYING work; it now bounds only when
		// billet starts reporting an overrunning drain, so an implausibly large
		// value costs a quieter log and nothing else.
		{"0s", "second signal"},
		{"-1h", "must be positive"},
		// A bare number is the commonest mistake, and time.ParseDuration rejects it
		// with "missing unit in duration" — true but not actionable, so the message
		// has to show the shape that works.
		{"90", "such as \"90m\""},
		{"soon", "such as \"90m\""},
	} {
		_, err := Load(writeConfig(t, withServerKey("drain_timeout: "+tc.value)))
		if err == nil {
			t.Errorf("Load accepted server.drain_timeout: %s", tc.value)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("server.drain_timeout: %s\n got: %v\nwant it to contain: %q", tc.value, err, tc.want)
		}

		// The node's key is validated by the same rules. Validating only the
		// server's would leave a node that cannot start, diagnosed on the far side
		// of the deployment from the file that caused it.
		//
		// Asserting the DIAGNOSTIC here too, not merely that an error occurred: the
		// fixture has plenty of other ways to fail validation, so "some error"
		// would stay green if node.drain_timeout stopped being checked at all.
		_, err = Load(writeConfig(t, withNodeKey("drain_timeout: "+tc.value)))
		if err == nil {
			t.Errorf("Load accepted node.drain_timeout: %s", tc.value)
			continue
		}

		if !strings.Contains(err.Error(), "node.drain_timeout") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("node.drain_timeout: %s\n got: %v\nwant it to name node.drain_timeout and contain: %q",
				tc.value, err, tc.want)
		}
	}
}
