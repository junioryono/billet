package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNotInstalled means the app exists but is not installed on the organization.
// Distinct from any other failure because the remedy is a browser visit, not a
// retry or a credential fix.
var ErrNotInstalled = errors.New("github: app is not installed on the organization")

// Installation identifies an app installation on an account.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	Permissions  map[string]string `json:"permissions"`
	RepositoryCt int               `json:"-"`
}

// GetOrgInstallation resolves the installation id for an organization,
// authenticating as the app itself.
//
// This is the fallback for when the post-install redirect does not arrive — an
// operator who closes the tab, or an install completed on a different machine.
// Without it, onboarding would dead-end on a value the operator has no
// straightforward way to look up.
func GetOrgInstallation(ctx context.Context, client *http.Client, appID int64, privateKeyPEM []byte, org string) (*Installation, error) {
	return getOrgInstallationAt(ctx, client, apiBase, appID, privateKeyPEM, org)
}

// getOrgInstallationAt is GetOrgInstallation with an injectable base URL, so the
// onboarding flow can be tested end to end without reaching GitHub.
func getOrgInstallationAt(ctx context.Context, client *http.Client, base string, appID int64, privateKeyPEM []byte, org string) (*Installation, error) {
	jwt, err := SignAppJWT(appID, privateKeyPEM, time.Now())
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/orgs/%s/installation", base, url.PathEscape(org))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github: build installation request: %w", err)
	}

	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := doWithTimeout(client, req)
	if err != nil {
		return nil, fmt.Errorf("github: get org installation: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read installation response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// 404 here is the ordinary "created but not installed yet" state, which
		// the caller polls through rather than treating as an error.
		return nil, ErrNotInstalled
	default:
		return nil, fmt.Errorf("github: get org installation: %w", apiError(resp.StatusCode, body))
	}

	var inst Installation
	if err := json.Unmarshal(body, &inst); err != nil {
		return nil, fmt.Errorf("github: decode installation response: %w", err)
	}

	if inst.ID == 0 {
		return nil, fmt.Errorf("github: installation response carried no id")
	}

	return &inst, nil
}

// WaitForOrgInstallation polls until the app is installed or ctx is done.
//
// Used when the post-install redirect never arrives. Polling rather than waiting
// on the callback alone because the operator may finish the install in a
// different browser, or on a different machine entirely.
func WaitForOrgInstallation(ctx context.Context, client *http.Client, appID int64, privateKeyPEM []byte, org string, every time.Duration) (*Installation, error) {
	return waitForOrgInstallationAt(ctx, client, apiBase, appID, privateKeyPEM, org, every)
}

func waitForOrgInstallationAt(ctx context.Context, client *http.Client, base string, appID int64, privateKeyPEM []byte, org string, every time.Duration) (*Installation, error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		inst, err := getOrgInstallationAt(ctx, client, base, appID, privateKeyPEM, org)

		switch {
		case err == nil:
			return inst, nil
		case errors.Is(err, ErrNotInstalled):
			// Keep waiting; this is the expected state until the operator finishes.
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, err
		default:
			// Anything else is a real problem — a bad key, a revoked app, GitHub
			// down. Polling through it would hide the cause behind a timeout.
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("github: waiting for installation: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// MissingPermissions reports any permission billet requires that the
// installation does not grant.
//
// An operator can edit permissions between creating the app and installing it,
// and a scale set that cannot register runners fails at job time with an error
// that says nothing about permissions.
func (i *Installation) MissingPermissions() []string {
	var missing []string

	for name, want := range Permissions {
		got, ok := i.Permissions[name]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (want %s, not granted)", name, want))
			continue
		}

		// write implies read; read does not imply write.
		if want == "write" && got != "write" {
			missing = append(missing, fmt.Sprintf("%s (want write, granted %s)", name, got))
		}
	}

	return missing
}
