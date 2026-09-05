package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// RunnerGroupPolicyClient reads the GitHub-side state needed to protect pools.
// Its implementation is hidden so credential-bearing values cannot be copied
// out and rendered without the redaction methods below.
type RunnerGroupPolicyClient interface {
	ValidateTrustedRunnerGroup(ctx context.Context, groupID int, wantWorkflows []string) error
	// ValidateRunnerGroupReach asks only whether a group can be assigned a job
	// at all. It is the half of the trusted validation that applies to EVERY
	// tier, including an untrusted one, which lives in the default group and was
	// asked nothing.
	ValidateRunnerGroupReach(ctx context.Context, groupID int) error
	InspectScaleSetRunner(ctx context.Context, runnerName string, runnerID int64) (RunnerRecovery, error)
	// FindRunnerGroupID resolves a name so a caller holding only the App
	// credentials can validate a group, which is what lets `billet check` reach
	// the same verdict the server does without an Actions-tenant client.
	FindRunnerGroupID(ctx context.Context, name string) (int, bool, error)
}

// RunnerRecovery reports whether an exact legacy scale-set registration exists
// and is still busy. A zero value means it is absent.
type RunnerRecovery struct {
	RunnerID int64
	Present  bool
	Busy     bool
}

type runnerGroupPolicyClient struct {
	client         *http.Client
	base           string
	target         Target
	appID          int64
	installationID int64
	privateKey     []byte

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewRunnerGroupPolicyClient builds a client for GitHub.com's REST API on one
// target.
//
// A REPOSITORY TARGET GETS A CLIENT TOO, because runner recovery
// (InspectScaleSetRunner) lists the target's runners, which a repository has;
// every runner-group question on such a client answers ErrNoRunnerGroups.
func NewRunnerGroupPolicyClient(target Target, appID, installationID int64,
	privateKey []byte,
) RunnerGroupPolicyClient {
	return newRunnerGroupPolicyClient(http.DefaultClient, apiBase, target, appID, installationID, privateKey)
}

// NewRunnerGroupPolicyClientAt builds a client for a GitHub Enterprise API base.
// AN EMPTY BASE MEANS THE REAL GITHUB, which is what every production caller
// passes and what no test ever did.
//
// `cmd/billet`'s githubAPIBase is a var whose zero value selects the default, so
// a test can point it at a fake; VerifyAppAt honours that and this did not. The
// result was a URL with no scheme or host — `Post "/app/installations/…"` — so
// the runner-group check FAILED on every real deployment and passed in every
// test, because the tests are the only callers that set a base.
//
// That check exists because a misconfigured runner group was the first failure
// two operators hit on a fresh host, and it could not have caught one.
func NewRunnerGroupPolicyClientAt(base string, target Target, appID, installationID int64,
	privateKey []byte,
) RunnerGroupPolicyClient {
	if base == "" {
		base = apiBase
	}

	return newRunnerGroupPolicyClient(http.DefaultClient, base, target, appID, installationID, privateKey)
}

func newRunnerGroupPolicyClient(client *http.Client, base string, target Target, appID, installationID int64,
	privateKey []byte,
) *runnerGroupPolicyClient {
	return &runnerGroupPolicyClient{client: client, base: base, target: target, appID: appID,
		installationID: installationID, privateKey: bytes.Clone(privateKey)}
}

// configured reports whether this client can authenticate at all.
func (c *runnerGroupPolicyClient) configured() bool {
	return c != nil && !c.target.IsZero() && c.appID > 0 && c.installationID > 0 && len(c.privateKey) > 0
}

// groupsOrRefuse is the guard every runner-group method starts with: a
// repository target has no groups to ask about.
func (c *runnerGroupPolicyClient) groupsOrRefuse() error {
	if c.target.Scope() == ScopeRepository {
		return fmt.Errorf("%w (target %s)", ErrNoRunnerGroups, c.target)
	}

	return nil
}

// ErrRunnerGroupNotFound reports that no runner group carries a given name.
var ErrRunnerGroupNotFound = errors.New("github: runner group not found")

// ValidateTrustedRunnerGroup requires GitHub's exact workflow restriction.
func (c *runnerGroupPolicyClient) ValidateTrustedRunnerGroup(ctx context.Context, groupID int,
	wantWorkflows []string,
) error {
	if !c.configured() {
		return fmt.Errorf("github: trusted runner-group validation is not configured")
	}
	if err := c.groupsOrRefuse(); err != nil {
		return err
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/%d", c.target.runnerGroupsEndpoint(c.base), groupID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("github: build runner-group policy request: %w", err)
	}
	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return fmt.Errorf("github: get runner-group policy: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github: read runner-group policy: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: get runner-group policy: %w", apiError(resp.StatusCode, body))
	}
	var policy struct {
		RestrictedToWorkflows bool     `json:"restricted_to_workflows"`
		SelectedWorkflows     []string `json:"selected_workflows"`
		Visibility            string   `json:"visibility"`
	}
	if err := json.Unmarshal(body, &policy); err != nil {
		return fmt.Errorf("github: decode runner-group policy: %w", err)
	}
	if !policy.RestrictedToWorkflows {
		return fmt.Errorf("github: runner group %d is not restricted to selected workflows", groupID)
	}

	// THE REACH RULE IS THE SAME ONE EVERY TIER NEEDS, so it is asked here
	// through the function an untrusted tier's check calls too, with the token
	// and the visibility this request already fetched rather than fetching them
	// twice.
	if err := c.checkRunnerGroupReach(ctx, token, groupID, policy.Visibility); err != nil {
		return err
	}

	want := slices.Clone(wantWorkflows)
	got := slices.Clone(policy.SelectedWorkflows)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return fmt.Errorf("github: runner group %d allows workflows %v, want exactly %v", groupID,
			got, want)
	}
	return nil
}

// ValidateRunnerGroupReach asks whether a group can be assigned a job at all,
// and nothing else about its policy.
//
// THIS IS THE HALF EVERY TIER NEEDS. An untrusted tier lives in the default
// group, which the trusted validation refuses outright and therefore never
// examines, so `billet check` asked nothing about it: the acceptance lane's
// third run sat queued for its whole window against a default group that
// granted no repository, with every surface reporting healthy.
func (c *runnerGroupPolicyClient) ValidateRunnerGroupReach(ctx context.Context, groupID int) error {
	if !c.configured() {
		return fmt.Errorf("github: runner-group validation is not configured")
	}

	if err := c.groupsOrRefuse(); err != nil {
		return err
	}

	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}

	visibility, err := c.runnerGroupVisibility(ctx, token, groupID)
	if err != nil {
		return err
	}

	return c.checkRunnerGroupReach(ctx, token, groupID, visibility)
}

// checkRunnerGroupReach is the rule itself, taking a visibility its caller has
// already read so the trusted path does not fetch the group twice.
//
// A GROUP THAT GRANTS NO REPOSITORY ROUTES NOTHING, AND SAYS SO NOWHERE.
//
// With visibility "selected" and an empty repository list, GitHub silently never
// assigns a job: the scale set registers, the listener advertises, and every
// surface reports healthy while the job queues forever with no runner group
// attached. Measured twice on a real organization — once at the cost of an hour,
// and again by the acceptance lane, which waited out its whole window against
// the DEFAULT group in exactly this state.
//
// The assertion is deliberately just "not empty" rather than matching a
// workflow's repository against the grant. Empty is unambiguous and cannot
// produce a false refusal; per-workflow matching would have to parse the
// owner/repo out of every selected_workflows entry and would refuse a working
// deployment the day that format admits a shape this does not expect.
//
// It is easy to reach by accident: a REST PATCH that sets visibility to
// "selected" without re-sending selected_repository_ids clears the list.
func (c *runnerGroupPolicyClient) checkRunnerGroupReach(
	ctx context.Context, token string, groupID int, visibility string,
) error {
	if !strings.EqualFold(visibility, "selected") {
		return nil
	}

	granted, err := c.runnerGroupRepositories(ctx, token, groupID)
	if err != nil {
		return err
	}

	if granted == 0 {
		return fmt.Errorf("github: runner group %d is visible to selected repositories and "+
			"grants none, so GitHub can never assign it a job", groupID)
	}

	return nil
}

// runnerGroupVisibility reads one group's visibility, for the callers that need
// the reach rule without the trusted policy around it.
func (c *runnerGroupPolicyClient) runnerGroupVisibility(
	ctx context.Context, token string, groupID int,
) (string, error) {
	endpoint := fmt.Sprintf("%s/%d", c.target.runnerGroupsEndpoint(c.base), groupID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("github: build runner-group request: %w", err)
	}

	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return "", fmt.Errorf("github: get runner group: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("github: read runner group: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: get runner group: %w", apiError(resp.StatusCode, body))
	}

	var group struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(body, &group); err != nil {
		return "", fmt.Errorf("github: decode runner group: %w", err)
	}

	return group.Visibility, nil
}

// InspectScaleSetRunner reads the state of a registration whose name, id and
// scale-set membership were independently established through the Actions
// service. Ambiguous identities and explicit non-ephemeral records are refused.
func (c *runnerGroupPolicyClient) InspectScaleSetRunner(
	ctx context.Context, runnerName string, runnerID int64,
) (RunnerRecovery, error) {
	if !c.configured() {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery is not configured")
	}
	if runnerName == "" {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery needs a name")
	}
	if runnerID <= 0 {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery needs a valid id")
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return RunnerRecovery{}, err
	}
	query := url.Values{"name": {runnerName}, "per_page": {"100"}}
	endpoint := c.target.runnersEndpoint(c.base) + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return RunnerRecovery{}, fmt.Errorf("github: build runner recovery request: %w", err)
	}
	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return RunnerRecovery{}, fmt.Errorf("github: list runners for recovery: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RunnerRecovery{}, fmt.Errorf("github: read runners for recovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return RunnerRecovery{}, fmt.Errorf("github: list runners for recovery: %w",
			apiError(resp.StatusCode, body))
	}
	type runnerRecord struct {
		ID        *int64  `json:"id"`
		Name      *string `json:"name"`
		Status    *string `json:"status"`
		Busy      *bool   `json:"busy"`
		Ephemeral *bool   `json:"ephemeral"`
	}
	var listed struct {
		TotalCount *int            `json:"total_count"`
		Runners    *[]runnerRecord `json:"runners"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return RunnerRecovery{}, fmt.Errorf("github: decode runners for recovery: %w", err)
	}
	if listed.TotalCount == nil || listed.Runners == nil {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery response was incomplete")
	}
	if *listed.TotalCount < 0 || *listed.TotalCount != len(*listed.Runners) {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery response was incomplete: total_count=%d runners=%d",
			*listed.TotalCount, len(*listed.Runners))
	}
	var exact []runnerRecord
	for _, runner := range *listed.Runners {
		if runner.Name == nil {
			return RunnerRecovery{}, fmt.Errorf("github: runner recovery response contained an incomplete runner")
		}
		if *runner.Name == runnerName {
			exact = append(exact, runner)
		}
	}
	if len(exact) == 0 {
		return RunnerRecovery{}, nil
	}
	if len(exact) != 1 {
		return RunnerRecovery{}, fmt.Errorf("github: found %d runners named %q; refusing ambiguous recovery",
			len(exact), runnerName)
	}
	runner := exact[0]
	if runner.ID == nil || runner.Status == nil || runner.Busy == nil {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery response contained an incomplete exact runner")
	}
	if *runner.ID != runnerID {
		return RunnerRecovery{}, fmt.Errorf("github: runner %q now has id %d, not the scale-set id %d; refusing replacement identity",
			runnerName, *runner.ID, runnerID)
	}
	if runner.Ephemeral != nil && !*runner.Ephemeral {
		return RunnerRecovery{}, fmt.Errorf("github: runner %q is not ephemeral; refusing recovery",
			runnerName)
	}
	if *runner.Status != "online" && *runner.Status != "offline" {
		return RunnerRecovery{}, fmt.Errorf("github: runner %q has unexpected status %q",
			runnerName, *runner.Status)
	}
	if *runner.Busy {
		if *runner.Status != "online" {
			return RunnerRecovery{}, fmt.Errorf("github: runner %q is busy but %s; refusing recovery",
				runnerName, *runner.Status)
		}
		return RunnerRecovery{RunnerID: *runner.ID, Present: true, Busy: true}, nil
	}
	return RunnerRecovery{RunnerID: *runner.ID, Present: true}, nil
}

func (c *runnerGroupPolicyClient) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > time.Minute {
		return c.token, nil
	}
	jwt, err := SignAppJWT(c.appID, c.privateKey, time.Now())
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.base, c.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("github: build installation-token request: %w", err)
	}
	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return "", fmt.Errorf("github: create installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github: create installation token: status %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("github: decode installation token: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", fmt.Errorf("github: installation-token response was incomplete")
	}
	c.token, c.expiresAt = out.Token, out.ExpiresAt
	return c.token, nil
}

func (c *runnerGroupPolicyClient) String() string {
	if c == nil {
		return "github.RunnerGroupPolicyClient<nil>"
	}
	return fmt.Sprintf("github.RunnerGroupPolicyClient{base:%q target:%q app_id:%d installation_id:%d credentials:[redacted]}",
		c.base, c.target.Path(), c.appID, c.installationID)
}

func (c *runnerGroupPolicyClient) GoString() string { return c.String() }

func (c *runnerGroupPolicyClient) Format(f fmt.State, _ rune) {
	if _, err := f.Write([]byte(c.String())); err != nil {
		return
	}
}

func (c *runnerGroupPolicyClient) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		Base           string `json:"base"`
		Target         string `json:"target"`
		AppID          int64  `json:"app_id"`
		InstallationID int64  `json:"installation_id"`
		Credentials    string `json:"credentials"`
	}{c.base, c.target.Path(), c.appID, c.installationID, "[redacted]"})
}

func (c *runnerGroupPolicyClient) LogValue() slog.Value {
	if c == nil {
		return slog.StringValue("github.RunnerGroupPolicyClient<nil>")
	}
	return slog.GroupValue(
		slog.String("base", c.base), slog.String("target", c.target.Path()),
		slog.Int64("app_id", c.appID), slog.Int64("installation_id", c.installationID),
		slog.String("credentials", "[redacted]"),
	)
}

// runnerGroupRepositories counts the repositories a selected-visibility runner
// group grants. The count is all the caller needs: zero is the failure, and the
// identities behind a non-zero answer are the operator's business.
func (c *runnerGroupPolicyClient) runnerGroupRepositories(
	ctx context.Context, token string, groupID int,
) (int, error) {
	endpoint := fmt.Sprintf("%s/%d/repositories", c.target.runnerGroupsEndpoint(c.base), groupID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("github: build runner-group repositories request: %w", err)
	}

	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return 0, fmt.Errorf("github: get runner-group repositories: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("github: read runner-group repositories: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github: get runner-group repositories: %w",
			apiError(resp.StatusCode, body))
	}

	var listed struct {
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return 0, fmt.Errorf("github: decode runner-group repositories: %w", err)
	}

	return listed.TotalCount, nil
}

// FindRunnerGroupID resolves a runner group's name to its id, and reports
// whether it is the default group.
//
// The policy client already speaks the organization REST API with an
// installation token, so this needs nothing an operator command does not
// already have — which is the point: `billet check` can validate a trusted
// tier's group without constructing the Actions-tenant client the server uses.
//
// Returns ErrRunnerGroupNotFound when no group carries the name, because "the
// group does not exist" and "the lookup failed" are different facts and only
// one of them is the operator's to fix.
func (c *runnerGroupPolicyClient) FindRunnerGroupID(ctx context.Context, name string) (int, bool, error) {
	if !c.configured() {
		return 0, false, fmt.Errorf("github: runner-group lookup is not configured")
	}

	if err := c.groupsOrRefuse(); err != nil {
		return 0, false, err
	}

	token, err := c.installationToken(ctx)
	if err != nil {
		return 0, false, err
	}

	endpoint := c.target.runnerGroupsEndpoint(c.base) + "?per_page=100"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, false, fmt.Errorf("github: build runner-group list request: %w", err)
	}

	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := doWithTimeout(c.client, req)
	if err != nil {
		return 0, false, fmt.Errorf("github: list runner groups: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false, fmt.Errorf("github: read runner groups: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("github: list runner groups: %w", apiError(resp.StatusCode, body))
	}

	var listed struct {
		RunnerGroups []struct {
			ID      int    `json:"id"`
			Name    string `json:"name"`
			Default bool   `json:"default"`
		} `json:"runner_groups"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return 0, false, fmt.Errorf("github: decode runner groups: %w", err)
	}

	// AN EMPTY NAME IS THE DEFAULT GROUP, which is where a tier that names none
	// lands — GitHub assigns its scale set there. Resolving it by name would
	// depend on the group being called "Default", which is only its name until
	// somebody renames it; the listing marks it, so the mark is what this reads.
	for _, g := range listed.RunnerGroups {
		if name == "" && g.Default {
			return g.ID, true, nil
		}

		if name != "" && g.Name == name {
			return g.ID, g.Default, nil
		}
	}

	if name == "" {
		return 0, false, fmt.Errorf("%w: this organization lists no default runner group, "+
			"which is where a tier naming none would land", ErrRunnerGroupNotFound)
	}

	return 0, false, fmt.Errorf("%w: %q", ErrRunnerGroupNotFound, name)
}
