package scaleset

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/hashicorp/go-retryablehttp"

	billetgithub "github.com/junioryono/billet/internal/github"
)

// HOW A TOKEN REACHES A CLIENT THAT ONLY TAKES A FIXED ONE. The vendored client
// either signs its own JWTs with a key it holds, or carries one static token for its
// whole life, and an installation token lasts an hour. Its one reader of that token
// is the registration-token request in its hourly refresh (getRunnerRegistrationToken,
// v0.4.0), and its transport must stay an *http.Transport (common_client.go), so
// nothing can be wrapped around it.
//
// retryablehttp calls RequestLogHook with the request about to go, before every
// attempt including the first. So the client carries tokenPlaceholder, and the hook
// replaces it with what the source holds at that moment.
//
// ONLY THERE, AND A POLL NEVER ASKS. Every other request carries a token the Actions
// service issued, so a broker that is down costs nothing until a refresh needs it,
// and a deployment's long polls ride out the outage.
const (
	// tokenPlaceholder is a value GitHub could not accept, in case it were ever sent.
	tokenPlaceholder = "billet-token-source-placeholder"

	// tokenUnavailableScheme stops a request the hook has no token for. The hook
	// cannot return an error, and sending no credential would come back from GitHub
	// as a 401 that reads as a verdict about the App. An unknown scheme ends in the
	// transport, sends nothing, and is the one failure retryablehttp does not retry.
	tokenUnavailableScheme = "billet-token-unavailable"
)

// sourceInstallationToken is the hook.
func sourceInstallationToken(tokens billetgithub.TokenSource, failures *tokenFailures, log *slog.Logger,
) retryablehttp.RequestLogHook {
	return func(_ retryablehttp.Logger, req *http.Request, _ int) {
		// EVERY ATTEMPT, NOT ONLY THE FIRST: the first swaps the placeholder, and a retry
		// after a long Retry-After must not resend a token that has since expired. The
		// placeholder is checked as well as the route so that a new reader of it in the
		// vendored client is a token sent rather than a placeholder.
		if req.Header.Get("Authorization") != "Bearer "+tokenPlaceholder &&
			!strings.HasSuffix(req.URL.Path, "/actions/runners/registration-token") {
			return
		}

		token, err := tokens.InstallationToken(req.Context())
		if err != nil {
			failures.last.Store(&err)
			log.Error("no installation token could be had, so this request was not sent", "error", err)
			req.Header.Del("Authorization")
			req.URL.Scheme = tokenUnavailableScheme

			return
		}

		failures.last.Store(nil)
		req.Header.Set("Authorization", "Bearer "+token.Bearer())
	}
}

// tokenFailures remembers why a request was stopped, because the vendored client
// reports only that its transport could not send it.
//
// THE LATEST REASON, NOT THIS REQUEST'S: nothing carries one through the vendored
// client. Two callers stopped together ask one source moments apart, so the reason one
// reads may be the other's, and a stopped request stays stopped either way.
type tokenFailures struct {
	last atomic.Pointer[error]
}

// named replaces the vendored client's report of a stopped request with why it was
// stopped, so a caller reads could-not-tell or a refusal through errors.Is. Every
// other error is returned as it came.
func (f *tokenFailures) named(err error) error {
	if f == nil || err == nil || !strings.Contains(err.Error(), tokenUnavailableScheme) {
		return err
	}

	if why := f.last.Load(); why != nil {
		return fmt.Errorf("the installation token: %w", *why)
	}

	// STOPPED FOR A REASON NO LONGER HELD: a later attempt has since had a token.
	// Still could-not-tell, and never the transport's words about a scheme.
	return fmt.Errorf("the installation token: %w", errors.Join(billetgithub.ErrTokenUnavailable))
}
