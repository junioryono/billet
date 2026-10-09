package app

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// jobTargetsConfig serves two owners: acme by organization and one of
// someone's repositories, with a tier on each.
func jobTargetsConfig() *config.Config {
	return &config.Config{
		GitHub:  &config.GitHubConfig{Org: "acme"},
		Targets: []config.GitHubConfig{{Name: "personal", Repository: "someone/tool"}},
		Tiers: []config.Tier{
			{Label: "acme-2vcpu", Target: config.DefaultTargetName},
			{Label: "tool-2vcpu", Target: "personal"},
		},
	}
}

// THE TARGET THAT READS A JOB IS ITS TIER'S, and only if that target holds the
// job's repository; a tier no longer declared falls back to the one target
// holding the repository, and none or two is refused, never guessed.
func TestAJobIsReadWithTheAppThatServedIt(t *testing.T) {
	t.Parallel()

	two := jobTargetsConfig()
	two.Targets = append(two.Targets, config.GitHubConfig{Name: "acme-too", Org: "ACME"})

	for name, tc := range map[string]struct {
		cfg              *config.Config
		tier, repository string
		want, refusal    string
	}{
		"the tier's organization":      {jobTargetsConfig(), "acme-2vcpu", "Acme/api", config.DefaultTargetName, ""},
		"the tier's repository":        {jobTargetsConfig(), "tool-2vcpu", "SOMEONE/Tool", "personal", ""},
		"a repository the tier lacks":  {jobTargetsConfig(), "tool-2vcpu", "someone/other", "", "does not hold"},
		"another owner's repository":   {jobTargetsConfig(), "acme-2vcpu", "evil/api", "", "does not hold"},
		"a removed tier, one holder":   {jobTargetsConfig(), "gone", "acme/api", config.DefaultTargetName, ""},
		"a removed tier, no holder":    {jobTargetsConfig(), "gone", "evil/api", "", "no target holds"},
		"a removed tier, two holders":  {two, "gone", "acme/api", "", "refusing to guess"},
		"a repository with no owner":   {jobTargetsConfig(), "acme-2vcpu", "api", "", "not owner/name"},
		"a tier naming no real target": {&config.Config{GitHub: &config.GitHubConfig{Org: "acme"}, Tiers: []config.Tier{{Label: "x", Target: "nowhere"}}}, "x", "acme/api", "", "names no target"},
	} {
		got, err := jobTarget(tc.cfg, tc.tier, tc.repository)
		switch {
		case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
			t.Errorf("%s: %v, %v, want refused with %q", name, got.Name, err, tc.refusal)
		case tc.refusal == "" && (err != nil || got.Name != tc.want):
			t.Errorf("%s: %q, %v, want %q", name, got.Name, err, tc.want)
		}
	}
}
