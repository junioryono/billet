package config

import (
	"strings"
	"testing"
)

func codebuildTier(label string, trust WorkloadTrust, node string) Tier {
	return Tier{Label: label, Trust: trust, Providers: []ProviderKind{ProviderCodeBuild}, Node: node}
}

// A CODEBUILD NODE SERVES ONE TRUST CLASS, and a tier reaches a node by pinning
// it or by pinning nothing. Two unpinned tiers of different classes share every
// node; a pinned tier shares its node with every unpinned one.
func TestCodeBuildTrustConflictSeesEveryWayTwoClassesShareANode(t *testing.T) {
	t.Parallel()

	trustedAny := codebuildTier("trusted-any", WorkloadTrusted, "")
	untrustedAny := codebuildTier("untrusted-any", WorkloadUntrusted, "")
	trustedA := codebuildTier("trusted-a", WorkloadTrusted, "cb-a")
	untrustedB := codebuildTier("untrusted-b", WorkloadUntrusted, "cb-b")
	firecracker := Tier{Label: "fc", Trust: WorkloadUntrusted, Provider: ProviderFirecracker}

	for _, tc := range []struct {
		name  string
		tiers []Tier
		node  string
		want  string
	}{
		{"two unpinned classes share every node", []Tier{trustedAny, untrustedAny}, "", "every codebuild node"},
		{"an unpinned trusted tier reaches a pinned untrusted node", []Tier{trustedAny, untrustedB}, "cb-b", `codebuild node "cb-b"`},
		{"pinned to different nodes is fine", []Tier{trustedA, untrustedB}, "cb-a", ""},
		{"pinned to different nodes is fine, the other node", []Tier{trustedA, untrustedB}, "cb-b", ""},
		{"a node nobody pins with one unpinned class is fine", []Tier{trustedAny, untrustedB}, "", ""},
		{"a firecracker tier is not a codebuild tier", []Tier{trustedAny, firecracker}, "", ""},
		{"one class alone is fine", []Tier{untrustedAny, untrustedB}, "cb-b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := CodeBuildTrustConflict(tc.tiers, tc.node)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Fatal("not refused")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal %q does not name %q", err, tc.want)
			}
			if err != nil && (!strings.Contains(err.Error(), "trusted tier(s) trusted") ||
				!strings.Contains(err.Error(), "untrusted tier(s) untrusted")) {
				t.Errorf("refusal does not name both sides: %v", err)
			}
		})
	}
}

// AND LOAD REFUSES IT WHERE THE FILE IS WRITTEN. The positive case is what proves
// the fixture loads at all, or the negative one passes for any reason.
func TestLoadRefusesACodeBuildNodeSharedByBothTrustClasses(t *testing.T) {
	body := codeBuildConfig(t, "", "")
	head, _, ok := strings.Cut(body, "tiers:\n")
	if !ok {
		t.Fatal("validConfig has no tiers block")
	}
	tier := func(label string, trust string, workflows bool) string {
		s := "  - label: " + label + "\n" +
			"    trust: " + trust + "\n" +
			"    provider: codebuild\n" +
			"    vcpu: 4\n" +
			"    memory: 4GiB\n" +
			"    image: aws/codebuild/amazonlinux-x86_64-standard:5.0\n"
		if workflows {
			s += "    workflows: [\".github/workflows/ci.yml\"]\n"
		}
		return s
	}

	both := head + "tiers:\n" + tier("cb-trusted", "trusted", true) + tier("cb-untrusted", "untrusted", false)
	got := loadErr(t, both)
	if !strings.Contains(got, `trusted tier(s) cb-trusted and untrusted tier(s) cb-untrusted would share`) {
		t.Errorf("a codebuild node shared by both trust classes was not refused as such: %s", got)
	}

	one := head + "tiers:\n" + tier("cb-untrusted", "untrusted", false) + tier("cb-untrusted-2", "untrusted", false)
	if _, err := Load(writeConfig(t, one)); err != nil {
		t.Fatalf("two untrusted codebuild tiers did not load, so the refusal above proves nothing: %v", err)
	}
}
