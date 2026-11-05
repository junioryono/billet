package config

import (
	"strings"
	"testing"
)

// withRetry is validConfig with one tier's retry block written out.
func withRetry(block string) string {
	return strings.Replace(validConfig,
		"  - label: billet-8vcpu-ubuntu-2404\n",
		"  - label: billet-8vcpu-ubuntu-2404\n"+block, 1)
}

// RETRY.DISRUPTED IS A SMALL COUNT, AND ABSENT IS NEVER. Both ends of the bound
// are held in both directions: a negative reads as no limit to anything comparing
// against it, and a large one hides a fleet that keeps ending its jobs.
func TestRetryDisruptedIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        int
		refused     string
	}{
		{name: "absent is never", block: ""},
		{name: "zero is never", block: "    retry: {disrupted: 0}\n"},
		{name: "one", block: "    retry: {disrupted: 1}\n", want: 1},
		{name: "the most", block: "    retry: {disrupted: 5}\n", want: MaxDisruptedReruns},
		{name: "past the most", block: "    retry: {disrupted: 6}\n", refused: "retry.disrupted 6 is not between 0 and 5"},
		{name: "negative", block: "    retry: {disrupted: -1}\n", refused: "retry.disrupted -1 is not between 0 and 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, withRetry(tc.block)))
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("Load = %v, want a refusal saying %q", err, tc.refused)
				}

				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			tier, ok := cfg.TierByLabel("billet-8vcpu-ubuntu-2404")
			if !ok {
				t.Fatal("the tier is gone")
			}
			if got := tier.DisruptedReruns(); got != tc.want {
				t.Errorf("DisruptedReruns = %d, want %d", got, tc.want)
			}
		})
	}
}

// THE RULE IS THE EXPORTED ONE alloc.New CALLS, and it sees a tier that never came
// through Load. That alloc.New does is held by alloc's own tests.
func TestRetryErrorsJudgesATierThatNeverWentThroughLoad(t *testing.T) {
	t.Parallel()

	if errs := (Tier{}).RetryErrors("tiers[0]"); len(errs) != 0 {
		t.Errorf("a tier saying nothing = %v", errs)
	}
	if errs := (Tier{Retry: &TierRetry{Disrupted: -2}}).RetryErrors("tiers[0]"); len(errs) != 1 ||
		!strings.Contains(errs[0].Error(), "tiers[0]") {
		t.Errorf("a negative count = %v, want one refusal naming the tier", errs)
	}
}

// ACTIONS: WRITE IS ASKED OF THE TARGET WHOSE TIER RE-RUNS, and only of it: each
// target has its own App, and one target's policy must not make another's minimal
// App look deficient.
func TestOnlyATierThatRerunsAsksItsTargetForActionsWrite(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Targets: []GitHubConfig{
			{Name: "api", Org: "acme", AppID: 1, InstallationID: 1, PrivateKeyPath: "/k1"},
			{Name: "web", Org: "other", AppID: 2, InstallationID: 2, PrivateKeyPath: "/k2"},
			{Name: "idle", Org: "third", AppID: 3, InstallationID: 3, PrivateKeyPath: "/k3"},
		},
		Tiers: []Tier{
			{Label: "a", Target: "api", Retry: &TierRetry{Disrupted: 1}},
			{Label: "b", Target: "web", Retry: &TierRetry{}},
			{Label: "c", Target: "web"},
		},
	}

	for name, want := range map[string]bool{"api": true, "web": false, "idle": false, "missing": false} {
		if got := cfg.TargetNeedsActionsWrite(name); got != want {
			t.Errorf("TargetNeedsActionsWrite(%q) = %v, want %v", name, got, want)
		}
	}
}
