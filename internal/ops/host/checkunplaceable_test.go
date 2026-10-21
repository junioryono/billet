package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// checkWithHosts runs billet check over the admin config's one docker tier
// (2 vCPU, 8 GiB) with these hosts registered live, and returns what it printed.
func checkWithHosts(t *testing.T, hosts ...alloc.NodeRegistration) string {
	t.Helper()

	stateDir := ledgertest.Dir(t)
	configPath := writeAdminConfig(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	for i := range hosts {
		if _, err := a.RegisterNode(t.Context(), hosts[i]); err != nil {
			t.Fatalf("RegisterNode(%s): %v", hosts[i].Name, err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var checkErr error

	out := capture(t, func() {
		stubGitHubUnverifiable(t)
		checkErr = Check(ctx, processEnv(), []string{"--config", configPath})
	})
	if checkErr != nil {
		t.Fatalf("billet check: %v", checkErr)
	}

	return out
}

// A TIER NO LIVE HOST CAN PLACE IS NAMED WHERE IT IS LISTED (#140). It
// advertises and is assigned work all the same, and its jobs wait however much
// else is free; nothing said so until they had queued. A host too small for
// the shape, and one running another provider, place it nowhere.
func TestCheckNamesATierNoLiveHostCanPlace(t *testing.T) {
	out := checkWithHosts(t,
		alloc.NodeRegistration{Name: "tiny", Provider: config.ProviderDocker, VCPU: 1, Memory: 4 * config.GiB},
		alloc.NodeRegistration{Name: "vm-host", Provider: config.ProviderFirecracker, VCPU: 64, Memory: 256 * config.GiB},
	)

	if !strings.Contains(out, "UNPLACEABLE: no live host can place this shape") {
		t.Errorf("check does not say the 2 vCPU docker tier fits no live host:\n%s", out)
	}

	// A docker host the shape fits is the difference.
	fits := checkWithHosts(t,
		alloc.NodeRegistration{Name: "roomy", Provider: config.ProviderDocker, VCPU: 4, Memory: 16 * config.GiB},
	)

	if strings.Contains(fits, "UNPLACEABLE") {
		t.Errorf("check called a tier unplaceable with a live host its shape fits:\n%s", fits)
	}

	// AND WITH NO HOST AT ALL IT SAYS NOTHING NEW: every tier would say it, and
	// the fleet line has said why.
	if empty := checkWithHosts(t); strings.Contains(empty, "UNPLACEABLE") {
		t.Errorf("check called a tier unplaceable on a fleet with no host registered:\n%s", empty)
	}
}
