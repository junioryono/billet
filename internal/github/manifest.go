// Package github talks to GitHub's App and Runner Scale Set APIs.
//
// Onboarding uses the App Manifest flow rather than asking an operator to click
// through app creation by hand. Two reasons, and the second is the important
// one: hand registration is roughly fifteen steps and a known adoption barrier
// for comparable tools, and — because the manifest declares the permission set —
// every billet deployment ends up with provably identical, minimal permissions
// instead of whatever the operator happened to tick.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiBase       = "https://api.github.com"
	webBase       = "https://github.com"
	apiVersion    = "2022-11-28"
	acceptJSON    = "application/vnd.github+json"
	requestTimout = 30 * time.Second
)

// ManifestTTL is GitHub's limit on the whole handshake: register, redirect,
// exchange. The exchange fails outright past it, so the CLI says so up front
// rather than letting an operator wander off mid-flow.
const ManifestTTL = time.Hour

// permissions is the complete set billet requests.
//
// Unexported and copied on the way out: it is a security claim, and an exported
// map is one an importing package could rewrite at runtime — changing the
// manifest, the CLI's own disclosure, and the post-install validation together.
//
// Deliberately absent: `actions: read`. It would expose workflow runs, logs and
// artifacts, and billet needs none of them — it is handed jobs by the scale-set
// API rather than discovering them. Its absence is what makes "billet cannot
// read your code" true rather than merely reassuring.
var permissions = map[string]string{
	"metadata":                         "read",
	"organization_self_hosted_runners": "write",
}

// Permissions returns a copy of the permission set billet requests.
func Permissions() map[string]string {
	out := make(map[string]string, len(permissions))
	for k, v := range permissions {
		out[k] = v
	}

	return out
}

// Manifest is the app registration billet asks GitHub to create.
// Field order follows GitHub's documented parameter table.
type Manifest struct {
	Name string `json:"name,omitempty"`
	// URL is required by GitHub even for an app that serves no web surface.
	URL            string            `json:"url"`
	HookAttributes *HookAttributes   `json:"hook_attributes,omitempty"`
	RedirectURL    string            `json:"redirect_url,omitempty"`
	SetupURL       string            `json:"setup_url,omitempty"`
	Description    string            `json:"description,omitempty"`
	Public         bool              `json:"public"`
	DefaultEvents  []string          `json:"default_events,omitempty"`
	Permissions    map[string]string `json:"default_permissions,omitempty"`
	// SetupOnUpdate keeps the installation callback firing when an operator
	// changes which repositories the app can see, so a re-scoped install still
	// lands back here.
	SetupOnUpdate bool `json:"setup_on_update,omitempty"`
}

// HookAttributes configures the app's webhook.
type HookAttributes struct {
	URL    string `json:"url,omitempty"`
	Active bool   `json:"active"`
}

// NewManifest builds billet's manifest. redirectURL and setupURL point at the
// loopback server the CLI runs for the duration of onboarding.
func NewManifest(name, redirectURL, setupURL string) Manifest {
	return Manifest{
		Name:        name,
		URL:         "https://github.com/junioryono/billet",
		Description: "Self-hosted GitHub Actions runners on your own hardware.",
		RedirectURL: redirectURL,
		SetupURL:    setupURL,
		// An app registered for one organization's runners has no reason to be
		// installable by strangers.
		Public: false,
		// billet receives work by long-polling the Runner Scale Set API, so it
		// needs no inbound webhook at all. That is what lets a deployment run with
		// no public ingress, and Active:false keeps the registration honest.
		//
		// The URL is still required: GitHub marks hook_attributes.url mandatory
		// whenever the object is present, and omitting it rejects the whole
		// registration. It is a placeholder that is never called — nothing is
		// delivered to it while Active is false.
		HookAttributes: &HookAttributes{
			URL:    webhookPlaceholderURL,
			Active: false,
		},
		Permissions: Permissions(),
		// Deliberately NOT setting SetupOnUpdate. It would redirect a future
		// repository-access change back to setup_url — a loopback port that
		// stopped existing the moment onboarding finished, so the operator would
		// land on a connection error with no explanation.
		SetupOnUpdate: false,
	}
}

// webhookPlaceholderURL satisfies GitHub's requirement that hook_attributes
// carry a URL. Nothing is ever delivered to it: the hook is registered inactive,
// and billet has no webhook receiver by design.
const webhookPlaceholderURL = "https://github.com/junioryono/billet#billet-uses-no-webhooks"

// RegistrationURL is where the browser POSTs the manifest form.
//
// It must be a form POST, not a redirect: the manifest travels in the request
// body as a `manifest` field, which is why the CLI serves a self-submitting page
// rather than simply opening a URL.
func RegistrationURL(org, state string) string {
	return fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s",
		webBase, urlPathEscape(org), urlQueryEscape(state))
}

// App is what GitHub hands back once the manifest is converted. It carries
// credentials, so it must never be logged.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	NodeID        string `json:"node_id"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`

	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// InstallURL is where the operator installs the freshly created app. Creating an
// app does NOT install it, and the installation is where the installation ID —
// which billet cannot work without — comes from.
func (a *App) InstallURL() string {
	if a.HTMLURL != "" {
		return a.HTMLURL + "/installations/new"
	}
	return fmt.Sprintf("%s/apps/%s/installations/new", webBase, a.Slug)
}

// ConvertManifest exchanges the temporary code for the app's credentials.
//
// The code is single-use and short-lived, so a failure here is terminal for that
// attempt: the operator has to re-run the flow rather than retry the exchange.
func ConvertManifest(ctx context.Context, client *http.Client, code string) (*App, error) {
	return convertManifestAt(ctx, client, apiBase, code)
}

// convertManifestAt is ConvertManifest with an injectable base URL, so the
// exchange can be tested against httptest without reaching GitHub.
func convertManifestAt(ctx context.Context, client *http.Client, base, code string) (*App, error) {
	if code == "" {
		return nil, fmt.Errorf("github: empty manifest code")
	}

	url := fmt.Sprintf("%s/app-manifests/%s/conversions", base, urlPathEscape(code))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github: build conversion request: %w", err)
	}

	setAPIHeaders(req)

	resp, err := doWithTimeout(client, req)
	if err != nil {
		return nil, fmt.Errorf("github: convert manifest: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read conversion response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github: convert manifest: %w", apiError(resp.StatusCode, body))
	}

	var app App
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, fmt.Errorf("github: decode conversion response: %w", err)
	}

	// Fail loudly on a response that parsed but is unusable — a missing key here
	// would otherwise surface much later as an inscrutable auth failure.
	if app.ID == 0 {
		return nil, fmt.Errorf("github: conversion response carried no app id")
	}

	if app.PEM == "" {
		return nil, fmt.Errorf("github: conversion response carried no private key")
	}

	return &app, nil
}

func setAPIHeaders(req *http.Request) {
	req.Header.Set("Accept", acceptJSON)
	// Go canonicalizes header keys, so this goes on the wire as GitHub documents
	// it ("X-GitHub-Api-Version") despite the Go-canonical spelling here.
	req.Header.Set("X-Github-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "billet")
}

func doWithTimeout(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = &http.Client{Timeout: requestTimout}
	}

	return client.Do(req) //nolint:wrapcheck // callers wrap with the operation name.
}

// apiError renders GitHub's error body, which usually explains the failure far
// better than the status code alone.
func apiError(status int, body []byte) error {
	var parsed struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return fmt.Errorf("HTTP %d: %s", status, parsed.Message)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 200 {
		trimmed = trimmed[:200]
	}

	return fmt.Errorf("HTTP %d: %s", status, trimmed)
}
