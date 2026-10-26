package alloc

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// AN UNEXPANDED MULTI-TARGET DECLARATION IS REFUSED BY NAME: New cannot prove
// its catalogue came through config.Parse, and as written the tier is one tier
// for several targets.
func TestNewRefusesAnUnexpandedMultiTargetTier(t *testing.T) {
	db := openTestLedger(t)

	declared := tier("${target}-1vcpu", 1, 4*config.GiB)
	declared.Targets = config.TierTargets{Names: []string{"a", "b"}}

	_, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 64 * config.GiB}, []config.Tier{declared})
	if err == nil || !strings.Contains(err.Error(), "still carries `targets`") {
		t.Errorf("New = %v, want the unexpanded declaration refused by name", err)
	}

	if _, err := config.ExpandTierTargets([]config.Tier{declared}, []string{"a", "b"}); err == nil {
		t.Fatal("ExpandTierTargets accepted a declaration with no runs_on")
	}

	declared.RunsOn = "billet-1vcpu"

	expanded, err := config.ExpandTierTargets([]config.Tier{declared}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("ExpandTierTargets: %v", err)
	}

	if _, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 64 * config.GiB}, expanded); err != nil {
		t.Errorf("New refused the expanded catalogue: %v", err)
	}
}
