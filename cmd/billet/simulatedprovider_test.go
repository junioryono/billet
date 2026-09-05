package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE CLI NEVER BUILDS THE SIMULATED BACKEND.
//
// config.Load refuses the kind, so this is unreachable from a file; it is asserted
// anyway because newProvider is the one place a kind becomes compute, and the
// fallback for a kind the switch does not name is a generic "unknown provider"
// that says nothing about why. The config is built in code because Load will
// not hand one over.
func TestTheSimulatedBackendIsNeverConstructedByTheCLI(t *testing.T) {
	t.Parallel()

	_, err := newProvider(&config.Config{Node: &config.NodeConfig{
		Name:     "sim-1",
		Provider: config.ProviderSimulated,
	}}, "deployment-1")
	if err == nil {
		t.Fatal("the CLI built a simulated backend; a host running it reports every job " +
			"finished and runs none")
	}

	if !strings.Contains(err.Error(), "test harness") {
		t.Errorf("the refusal does not say the backend is test-only: %v", err)
	}

	if strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("the simulated backend fell through as an unknown kind: %v", err)
	}
}
