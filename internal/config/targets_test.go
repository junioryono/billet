package config

import (
	"net/url"
	"strings"
	"testing"
)

// repositoryConfig is validConfig with its one target a repository.
func repositoryConfig(t *testing.T) string {
	t.Helper()

	const org = "  org: acme\n"
	if !strings.Contains(validConfig, org) {
		t.Fatal("the fixture's github block has changed, so this case patches nothing")
	}

	return strings.Replace(validConfig, org, "  repository: acme/widgets\n", 1)
}

// twoTargetConfig is validConfig with a second, repository target and every
// tier naming one of the two.
func twoTargetConfig(t *testing.T, tierTarget string) string {
	t.Helper()

	body := validConfig + `
targets:
  - name: personal
    repository: someone/widgets
    app_id: 777
    installation_id: 888
    private_key_path: /etc/billet/app-personal.pem
`

	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"
	if !strings.Contains(body, tier) {
		t.Fatal("the fixture's first tier has changed, so this case patches nothing")
	}

	if tierTarget != "" {
		body = strings.Replace(body, tier, tier+"    target: "+tierTarget+"\n", 1)
	}

	return body
}

func TestARepositoryTargetLoadsAsOne(t *testing.T) {
	cfg, err := Load(writeConfig(t, repositoryConfig(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	targets := cfg.GitHubTargets()
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}

	target := targets[0]

	if target.Name != DefaultTargetName || !target.IsRepository() ||
		target.Scope() != ScopeRepository || target.Owner() != "acme" ||
		target.RepositoryName() != "widgets" || target.Path() != "acme/widgets" ||
		target.Where() != "github" {
		t.Errorf("the repository target resolved as %+v", target)
	}

	if target.KeyName("github-app-key") != "github-app-key" {
		t.Errorf("the default target's key name is %q", target.KeyName("github-app-key"))
	}

	for _, tier := range cfg.Tiers {
		if tier.Target != DefaultTargetName {
			t.Errorf("tier %s resolved to target %q, want the only target", tier.Label, tier.Target)
		}
	}
}

func TestAnOrganizationTargetIsOne(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	target := cfg.GitHubTargets()[0]
	if target.IsRepository() || target.Scope() != ScopeOrganization || target.Owner() != "acme" ||
		target.RepositoryName() != "" || target.Path() != "acme" {
		t.Errorf("the organization target resolved as %+v", target)
	}
}

func TestATargetIsExactlyOneOfOrgAndRepository(t *testing.T) {
	both := strings.Replace(validConfig, "  org: acme\n", "  org: acme\n  repository: acme/widgets\n", 1)

	_, err := Load(writeConfig(t, both))
	if err == nil {
		t.Fatal("Load accepted a target that is both an organization and a repository")
	}

	if !strings.Contains(err.Error(), "exactly one of them") {
		t.Errorf("the error does not say a target is one or the other: %v", err)
	}

	neither := strings.Replace(validConfig, "  org: acme\n", "", 1)

	_, err = Load(writeConfig(t, neither))
	if err == nil {
		t.Fatal("Load accepted a target naming nothing")
	}

	for _, want := range []string{"github.org is required", "github.repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
}

func TestTheGitHubBlockCarriesNoName(t *testing.T) {
	body := strings.Replace(validConfig, "  org: acme\n", "  name: main\n  org: acme\n", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted github.name")
	}

	if !strings.Contains(err.Error(), "github.name is not a field") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTwoTargetsLoadWhenEveryTierNamesOne(t *testing.T) {
	body := twoTargetConfig(t, "personal")
	body = strings.Replace(body, "  - label: billet-8vcpu-ubuntu-2404\n",
		"  - label: billet-8vcpu-ubuntu-2404\n    target: default\n", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	targets := cfg.GitHubTargets()
	if len(targets) != 2 || targets[0].Name != DefaultTargetName || targets[1].Name != "personal" {
		t.Fatalf("targets resolved as %+v", targets)
	}

	if targets[1].Where() != "targets[personal]" || targets[1].KeyName("github-app-key") != "github-app-key-personal" {
		t.Errorf("the named target renders as %q with key %q", targets[1].Where(),
			targets[1].KeyName("github-app-key"))
	}

	first, ok := cfg.TierTarget(&cfg.Tiers[0])
	if !ok || first.Name != "personal" || first.Path() != "someone/widgets" {
		t.Errorf("the first tier resolved to %+v (%v)", first, ok)
	}

	second, ok := cfg.TierTarget(&cfg.Tiers[1])
	if !ok || second.Name != DefaultTargetName {
		t.Errorf("the second tier resolved to %+v (%v)", second, ok)
	}
}

func TestATierMustNameItsTargetWhenThereAreSeveral(t *testing.T) {
	_, err := Load(writeConfig(t, twoTargetConfig(t, "")))
	if err == nil {
		t.Fatal("Load accepted a tier naming no target beside two targets")
	}

	if !strings.Contains(err.Error(), "target is required") {
		t.Errorf("unexpected error: %v", err)
	}

	_, err = Load(writeConfig(t, twoTargetConfig(t, "nowhere")))
	if err == nil {
		t.Fatal("Load accepted a tier naming a target the config does not declare")
	}

	if !strings.Contains(err.Error(), `target "nowhere" is not one this config declares`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestATierUnderOneTargetNeedsNoTargetKey(t *testing.T) {
	body := strings.Replace(validConfig, "  - label: billet-4vcpu-ubuntu-2404\n",
		"  - label: billet-4vcpu-ubuntu-2404\n    target: default\n", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load refused a tier naming the only target by name: %v", err)
	}

	unknown := strings.Replace(validConfig, "  - label: billet-4vcpu-ubuntu-2404\n",
		"  - label: billet-4vcpu-ubuntu-2404\n    target: elsewhere\n", 1)

	if _, err := Load(writeConfig(t, unknown)); err == nil {
		t.Fatal("Load accepted a tier naming an undeclared target beside one target")
	}
}

func TestTargetNamesAreLabelsAndUnique(t *testing.T) {
	for name, entry := range map[string]string{
		"no name":           "  - repository: someone/widgets\n",
		"a bad name":        "  - name: 'has space'\n    repository: someone/widgets\n",
		"the reserved name": "  - name: default\n    repository: someone/widgets\n",
		"a duplicate": "  - name: personal\n    repository: someone/widgets\n" +
			"    app_id: 777\n    installation_id: 888\n    private_key_path: /etc/billet/p.pem\n" +
			"  - name: personal\n    org: other\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := validConfig + "\ntargets:\n" + entry

			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatalf("Load accepted a targets list with %s", name)
			}
		})
	}
}

func TestATargetsEntryNeedsItsIdentity(t *testing.T) {
	body := validConfig + "\ntargets:\n  - name: personal\n    repository: someone/widgets\n"

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a target with no App")
	}

	for _, want := range []string{
		"targets[personal].app_id is required",
		"targets[personal].installation_id is required",
		"targets[personal].private_key_path is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing diagnostic %q in: %v", want, err)
		}
	}
}

func TestAStoreBackedDeploymentRefusesAKeyPathOnEveryTarget(t *testing.T) {
	const identity = "  identity:\n    backend: aws-ssm\n    aws_ssm:\n      region: us-east-1\n      prefix: /billet/prod\n"

	body := strings.Replace(twoTargetConfig(t, "personal"),
		"  max_vcpu: 120\n", "  max_vcpu: 120\n"+identity, 1)
	body = strings.Replace(body, "  private_key_path: /etc/billet/app.pem\n", "", 1)
	body = strings.Replace(body, "  - label: billet-8vcpu-ubuntu-2404\n",
		"  - label: billet-8vcpu-ubuntu-2404\n    target: default\n", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a private_key_path on a targets entry under the store backend")
	}

	if !strings.Contains(err.Error(), "targets[personal].private_key_path is written") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestARepositoryTargetIsUntrustedOnly(t *testing.T) {
	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"

	for name, extra := range map[string]string{
		"trusted": "    trust: trusted\n    runner_group: billet\n" +
			"    workflows: [acme/widgets/.github/workflows/ci.yml@refs/heads/main]\n",
		"a runner group": "    runner_group: billet\n",
		"workflows":      "    workflows: [acme/widgets/.github/workflows/ci.yml@refs/heads/main]\n",
		"interception":   "    intercept: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(repositoryConfig(t), tier, tier+extra, 1)

			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatalf("Load accepted %s under a repository target", name)
			}

			if !strings.Contains(err.Error(), `under repository target "default"`) {
				t.Errorf("the error does not name the repository target: %v", err)
			}
		})
	}

	untrusted := strings.Replace(repositoryConfig(t), tier, tier+"    trust: untrusted\n", 1)
	if _, err := Load(writeConfig(t, untrusted)); err != nil {
		t.Fatalf("Load refused an untrusted tier under a repository target: %v", err)
	}
}

func TestATrustedTierUnderAnOrganizationTargetStillLoads(t *testing.T) {
	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"

	body := strings.Replace(validConfig, tier, tier+"    trust: trusted\n    runner_group: billet\n"+
		"    workflows: [acme/api/.github/workflows/ci.yml@refs/heads/main]\n", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load refused a trusted tier under an organization target: %v", err)
	}
}

func TestACacheScopeStaysInsideItsTarget(t *testing.T) {
	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"

	scope := func(owner, repo string) string {
		return "    cache_scope:\n      owner: " + owner + "\n      repository: " + repo +
			"\n      workflow_ref: " + owner + "/" + repo + "/.github/workflows/ci.yml@refs/heads/main\n"
	}

	t.Run("another owner under an organization", func(t *testing.T) {
		body := strings.Replace(validConfig, tier, tier+scope("other", "api"), 1)

		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatal("Load accepted a cache scope owned by another organization")
		}

		if !strings.Contains(err.Error(), `cache_scope.owner "other" is not target "default"'s owner "acme"`) {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("the owner's other repository under an organization", func(t *testing.T) {
		body := strings.Replace(validConfig, tier, tier+scope("acme", "api"), 1)

		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Fatalf("Load refused a cache scope inside the organization: %v", err)
		}
	})

	t.Run("another repository under a repository", func(t *testing.T) {
		body := strings.Replace(repositoryConfig(t), tier, tier+scope("acme", "api"), 1)

		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatal("Load accepted a cache scope for another repository of the owner")
		}

		if !strings.Contains(err.Error(), `cache_scope.repository "api" is not target "default"'s repository "widgets"`) {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("the repository itself", func(t *testing.T) {
		body := strings.Replace(repositoryConfig(t), tier, tier+scope("acme", "widgets"), 1)

		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Fatalf("Load refused a cache scope inside the repository: %v", err)
		}
	})
}

func TestCheckRepositoryRefusesEveryShapeButOwnerSlashName(t *testing.T) {
	for _, bad := range []string{"", "   ", "acme", "acme/", "/widgets", "acme/widgets/extra",
		" acme/widgets", "acme/widgets ", "ac#me/widgets", "acme/wid%67ets",
		"acme/widgets?x", "acme/wid\x01gets"} {
		if err := CheckRepository(bad); err == nil {
			t.Errorf("CheckRepository accepted %q", bad)
		}
	}

	// An interior space survives both boundaries, as it does for an organization;
	// the rule is what the transport damages, and a name GitHub would never
	// register is GitHub's 404 to give.
	for _, good := range []string{"acme/widgets", "some-one/my.repo_1", "Änderung/wïdgets", "acme/wid gets"} {
		if err := CheckRepository(good); err != nil {
			t.Errorf("CheckRepository refused %q: %v", good, err)
		}
	}

	if err := CheckRepository("acme/wid?gets"); err == nil ||
		!strings.Contains(err.Error(), "repository name") {
		t.Errorf("a bad name should be blamed on the repository name, got: %v", err)
	}

	if err := CheckRepository("ac#me/widgets"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Errorf("a bad owner should be blamed on the owner, got: %v", err)
	}
}

// mirrorParse is actions/scaleset@v0.4.0's parseGitHubConfigFromURL, the
// boundary the config URL crosses, written out here because config may import
// nothing of billet's and the vendored function is unexported anyway.
func mirrorParse(in string) (owner, repository string, ok bool) {
	u, err := url.Parse(strings.Trim(in, "/"))
	if err != nil {
		return "", "", false
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch len(parts) {
	case 1:
		return parts[0], "", parts[0] != ""
	case 2:
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
}

// restRoundTrip is the other boundary: the name PathEscaped into a REST path
// and read back by url.Parse.
func restRoundTrip(segment string) bool {
	u, err := url.Parse("https://api.github.com/repos/" + url.PathEscape(segment) + "/installation")

	return err == nil && strings.TrimSuffix(strings.TrimPrefix(u.Path, "/repos/"), "/installation") == segment
}

// TestOwnerRulesAgreeWithTheBoundariesOverAllOfASCII sweeps every printable
// ASCII character through both boundaries an owner and a repository name cross,
// and holds orgUnsafe to exactly what they damage — so a character MISSING from
// the set is found, which a hand-picked table cannot do.
func TestOwnerRulesAgreeWithTheBoundariesOverAllOfASCII(t *testing.T) {
	for c := rune(0x20); c < 0x7f; c++ {
		segment := "ab" + string(c) + "cd"

		owner, _, ownerOK := mirrorParse("https://github.com/" + segment)
		ownerSurvives := ownerOK && owner == segment && restRoundTrip(segment)

		if got := CheckOrg(segment) == nil; got != ownerSurvives {
			t.Errorf("owner %q: CheckOrg accepts=%v, the boundaries carry it=%v", segment, got, ownerSurvives)
		}

		repoOwner, name, repoOK := mirrorParse("https://github.com/acme/" + segment)
		nameSurvives := repoOK && repoOwner == "acme" && name == segment && restRoundTrip(segment)

		if got := CheckRepository("acme/"+segment) == nil; got != nameSurvives {
			t.Errorf("repository name %q: CheckRepository accepts=%v, the boundaries carry it=%v",
				segment, got, nameSurvives)
		}
	}
}

func TestTierTargetPolicyErrorsIsWhatTheServerReapplies(t *testing.T) {
	repo := GitHubTarget{Name: "personal", Repository: "someone/widgets"}
	org := GitHubTarget{Name: "default", Org: "acme"}

	trusted := Tier{Label: "x", Trust: WorkloadTrusted, RunnerGroup: "g", Workflows: []string{"a/b/.github/workflows/c.yml@refs/heads/main"}}

	if errs := TierTargetPolicyErrors("tier x", trusted, repo); len(errs) != 3 {
		t.Errorf("a trusted tier under a repository target produced %d errors, want 3: %v", len(errs), errs)
	}

	if errs := TierTargetPolicyErrors("tier x", trusted, org); len(errs) != 0 {
		t.Errorf("a trusted tier under an organization target produced %v", errs)
	}

	plain := Tier{Label: "x"}

	if errs := TierTargetPolicyErrors("tier x", plain, repo); len(errs) != 0 {
		t.Errorf("an untrusted tier under a repository target produced %v", errs)
	}
}
