package config

import (
	"fmt"
	"strings"
)

// DefaultTargetName is the name of the target the `github:` block declares.
//
// A deployment that serves one organization writes `github:` and nothing else;
// that block IS a target, named so every path that resolves a tier to its
// credential has one vocabulary. A `targets:` entry may not take the name,
// because two blocks naming one target is two spellings of one value.
const DefaultTargetName = "default"

// TargetScope is what kind of GitHub owner a target is: an organization, whose
// runners live in runner groups, or a repository, whose runners belong to it
// alone.
//
// There is no user-account scope. GitHub has three runner scopes — repository,
// organization, enterprise — and a personal account as a whole cannot own
// runners; each of its repositories can, so a personal account is served one
// repository target at a time.
type TargetScope string

const (
	ScopeOrganization TargetScope = "organization"
	ScopeRepository   TargetScope = "repository"
)

// GitHubTarget is one GitHub owner the control plane serves, with the App
// credential that serves it.
//
// A VIEW over GitHubConfig rather than the block itself, so every reader sees
// the `github:` block and each `targets:` entry through one shape, named. The
// block is what the operator writes and the identity edit rewrites; this is
// what the server, the commands and the archive resolve a tier's credential
// through.
type GitHubTarget struct {
	// Name is the target's name in config: DefaultTargetName for the `github:`
	// block, or the entry's own name under `targets:`.
	Name string
	// Org is the organization login, for an organization target.
	Org string
	// Repository is owner/name, for a repository target.
	Repository string
	AppID      int64
	ClientID   string
	// InstallationID is the App's installation on the organization or on the
	// repository's owner.
	InstallationID int64
	// PrivateKeyPath is where this target's App key lives, when the deployment
	// keeps keys in files.
	PrivateKeyPath string
}

// Scope reports whether this target is an organization or a repository.
func (t GitHubTarget) Scope() TargetScope {
	if t.Repository != "" {
		return ScopeRepository
	}

	return ScopeOrganization
}

// IsRepository reports whether this target is a repository.
func (t GitHubTarget) IsRepository() bool { return t.Scope() == ScopeRepository }

// Owner is the account the target belongs to: the organization, or the
// repository's owner.
func (t GitHubTarget) Owner() string {
	if t.Repository == "" {
		return t.Org
	}

	owner, _, _ := SplitRepository(t.Repository)

	return owner
}

// RepositoryName is the repository's own name, or empty for an organization.
func (t GitHubTarget) RepositoryName() string {
	if t.Repository == "" {
		return ""
	}

	_, name, _ := SplitRepository(t.Repository)

	return name
}

// Path is the target's GitHub path: `owner` for an organization, `owner/name`
// for a repository.
//
// THE IDENTITY OF A TARGET ON THE WIRE AND IN THE LEDGER. It is what the
// scale-set client's config URL is built from, what a scale set record is keyed
// by and what an archive names, because a target's config NAME is the
// operator's label and may be renamed, while the path names the thing on
// GitHub the scale sets actually belong to.
func (t GitHubTarget) Path() string {
	if t.Repository != "" {
		return t.Repository
	}

	return t.Org
}

// Where names the block this target came from, for diagnostics.
func (t GitHubTarget) Where() string {
	if t.Name == DefaultTargetName {
		return "github"
	}

	return fmt.Sprintf("targets[%s]", t.Name)
}

// KeyName names this target's App key in a store or a per-target file: the
// bare leaf for the default target, so every deployment written before
// targets existed keeps its key where it was, and a suffixed one for the rest.
func (t GitHubTarget) KeyName(base string) string {
	if t.Name == DefaultTargetName {
		return base
	}

	return base + "-" + t.Name
}

// targetOf views one block as a target.
func targetOf(name string, g *GitHubConfig) GitHubTarget {
	return GitHubTarget{
		Name:           name,
		Org:            g.Org,
		Repository:     g.Repository,
		AppID:          g.AppID,
		ClientID:       g.ClientID,
		InstallationID: g.InstallationID,
		PrivateKeyPath: g.PrivateKeyPath,
	}
}

// GitHubTargets is every target this config serves, the `github:` block first
// as DefaultTargetName and then the `targets:` list in its written order.
//
// EVERY READER OF cfg.GitHub GOES THROUGH HERE. A reader of the block alone
// serves one target and silently ignores the rest, which for a backup is a
// credential never captured and for a check is a target never verified.
func (c *Config) GitHubTargets() []GitHubTarget {
	out := make([]GitHubTarget, 0, 1+len(c.Targets))

	if c.GitHub != nil {
		out = append(out, targetOf(DefaultTargetName, c.GitHub))
	}

	for i := range c.Targets {
		out = append(out, targetOf(c.Targets[i].Name, &c.Targets[i]))
	}

	return out
}

// GitHubTarget resolves a target by name.
func (c *Config) GitHubTarget(name string) (GitHubTarget, bool) {
	for _, t := range c.GitHubTargets() {
		if t.Name == name {
			return t, true
		}
	}

	return GitHubTarget{}, false
}

// TierTarget resolves the target a tier belongs to.
//
// An empty tier target resolves to the deployment's only target, which is what
// applyDefaults writes down; asked here as well because a Tier reaches
// TierTargetPolicyErrors from alloc.New too, on a catalogue that never went
// through Parse.
func (c *Config) TierTarget(t *Tier) (GitHubTarget, bool) {
	targets := c.GitHubTargets()

	if t.Target == "" {
		if len(targets) == 1 {
			return targets[0], true
		}

		return GitHubTarget{}, false
	}

	return c.GitHubTarget(t.Target)
}

// defaultTierTargets writes the only target's name onto every tier that names
// none, so nothing downstream has to know that "" meant "the one there is".
func (c *Config) defaultTierTargets() {
	targets := c.GitHubTargets()
	if len(targets) != 1 {
		return
	}

	for i := range c.Tiers {
		if c.Tiers[i].Target == "" {
			c.Tiers[i].Target = targets[0].Name
		}
	}
}

// validateTargets checks the `github:` block and every `targets:` entry.
//
// The server role needs at least one; every target names exactly one of an
// organization or a repository, held to the transport rule for each; names are
// label-shaped and unique; and `default` is reserved for the `github:` block
// whenever that block is written, because a `targets:` entry taking the name
// beside it would be two blocks describing one target.
func (c *Config) validateTargets() []error {
	var errs []error

	if c.GitHub == nil && len(c.Targets) == 0 {
		if c.Server != nil {
			return []error{errNoTarget}
		}

		return nil
	}

	if c.GitHub != nil {
		if c.GitHub.Name != "" {
			errs = append(errs, fmt.Errorf("github.name is not a field: the github block is the "+
				"target named %q, and only a targets entry carries a name", DefaultTargetName))
		}

		errs = append(errs, c.GitHub.validate("github")...)
	}

	seen := make(map[string]struct{}, len(c.Targets))

	for i := range c.Targets {
		t := &c.Targets[i]

		where := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			where = fmt.Sprintf("targets[%s]", t.Name)
		}

		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required; a tier names its target by it", where))
		case !labelRe.MatchString(t.Name):
			errs = append(errs, fmt.Errorf("%s: name must match %s", where, labelRe))
		case t.Name == DefaultTargetName && c.GitHub != nil:
			errs = append(errs, fmt.Errorf("%s: the github block is already the target named %q, "+
				"so this entry and that block are two spellings of one target; rename it, or "+
				"move the github block into targets under this name", where, DefaultTargetName))
		}

		if _, dup := seen[t.Name]; dup && t.Name != "" {
			errs = append(errs, fmt.Errorf("%s: duplicate target name", where))
		}

		seen[t.Name] = struct{}{}

		errs = append(errs, t.validate(where)...)
	}

	return errs
}

// validate checks one block's identity, under the field prefix it was written
// at.
func (g *GitHubConfig) validate(where string) []error {
	var errs []error

	switch {
	case g.Org != "" && g.Repository != "":
		errs = append(errs, fmt.Errorf("%s: org and repository are both written, and a target is "+
			"exactly one of them: an organization, whose runners live in runner groups, or one "+
			"repository. Serve both by declaring two targets", where))
	case g.Repository != "":
		if err := checkRepository(where+".repository", g.Repository); err != nil {
			errs = append(errs, err)
		}
	default:
		// checkOrg rather than a non-empty test: this name is concatenated into the
		// scale-set client's URL, so a value that validates here and names a
		// different organization there is the whole failure. Its "is required"
		// wording names org because that is the block every deployment before
		// repository targets wrote; the clause after it says the alternative.
		if err := checkOrg(where+".org", g.Org); err != nil {
			if strings.TrimSpace(g.Org) == "" {
				err = fmt.Errorf("%w (or %s.repository, for a repository-scoped target)", err, where)
			}

			errs = append(errs, err)
		}
	}

	if g.AppID <= 0 {
		errs = append(errs, fmt.Errorf("%s.app_id is required; run `billet github-app create`", where))
	}

	if g.InstallationID <= 0 {
		errs = append(errs, fmt.Errorf("%s.installation_id is required; creating an App does not "+
			"install it", where))
	}

	return errs
}

// validateTargetKeyPaths applies the identity-backend rule to every target:
// under the file backend each target names a key path, and under the store
// backend none may, because a path beside a store is two spellings of where
// the key lives.
func (c *Config) validateTargetKeyPaths() []error {
	var errs []error

	if c.Server == nil {
		return nil
	}

	for _, t := range c.GitHubTargets() {
		switch c.Server.IdentityBackendKind() {
		case IdentityFile:
			if t.PrivateKeyPath == "" {
				errs = append(errs, fmt.Errorf("%s.private_key_path is required", t.Where()))
			}
		case IdentitySSM:
			// TWO SPELLINGS OF ONE VALUE, WHICH THIS FILE HAS ALREADY GOT WRONG THREE
			// TIMES. With the App key in Parameter Store there is no path to read, and
			// a config carrying both would leave an operator unable to tell which one
			// the deployment is actually using — or worse, updating the one nothing
			// reads.
			if t.PrivateKeyPath != "" {
				errs = append(errs, fmt.Errorf(
					"%s.private_key_path is written and server.identity.backend is %s, which are "+
						"two spellings of where the App key lives. With this backend the key is a "+
						"SecureString under %s, so remove the path; `billet github-app create` writes "+
						"it there", t.Where(), IdentitySSM, c.Server.IdentitySSM().Prefix))
			}
		}
	}

	return errs
}

// validateTierTargets checks that every tier names a target the config
// declares, and applies the per-target policy to it.
func (c *Config) validateTierTargets() []error {
	var errs []error

	targets := c.GitHubTargets()
	if len(targets) == 0 {
		// A node-only file declares no target and its tiers belong to whichever
		// the control plane resolves; there is nothing here to check against.
		return nil
	}

	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}

	for i := range c.Tiers {
		t := &c.Tiers[i]

		where := fmt.Sprintf("tiers[%d]", i)
		if t.Label != "" {
			where = fmt.Sprintf("tier %q", t.Label)
		}

		target, ok := c.TierTarget(t)
		if !ok {
			if t.Target == "" {
				errs = append(errs, fmt.Errorf("%s: target is required, because this deployment "+
					"declares %d targets (%s) and a tier belongs to exactly one", where,
					len(targets), strings.Join(names, ", ")))
			} else {
				errs = append(errs, fmt.Errorf("%s: target %q is not one this config declares (%s)",
					where, t.Target, strings.Join(names, ", ")))
			}

			continue
		}

		errs = append(errs, TierTargetPolicyErrors(where, *t, target)...)
	}

	return errs
}

// TierTargetPolicyErrors reports what a tier may not be under its target.
//
// A REPOSITORY TARGET IS UNTRUSTED-ONLY. A trusted tier is a non-default,
// workflow-restricted runner group, and a repository has no runner groups: its
// runners are its own, GitHub offers nothing to restrict a pool with, and so
// billet has no policy to read before a mint. Trust, a group, a workflow
// allowlist and cache interception (which requires trust) are each refused by
// name. And a tier's cache scope stays inside its target's owner — and its
// repository, for a repository target — because a cache is a trust boundary of
// the deployment and site, and one target's jobs must not be handed another
// owner's bytes.
//
// Exported because the server applies it at Run as well: alloc.New re-applies
// the pool rules on a catalogue that never went through Parse, and this is the
// rule the layer holding the target's scope can enforce for the same reason.
func TierTargetPolicyErrors(where string, t Tier, target GitHubTarget) []error {
	var errs []error

	if target.IsRepository() {
		if t.Trust.Effective() == WorkloadTrusted {
			errs = append(errs, fmt.Errorf("%s: trusted under repository target %q: a trusted "+
				"pool is a workflow-restricted runner group, and a repository has no runner "+
				"groups, so GitHub cannot restrict it; repository-scoped tiers are untrusted only",
				where, target.Name))
		}

		if t.RunnerGroup != "" {
			errs = append(errs, fmt.Errorf("%s: runner_group under repository target %q: a "+
				"repository's runners are its own and have no runner groups; leave it unset",
				where, target.Name))
		}

		if len(t.Workflows) != 0 {
			errs = append(errs, fmt.Errorf("%s: workflows under repository target %q: a workflow "+
				"allowlist is a runner-group policy, and a repository has no runner groups",
				where, target.Name))
		}

		if t.Intercept {
			errs = append(errs, fmt.Errorf("%s: intercept under repository target %q: interception "+
				"requires a trusted pool, and a repository-scoped tier cannot be one",
				where, target.Name))
		}
	}

	if scope := t.CacheScope; scope != nil {
		if scope.Owner != target.Owner() {
			errs = append(errs, fmt.Errorf("%s: cache_scope.owner %q is not target %q's owner %q; "+
				"a tier's cache scope stays inside its target's owner", where, scope.Owner,
				target.Name, target.Owner()))
		}

		if target.IsRepository() && scope.Repository != target.RepositoryName() {
			errs = append(errs, fmt.Errorf("%s: cache_scope.repository %q is not target %q's "+
				"repository %q; a repository-scoped tier's cache scope stays inside that repository",
				where, scope.Repository, target.Name, target.RepositoryName()))
		}
	}

	return errs
}
