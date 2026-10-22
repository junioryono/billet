package host

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// A CEILING THAT COVERS THE HOSTS SAYS NOTHING; ONE BELOW THEM IN EITHER
// DIMENSION SAYS BOTH SUMS AND BOTH LIMITS, and names whose hosts it counted.
func TestCeilingBelowSaysWhenTheHostsOutgrowTheCeiling(t *testing.T) {
	t.Parallel()

	server := &config.ServerConfig{MaxVCPU: 16, MaxMemory: 64 * config.GiB}

	if got := CeilingBelow("registered", 2, 16, 64*config.GiB, server); got != "" {
		t.Errorf("a ceiling equal to the hosts' sum was reported: %s", got)
	}

	for _, c := range []struct {
		name   string
		vcpu   int
		memory config.ByteSize
	}{
		{"vCPU above", 18, 64 * config.GiB},
		{"memory above", 16, 65 * config.GiB},
	} {
		got := CeilingBelow("registered", 3, c.vcpu, c.memory, server)
		if got == "" {
			t.Errorf("%s: a ceiling below the hosts was not reported", c.name)

			continue
		}
		for _, want := range []string{"BELOW THE HOSTS", "3 registered hosts", c.memory.String(), "allow 16 and 64GiB", "billet join accept"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the report does not say %q: %s", c.name, want, got)
			}
		}
	}
}

// `billet check` COMPARES THE CEILING WITH EVERY REGISTERED HOST, OFFLINE ONES
// INCLUDED AND DECOMMISSIONED ONES NOT (#183): a host down for a reboot comes
// back with its whole contribution, and a decommissioned one is out of the
// fleet until it registers again. The fixture's ceiling is 8 vCPU and 32GiB.
//
// Mutations: dropping the check's comparison loses the line; counting only live
// hosts (the placeable sum) hides the offline host's share and loses it too.
func TestCheckComparesTheCeilingWithEveryRegisteredHost(t *testing.T) {
	stateDir := ledgertest.Dir(t)
	configPath := writeAdminConfig(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	register := func(name string, vcpu int) int64 {
		t.Helper()

		epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: name, Provider: config.ProviderDocker, VCPU: vcpu, Memory: 8 * config.GiB,
		})
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", name, err)
		}

		return epoch
	}

	runCheck := func() string {
		t.Helper()

		var checkErr error
		out := capture(t, func() {
			stubGitHubUnverifiable(t)
			checkErr = Check(t.Context(), processEnv(), []string{"--config", configPath})
		})
		if checkErr != nil {
			t.Fatalf("billet check: %v", checkErr)
		}

		return out
	}

	// 4 live and 4 offline: the ceiling of 8 covers them exactly.
	register("live-1", 4)
	if err := a.NodeGone(t.Context(), "rebooting", register("rebooting", 4)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}
	// 8 more, decommissioned: out of the fleet, so not counted.
	if err := a.NodeGone(t.Context(), "retired", register("retired", 8)); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}
	if _, err := a.Decommission(t.Context(), alloc.DecommissionRequest{Node: "retired", Actor: "test", Force: true}); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	if out := runCheck(); strings.Contains(out, "BELOW THE HOSTS") {
		t.Fatalf("a ceiling that covers the registered hosts was reported below them:\n%s", out)
	}

	// One more live host: 4 + 4 + 2 = 10 vCPU against a ceiling of 8, which only
	// the offline host's share pushes over (the live hosts alone are 6).
	register("joined", 2)

	out := runCheck()
	want := "ceiling  BELOW THE HOSTS: the 3 registered hosts contribute 10 vCPU and 24GiB"
	if !strings.Contains(out, want) {
		t.Errorf("check does not report the ceiling below the registered hosts; want %q in:\n%s", want, out)
	}
}
