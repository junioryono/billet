package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotInstalled means the app exists but is not installed on the target.
// Distinct from any other failure because the remedy is a browser visit, not a
// retry or a credential fix.
var ErrNotInstalled = errors.New("github: app is not installed on the target")

// errInstallationRead marks a response that died mid-read, and
// errInstallationShape marks a 200 whose body is not a GitHub installation —
// a transparent proxy or captive portal answering HTML, a truncated body.
// Typed, not string-matched, so classification cannot silently move bands when
// a message is reworded; both say nothing about the App and classify as
// unverifiable.
var (
	errInstallationRead  = errors.New("github: read installation response")
	errInstallationShape = errors.New("github: the installation response was not a GitHub answer")
)

// Installation identifies an app installation on an account.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	Permissions  map[string]string `json:"permissions"`
	RepositoryCt int               `json:"-"`
	// SuspendedAt is non-nil when the installation is suspended — GitHub's way
	// of disabling an App without uninstalling it. A suspended installation
	// answers this endpoint with a matching id and matching permissions while
	// every installation-token request fails, so verification must refuse it.
	SuspendedAt *time.Time `json:"suspended_at"`
}

// GetInstallation resolves the installation id for a target, authenticating as
// the app itself: /orgs/{org}/installation for an organization, or
// /repos/{owner}/{repo}/installation for a repository, whose installation is
// on the repository's owner whichever kind of account that is.
//
// This is the fallback for when the post-install redirect does not arrive — an
// operator who closes the tab, or an install completed on a different machine.
// Without it, onboarding would dead-end on a value the operator has no
// straightforward way to look up.
func GetInstallation(ctx context.Context, client *http.Client, appID int64, privateKeyPEM []byte, target Target) (*Installation, error) {
	return getInstallationAt(ctx, client, apiBase, appID, privateKeyPEM, target)
}

// getInstallationAt is GetInstallation with an injectable base URL, so the
// onboarding flow can be tested end to end without reaching GitHub.
func getInstallationAt(ctx context.Context, client *http.Client, base string, appID int64, privateKeyPEM []byte, target Target) (*Installation, error) {
	jwt, err := SignAppJWT(appID, privateKeyPEM, time.Now())
	if err != nil {
		return nil, err
	}

	endpoint := target.installationEndpoint(base)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github: build installation request: %w", err)
	}

	setAPIHeaders(req)
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := doWithTimeout(client, req)
	if err != nil {
		return nil, fmt.Errorf("github: get installation for %s: %w", target, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInstallationRead, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// 404 here is the ordinary "created but not installed yet" state, which
		// the caller polls through rather than treating as an error.
		return nil, ErrNotInstalled
	default:
		return nil, fmt.Errorf("github: get installation for %s: %w", target, apiError(resp.StatusCode, body))
	}

	var inst Installation
	if err := json.Unmarshal(body, &inst); err != nil {
		return nil, fmt.Errorf("%w: %w", errInstallationShape, err)
	}

	if inst.ID == 0 {
		return nil, fmt.Errorf("%w: the response carried no installation id", errInstallationShape)
	}

	return &inst, nil
}

// WaitForInstallation polls until the app is installed on the target or ctx is
// done.
//
// Used when the post-install redirect never arrives. Polling rather than waiting
// on the callback alone because the operator may finish the install in a
// different browser, or on a different machine entirely.
func WaitForInstallation(ctx context.Context, client *http.Client, appID int64, privateKeyPEM []byte, target Target, every time.Duration) (*Installation, error) {
	return waitForInstallationAt(ctx, client, apiBase, appID, privateKeyPEM, target, every)
}

func waitForInstallationAt(ctx context.Context, client *http.Client, base string, appID int64, privateKeyPEM []byte, target Target, every time.Duration) (*Installation, error) {
	// time.NewTicker panics on a non-positive interval, and this is an exported
	// entry point — a caller's zero value should be a diagnostic, not a crash.
	if every <= 0 {
		return nil, fmt.Errorf("github: poll interval must be positive, got %s", every)
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		inst, err := getInstallationAt(ctx, client, base, appID, privateKeyPEM, target)

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

// PermissionMismatches reports every way the installation's effective
// permissions differ from what billet requested for a target of the given
// scope, in BOTH directions.
//
// An operator can edit an app's permissions between creating it and installing
// it, and each direction fails differently:
//
//   - Missing or downgraded: runner registration fails later, with an error that
//     never mentions permissions.
//   - Unexpected: billet holds access it publicly claims not to have. An app
//     edited to add `contents` or `actions` is exactly the case that would make
//     "billet cannot read your code" false while onboarding reported success.
//
// Results are sorted so the diagnostic is stable across runs — Go randomizes map
// iteration, and an error message that reorders itself is one nobody can diff.
func (i *Installation) PermissionMismatches(scope Scope) []string {
	var problems []string

	permissions := permissionsFor(scope)

	for name, want := range permissions {
		got, ok := i.Permissions[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: want %s, not granted", name, want))
			continue
		}

		// EXACT, in both directions of the value too: write in place of a
		// requested read is MORE access than billet claims to hold, which
		// falsifies the credential-isolation claim exactly the way an extra
		// permission does. (It used to be accepted as "write implies read".)
		if got != want {
			problems = append(problems, fmt.Sprintf("%s: want %s, granted %s", name, want, got))
		}
	}

	for name, got := range i.Permissions {
		if _, expected := permissions[name]; !expected {
			problems = append(problems,
				fmt.Sprintf("%s: granted %s, but billet never requested it", name, got))
		}
	}

	sort.Strings(problems)

	return problems
}

// ErrAppUnverifiable wraps failures that say NOTHING about the credential —
// DNS, timeouts, GitHub's own 5xx — so `billet check` can report them as
// advisory rather than fatal. The three-valued contract: a definite verdict
// about the App is fatal either way, and "could not tell" is never collapsed
// into either.
var ErrAppUnverifiable = errors.New(
	"github: could not verify the App (network or GitHub unavailable)")

// VerifyAppAt proves the configured App LIVE: the key signs a JWT GitHub
// accepts, the App is installed on the target's owner (and not suspended), the
// installation id matches the config, and the granted permissions are exactly
// what billet requested for the target's scope — every mismatch fatal, in both
// directions, matching PermissionMismatches' own contract (an extra permission
// falsifies "billet cannot read your code" just as a missing one breaks
// registration later). base selects the API host — empty means api.github.com;
// a test fake or a GitHub Enterprise Server deployment passes its own.
func VerifyAppAt(
	ctx context.Context, client *http.Client, base string,
	appID int64, privateKeyPEM []byte, target Target, installationID int64,
) (*Installation, error) {
	if base == "" {
		base = apiBase
	}

	return verifyAppAt(ctx, client, base, appID, privateKeyPEM, target, installationID)
}

func verifyAppAt(
	ctx context.Context, client *http.Client, base string,
	appID int64, privateKeyPEM []byte, target Target, installationID int64,
) (*Installation, error) {
	inst, err := getInstallationAt(ctx, client, base, appID, privateKeyPEM, target)
	if err != nil {
		// The CALLER cancelled: not a verdict and not "GitHub unreachable" — an
		// interrupted check must surface as the interruption, or a SIGINT reads
		// as an advisory line and the command sails on.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if errors.Is(err, ErrNotInstalled) {
			if target.Scope() == ScopeRepository {
				return nil, fmt.Errorf("github: the App (id %d) is not installed on repository %q — "+
					"open the App's settings page and install it on the repository's owner with "+
					"access to that repository (or check that the target names the right "+
					"repository; GitHub answers 404 for both)", appID, target)
			}

			return nil, fmt.Errorf("github: the App (id %d) is not installed on %q — open the "+
				"App's settings page and install it on the organization (or check that "+
				"github.org names the right organization; GitHub answers 404 for both)", appID, target)
		}

		if api, ok := errors.AsType[*APIError](err); ok {
			switch {
			// A 5xx says nothing about the App; neither does a throttle — 429,
			// or the rate-limited shape of 403, which must NOT be reported as a
			// refused credential (the same lesson conversionError already
			// records: widening 4xx to fatal swallows 429, the more expensive
			// mistake).
			case api.Status >= 500, api.Status == http.StatusTooManyRequests, api.RateLimited:
				return nil, fmt.Errorf("%w: %w", ErrAppUnverifiable, err)
			// GitHub understood the request and refused the credential: the key
			// does not belong to this app_id, or the app_id names another App.
			case api.Status == http.StatusUnauthorized, api.Status == http.StatusForbidden:
				return nil, fmt.Errorf("github: GitHub refused the App credential (is "+
					"github.app_id %d the App this key belongs to?): %w", appID, err)
			default:
				return nil, err
			}
		}

		// No usable API answer. Transport failures, a response that died
		// mid-read, and a 200 that was not a GitHub answer (a transparent proxy
		// or captive portal) all say nothing about the App. Anything remaining
		// failed BEFORE any request — a key that cannot sign — and that is a
		// verdict about the configuration.
		//nolint:errcheck // the discarded value is the typed error itself, not a failure; the bool is the answer. errcheck cannot exclude a generic function.
		_, transport := errors.AsType[*url.Error](err)
		if transport || errors.Is(err, errInstallationRead) ||
			errors.Is(err, errInstallationShape) {
			return nil, fmt.Errorf("%w: %w", ErrAppUnverifiable, err)
		}

		return nil, err
	}

	if inst.SuspendedAt != nil {
		return nil, fmt.Errorf("github: the installation on %q is SUSPENDED (since %s): every "+
			"token request will fail until it is unsuspended on the owner's "+
			"installation settings page", target, inst.SuspendedAt.Format(time.RFC3339))
	}

	if inst.ID != installationID {
		return nil, fmt.Errorf("github: the App is installed on %q as installation %d, but the "+
			"config says installation_id %d — update the config to %d (or re-run "+
			"`billet github-app create`, which fills it in)", target, inst.ID, installationID, inst.ID)
	}

	if problems := inst.PermissionMismatches(target.Scope()); len(problems) > 0 {
		// THE OWNER'S KIND COMES FROM THE ANSWER. The installation names the
		// account it is on, so the review page is the right one for a user's
		// repository as well as an organization's.
		return nil, fmt.Errorf("github: the installation's permissions differ from what billet "+
			"requested; every mismatch matters — a missing one breaks runner registration "+
			"later, an extra one falsifies billet's credential-isolation claim. Review them "+
			"at %s:\n  %s",
			SettingsURL(target.Owner, OwnerType(inst.Account.Type)), strings.Join(problems, "\n  "))
	}

	return inst, nil
}
