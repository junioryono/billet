package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Scope is which of GitHub's runner scopes a target is.
//
// GitHub has three — repository, organization, enterprise — and billet serves
// the first two. There is no user-account scope: a personal account as a whole
// cannot own runners, only each of its repositories can, which is why a
// personal account is served one repository target at a time.
type Scope string

const (
	ScopeOrganization Scope = "organization"
	ScopeRepository   Scope = "repository"
)

// OwnerType is what kind of account an owner is, in GitHub's own vocabulary.
//
// It decides the manifest form and the settings page: an organization's are
// under /organizations/{org}/settings, a user's under /settings. A repository
// target's owner may be either, so it is asked of GitHub before the browser
// opens rather than guessed.
type OwnerType string

const (
	OwnerUser         OwnerType = "User"
	OwnerOrganization OwnerType = "Organization"
)

// Target is one GitHub owner billet manages runners for: an organization, or
// one repository as owner and name.
//
// A VALUE, NOT A STRING, because the same target crosses two boundaries whose
// spellings differ: the scale-set client's config URL takes the path unescaped
// and the REST API takes each segment escaped. Building both from one value is
// what keeps them naming the same thing.
type Target struct {
	Owner      string
	Repository string
}

// OrganizationTarget names an organization.
func OrganizationTarget(org string) Target { return Target{Owner: org} }

// RepositoryTarget names one repository.
func RepositoryTarget(owner, name string) Target {
	return Target{Owner: owner, Repository: name}
}

// Scope reports whether this is an organization or a repository.
func (t Target) Scope() Scope {
	if t.Repository != "" {
		return ScopeRepository
	}

	return ScopeOrganization
}

// IsZero reports a target naming nothing.
func (t Target) IsZero() bool { return t.Owner == "" }

// Path is the target's GitHub path: `owner` or `owner/name`.
func (t Target) Path() string {
	if t.Repository != "" {
		return t.Owner + "/" + t.Repository
	}

	return t.Owner
}

// String renders the path, so a target in a diagnostic reads as its URL does.
func (t Target) String() string { return t.Path() }

// restPrefix is the REST API prefix every endpoint about this target sits
// under, with each segment escaped.
func (t Target) restPrefix(base string) string {
	if t.Repository != "" {
		return fmt.Sprintf("%s/repos/%s/%s", base, url.PathEscape(t.Owner), url.PathEscape(t.Repository))
	}

	return fmt.Sprintf("%s/orgs/%s", base, url.PathEscape(t.Owner))
}

// installationEndpoint is where the App's installation on this target is read:
// /orgs/{org}/installation, or /repos/{owner}/{repo}/installation.
func (t Target) installationEndpoint(base string) string {
	return t.restPrefix(base) + "/installation"
}

// runnersEndpoint lists this target's self-hosted runners.
func (t Target) runnersEndpoint(base string) string {
	return t.restPrefix(base) + "/actions/runners"
}

// runnerGroupsEndpoint lists an organization's runner groups. A repository has
// none, and every caller refuses before building it.
func (t Target) runnerGroupsEndpoint(base string) string {
	return t.restPrefix(base) + "/actions/runner-groups"
}

// ErrNoRunnerGroups is the refusal for a runner-group question asked of a
// repository target.
//
// A SENTINEL BECAUSE IT IS NOT A FAILURE OF THE REQUEST. Repository runners are
// the repository's own; GitHub offers no group to put them in, nothing to
// restrict, and no endpoint that would answer. `billet check` skips the probe
// for such a target and says so, and a trusted tier under one is refused at
// load, so reaching this at runtime means a caller lost the target's scope.
var ErrNoRunnerGroups = errors.New("github: a repository has no runner groups; runner-group " +
	"policy exists only for an organization target")

// SettingsURL is the page an operator reviews the App's installation on: an
// organization's installations page, or a user's.
func SettingsURL(owner string, ownerType OwnerType) string {
	if ownerType == OwnerUser {
		return webBase + "/settings/installations"
	}

	return fmt.Sprintf("%s/organizations/%s/settings/installations", webBase, url.PathEscape(owner))
}

// ResolveOwnerType asks GitHub whether an owner is a user or an organization.
//
// UNAUTHENTICATED AND PUBLIC: `GET /users/{owner}` answers for any account that
// exists, which is what lets this run before an App exists. It is asked because
// a repository target's owner may be either kind and the manifest form differs;
// an organization target needs no asking. A lookup that fails refuses the
// caller, because nothing has been spent yet and guessing a form GitHub then
// refuses would send the operator through the browser for nothing.
func ResolveOwnerType(ctx context.Context, client *http.Client, owner string) (OwnerType, error) {
	return resolveOwnerTypeAt(ctx, client, apiBase, owner)
}

func resolveOwnerTypeAt(ctx context.Context, client *http.Client, base, owner string) (OwnerType, error) {
	if owner == "" {
		return "", errors.New("github: owner is required to resolve its account type")
	}

	endpoint := fmt.Sprintf("%s/users/%s", base, url.PathEscape(owner))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("github: build owner lookup: %w", err)
	}

	setAPIHeaders(req)

	resp, err := doWithTimeout(client, req)
	if err != nil {
		return "", fmt.Errorf("github: look up owner %q: %w", owner, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("github: read owner %q: %w", owner, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("github: no account named %q exists on GitHub, so nothing can own "+
			"the repository this target names", owner)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: look up owner %q: %w", owner, apiError(resp.StatusCode, body))
	}

	var account struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &account); err != nil {
		return "", fmt.Errorf("github: decode owner %q: %w", owner, err)
	}

	switch OwnerType(account.Type) {
	case OwnerUser, OwnerOrganization:
		return OwnerType(account.Type), nil
	default:
		return "", fmt.Errorf("github: owner %q has account type %q, which is neither a user nor "+
			"an organization", owner, account.Type)
	}
}
