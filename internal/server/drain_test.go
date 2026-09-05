package server

import (
	"os"
	"path/filepath"
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
		// NO UPPER BOUND ANY MORE. A ceiling was right while this decided how long
		// billet waited before DESTROYING the jobs still running: past a day a typo
		// was likelier than the intent, and believing one cost somebody a build a
		// day later. It now decides only when billet starts REPORTING an
		// overrunning drain, so an implausible value costs a quieter log — and a
		// listener that refused to start over it would be refusing an operator's
		// config to protect nothing.
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

// AND A LARGE ONE IS ACCEPTED RATHER THAN REFUSED, which is the other half of
// removing the ceiling.
//
// Dropping the "a drain longer than any job" case above only proves nothing
// refuses it any more if something also proves the value ARRIVES. Without this,
// a listener that silently discarded every large value and ran on the default
// would pass the shortened table.
func TestALongDrainGraceIsAcceptedNowThatItOnlyDecidesWhenBilletTalks(t *testing.T) {
	const aWeek = 168 * time.Hour

	l := NewListener(nil, "tier", nil, WithDrainGrace(aWeek))

	if err := l.configError(); err != nil {
		t.Fatalf("a drain grace of %s was refused: %v", aWeek, err)
	}

	if l.drainGrace != aWeek {
		t.Errorf("drain grace is %s, want %s; a value accepted and then discarded is a "+
			"listener running on a number nobody configured", l.drainGrace, aWeek)
	}
}

// The whole point of the config key: what an operator writes in billet.yaml is
// what the listener waits.
//
// Started from real YAML on disk, through config.Load, so every link is under
// test — the key, its parse, the control-plane option, and the listener it
// configures. Building a config struct by hand instead would skip the parse and
// leave the comment claiming more than the test does.
func TestTheConfiguredDrainTimeoutReachesEveryListener(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want time.Duration
	}{
		{"configured", "\n  drain_timeout: 90m", 90 * time.Minute},
		{"absent", "", 6 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfig(t, tc.key)

			opts, err := OptionsFromConfig(cfg)
			if err != nil {
				t.Fatalf("OptionsFromConfig: %v", err)
			}

			s := New(nil, nil, nil, "owner", nil, opts...)

			l := NewListener(nil, "tier", nil, s.listenerOpts(s.prov)...)
			if l.drainGrace != tc.want {
				t.Errorf("listener drain grace = %s, want %s", l.drainGrace, tc.want)
			}

			if err := l.configError(); err != nil {
				t.Errorf("a config-derived drain grace was refused: %v", err)
			}
		})
	}
}

// An explicitly bad control-plane value must REACH the listener and be refused
// there, not be filtered out on the way and replaced by a default.
//
// Guarding the hand-off on `> 0` looks like defensiveness and is the opposite:
// it turns an operator's mistake into a silent six-hour wait, which is the
// substitution checkGrace exists to refuse. Found by review, not by the tests
// above, which is why it has its own.
func TestAnExplicitlyBadDrainTimeoutIsRefusedRatherThanDefaulted(t *testing.T) {
	// 25h is no longer here: the ceiling went with the deadline, because the value
	// decides when billet REPORTS a long drain rather than when it destroys the
	// work. Zero and negative are still refusals — they are not a threshold at all.
	for _, d := range []time.Duration{0, -time.Second} {
		s := New(nil, nil, nil, "owner", nil, WithDrainTimeout(d))

		l := NewListener(nil, "tier", nil, s.listenerOpts(s.prov)...)
		if err := l.configError(); err == nil {
			t.Errorf("WithDrainTimeout(%s) was silently replaced by the default %s", d, l.drainGrace)
		}
	}
}

// loadConfig writes a minimal server config with an optional extra server key
// and loads it the way billet does.
func loadConfig(t *testing.T, serverKey string) *config.Config {
	t.Helper()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 64GiB` + serverKey + `
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/app.pem
tiers:
  - label: billet-4vcpu-a
    provider: docker
    vcpu: 4
    memory: 16GiB
    disk: 80GiB
    image: ghcr.io/example/runner:latest
    command: ["./run.sh"]
`

	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return cfg
}
