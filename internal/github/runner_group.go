package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

// RunnerGroupPolicyClient reads the GitHub-side state needed to protect pools.
// Its implementation is hidden so credential-bearing values cannot be copied
// out and rendered without the redaction methods below.
type RunnerGroupPolicyClient interface {
	ValidateTrustedRunnerGroup(context.Context, int, []string) error
	InspectEphemeralRunner(context.Context, string) (RunnerRecovery, error)
}

// RunnerRecovery reports whether an exact legacy ephemeral registration exists
// and is still busy. A zero value means it is absent.
type RunnerRecovery struct {
	RunnerID int64
	Present  bool
	Busy     bool
}

type runnerGroupPolicyClient struct {
	client         *http.Client
	base           string
	org            string
	appID          int64
	installationID int64
	privateKey     []byte

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewRunnerGroupPolicyClient builds a client for GitHub.com's organization API.
func NewRunnerGroupPolicyClient(org string, appID, installationID int64,
	privateKey []byte,
) RunnerGroupPolicyClient {
	return newRunnerGroupPolicyClient(http.DefaultClient, apiBase, org, appID, installationID, privateKey)
}

// NewRunnerGroupPolicyClientAt builds a client for a GitHub Enterprise API base.
func NewRunnerGroupPolicyClientAt(base, org string, appID, installationID int64,
	privateKey []byte,
) RunnerGroupPolicyClient {
	return newRunnerGroupPolicyClient(http.DefaultClient, base, org, appID, installationID, privateKey)
}

func newRunnerGroupPolicyClient(client *http.Client, base, org string, appID, installationID int64,
	privateKey []byte,
) *runnerGroupPolicyClient {
	return &runnerGroupPolicyClient{client: client, base: base, org: org, appID: appID,
		installationID: installationID, privateKey: bytes.Clone(privateKey)}
}

// ValidateTrustedRunnerGroup requires GitHub's exact workflow restriction.
func (c *runnerGroupPolicyClient) ValidateTrustedRunnerGroup(ctx context.Context, groupID int,
	wantWorkflows []string,
) error {
	if c == nil || c.org == "" || c.appID <= 0 || c.installationID <= 0 || len(c.privateKey) == 0 {
		return fmt.Errorf("github: trusted runner-group validation is not configured")
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/orgs/%s/actions/runner-groups/%d", c.base,
		url.PathEscape(c.org), groupID)
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
	}
	if err := json.Unmarshal(body, &policy); err != nil {
		return fmt.Errorf("github: decode runner-group policy: %w", err)
	}
	if !policy.RestrictedToWorkflows {
		return fmt.Errorf("github: runner group %d is not restricted to selected workflows", groupID)
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

// InspectEphemeralRunner reads an exact legacy runner's busy state. Ambiguous
// identities and non-ephemeral runners are refused.
func (c *runnerGroupPolicyClient) InspectEphemeralRunner(
	ctx context.Context, runnerName string,
) (RunnerRecovery, error) {
	if c == nil || c.org == "" || c.appID <= 0 || c.installationID <= 0 || len(c.privateKey) == 0 {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery is not configured")
	}
	if runnerName == "" {
		return RunnerRecovery{}, fmt.Errorf("github: runner recovery needs a name")
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return RunnerRecovery{}, err
	}
	query := url.Values{"name": {runnerName}, "per_page": {"100"}}
	endpoint := fmt.Sprintf("%s/orgs/%s/actions/runners?%s", c.base,
		url.PathEscape(c.org), query.Encode())
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
		if runner.ID == nil || runner.Name == nil || runner.Status == nil || runner.Busy == nil ||
			runner.Ephemeral == nil {
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
	if *runner.ID <= 0 {
		return RunnerRecovery{}, fmt.Errorf("github: runner %q has invalid id %d", runnerName, *runner.ID)
	}
	if !*runner.Ephemeral {
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
	return fmt.Sprintf("github.RunnerGroupPolicyClient{base:%q org:%q app_id:%d installation_id:%d credentials:[redacted]}",
		c.base, c.org, c.appID, c.installationID)
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
		Org            string `json:"org"`
		AppID          int64  `json:"app_id"`
		InstallationID int64  `json:"installation_id"`
		Credentials    string `json:"credentials"`
	}{c.base, c.org, c.appID, c.installationID, "[redacted]"})
}

func (c *runnerGroupPolicyClient) LogValue() slog.Value {
	if c == nil {
		return slog.StringValue("github.RunnerGroupPolicyClient<nil>")
	}
	return slog.GroupValue(
		slog.String("base", c.base), slog.String("org", c.org),
		slog.Int64("app_id", c.appID), slog.Int64("installation_id", c.installationID),
		slog.String("credentials", "[redacted]"),
	)
}
