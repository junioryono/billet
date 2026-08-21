package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

// RunnerGroupPolicyClient verifies the GitHub-side boundary of a trusted pool.
type RunnerGroupPolicyClient struct {
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
) *RunnerGroupPolicyClient {
	return newRunnerGroupPolicyClient(http.DefaultClient, apiBase, org, appID, installationID, privateKey)
}

// NewRunnerGroupPolicyClientAt builds a client for a GitHub Enterprise API base.
func NewRunnerGroupPolicyClientAt(base, org string, appID, installationID int64,
	privateKey []byte,
) *RunnerGroupPolicyClient {
	return newRunnerGroupPolicyClient(http.DefaultClient, base, org, appID, installationID, privateKey)
}

func newRunnerGroupPolicyClient(client *http.Client, base, org string, appID, installationID int64,
	privateKey []byte,
) *RunnerGroupPolicyClient {
	return &RunnerGroupPolicyClient{client: client, base: base, org: org, appID: appID,
		installationID: installationID, privateKey: bytes.Clone(privateKey)}
}

// ValidateTrustedRunnerGroup requires GitHub's exact workflow restriction.
func (c *RunnerGroupPolicyClient) ValidateTrustedRunnerGroup(ctx context.Context, groupID int,
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

func (c *RunnerGroupPolicyClient) installationToken(ctx context.Context) (string, error) {
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
