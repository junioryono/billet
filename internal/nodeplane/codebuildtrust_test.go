package nodeplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func codebuildTier(label string, trust config.WorkloadTrust) config.Tier {
	return config.Tier{Label: label, Trust: trust, Providers: []config.ProviderKind{config.ProviderCodeBuild},
		GuestOS: config.GuestLinux, VCPU: 4, Memory: 16 * config.GiB, Image: "aws/codebuild/amazonlinux-x86_64-standard:5.0"}
}

// THE SAME RULE WHERE THE CATALOGUE LIVES: a codebuild node the tiers would use
// for both trust classes is refused at registration, because the node's file
// may be the node's alone and load-time is not the enforcement point.
func TestACodeBuildNodeSharedByBothTrustClassesCannotRegister(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithTierCatalog([]config.Tier{
		codebuildTier("cb-trusted", config.WorkloadTrusted),
		codebuildTier("cb-untrusted", config.WorkloadUntrusted),
	}))
	req := registerAt("")
	req.Provider = config.ProviderCodeBuild

	_, err := p.Register(t.Context(), req)
	if err == nil || !errors.Is(err, ErrRefused) {
		t.Fatalf("registration error = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "cb-trusted") || !strings.Contains(err.Error(), "cb-untrusted") {
		t.Errorf("refusal does not name both tiers: %v", err)
	}

	// One class alone registers, or the refusal above proves nothing.
	p = testPlane(t, WithTierCatalog([]config.Tier{
		codebuildTier("cb-untrusted", config.WorkloadUntrusted),
		codebuildTier("cb-untrusted-2", config.WorkloadUntrusted),
	}))
	if _, err := p.Register(t.Context(), req); err != nil {
		t.Fatalf("a single-class codebuild node was refused: %v", err)
	}
}
