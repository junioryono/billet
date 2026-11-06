package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// TokenSource is where a control plane's installation tokens come from: the App
// key it holds, or a broker that holds the key for it. A caller of one cannot
// tell which, and holds no key either way.
type TokenSource interface {
	// InstallationToken answers a token with at least a minute of life left. A
	// failure says a token could not be minted, never that the installation is
	// gone: ErrTokenUnavailable is could-not-tell, and only ErrNotInstalled says
	// the App is not there.
	InstallationToken(ctx context.Context) (Token, error)
}

// ErrTokenUnavailable means a token source could not answer: a broker that did
// not respond or that could not reach GitHub. Undecided reads it as could-not-tell,
// and nothing may read it as a verdict about the installation.
var ErrTokenUnavailable = errors.New("github: no installation token could be minted; the token source did not answer")

// ErrTokenRefused means a token source answered, and will not mint this token: a
// broker that does not know this deployment, or that has no such installation
// registered to it. An operator fixes it; a retry will not.
var ErrTokenRefused = errors.New("github: the token source refused to mint this installation token")

// Token is an installation access token. It is a bearer credential scoped to one
// installation, and every rendering of it is redacted.
type Token struct {
	bearer      string
	expiresAt   time.Time
	permissions map[string]string
}

const redactedToken = "[redacted installation token]"

// NewToken holds a token a source was handed.
func NewToken(bearer string, expiresAt time.Time, permissions map[string]string) Token {
	return Token{bearer: bearer, expiresAt: expiresAt, permissions: maps.Clone(permissions)}
}

// Bearer is the credential itself, for the one request it authorizes.
func (t Token) Bearer() string { return t.bearer }

// ExpiresAt is when GitHub stops accepting it.
func (t Token) ExpiresAt() time.Time { return t.expiresAt }

// Permissions is what GitHub says this token holds, or nil when it did not say.
func (t Token) Permissions() map[string]string { return maps.Clone(t.permissions) }

// String redacts the token.
func (Token) String() string { return redactedToken }

// GoString covers %#v, which does not consult String.
func (t Token) GoString() string { return t.String() }

// Format makes every verb safe, not only the ones fmt.Stringer covers.
func (t Token) Format(s fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, t.String())

	_ = verb
}

// MarshalJSON keeps the token out of anything that serializes it.
func (t Token) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(t.String())
	if err != nil {
		return nil, fmt.Errorf("github: marshal redacted token: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (t Token) LogValue() slog.Value { return slog.StringValue(t.String()) }

// tokenFresh is how much life a cached token must have left to be handed out.
const tokenFresh = time.Minute

// appKeyTokens mints installation tokens with the App's own key.
//
// A HANDLE ON ONE SHARED STATE, so its rendering methods can sit on a value
// receiver with nothing to copy: every path reaching a token through a field is
// covered, and a value printed anywhere prints the sentence below.
type appKeyTokens struct{ *appKeyState }

type appKeyState struct {
	client         *http.Client
	request        time.Duration
	base           string
	appID          int64
	installationID int64
	key            AppKey
	// scope narrows what the token may do, for a minter that serves someone else.
	// Empty asks GitHub for everything the installation holds.
	scope TokenScope
	// fresh is how much life a cached token must have left to be handed out.
	fresh time.Duration

	// slot admits one exchange at a time. A channel rather than a mutex so a
	// caller waiting behind a stalled exchange gives up when its own context does.
	slot  chan struct{}
	token Token

	// sentJWTs are the App JWTs this client has sent, newest last, so an answer
	// that echoes one back is refused like one echoing the token: a server that
	// saw a JWT at the token exchange could return it in any later answer. Guarded
	// by sentMu, since reads run beside the exchange.
	sentMu   sync.Mutex
	sentJWTs []string
}

// TokenScope narrows an installation token below what the installation holds.
type TokenScope struct {
	// Permissions are the permissions the token carries.
	Permissions map[string]string
	// Repositories are the repository names, under the installation's owner, the
	// token reaches. Empty reaches what the installation does.
	Repositories []string
}

// ScopeFor is the scope a deployment needs for one target: exactly the permissions
// billet asks of an App for it, and for a repository target that repository alone.
func ScopeFor(target Target, actions ActionsAccess) TokenScope {
	scope := TokenScope{Permissions: Permissions(target.Scope(), actions)}
	if target.Scope() == ScopeRepository {
		scope.Repositories = []string{target.Repository}
	}

	return scope
}

// NewAppKeyTokensFor is the token source a broker mints with: scoped to what one
// deployment needs, and handing out a token only with fresh left on it, so its caller
// can ride out the broker's own outage. An empty base means the real GitHub.
func NewAppKeyTokensFor(base string, appID, installationID int64, key AppKey, scope TokenScope,
	fresh time.Duration,
) TokenSource {
	if base == "" {
		base = apiBase
	}

	tokens := newAppKeyTokens(newPolicyHTTPClient(defaultPolicyBounds), defaultPolicyBounds.request,
		base, appID, installationID, key, scope)
	tokens.fresh = fresh

	return tokens
}

func newAppKeyTokens(client *http.Client, request time.Duration, base string, appID, installationID int64,
	key []byte, scope TokenScope,
) appKeyTokens {
	return appKeyTokens{&appKeyState{
		client: client, request: request, base: base, appID: appID, installationID: installationID,
		key: AppKey(bytes.Clone(key)), scope: scope, fresh: tokenFresh, slot: make(chan struct{}, 1),
	}}
}

// InstallationToken answers the token this source holds, or exchanges a JWT for a
// new one.
func (s appKeyTokens) InstallationToken(ctx context.Context) (Token, error) {
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return Token{}, fmt.Errorf("%w: waiting for another installation-token exchange: %w",
			errNoAnswer, ctx.Err())
	}
	defer func() { <-s.slot }()

	if s.token.bearer != "" && time.Until(s.token.expiresAt) > s.fresh {
		return s.token, nil
	}

	jwt, err := SignAppJWT(s.appID, s.key, time.Now())
	if err != nil {
		return Token{}, err
	}

	s.rememberJWT(jwt)

	var request []byte

	if len(s.scope.Permissions) > 0 || len(s.scope.Repositories) > 0 {
		request, err = json.Marshal(struct {
			Permissions  map[string]string `json:"permissions,omitempty"`
			Repositories []string          `json:"repositories,omitempty"`
		}{s.scope.Permissions, s.scope.Repositories})
		if err != nil {
			return Token{}, fmt.Errorf("github: encode the token's scope: %w", err)
		}
	}

	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", s.base, s.installationID)

	// THE APP'S OWN BUDGET answers a JWT, not the installation's, so its
	// headers are not the installation's account.
	status, _, body, err := exchange(ctx, s.client, s.request, http.MethodPost, endpoint, jwt, request)
	if err != nil {
		return Token{}, fmt.Errorf("github: create installation token: %w", err)
	}

	if status != http.StatusCreated {
		// TYPED, so a caller can tell GitHub refusing the App (401, 403) from
		// GitHub being unable to answer (5xx, a throttle) through Undecided.
		return Token{}, fmt.Errorf("github: create installation token: %w", apiErrorWithout(status, body, jwt))
	}

	// THE ANSWER CARRIES THE NEW TOKEN, and must not carry the JWT that asked.
	if carries(body, jwt) {
		return Token{}, errors.New("github: create installation token: the answer carried the App's JWT, " +
			"or could not be checked for it, so it is not read")
	}

	var out struct {
		Token       string            `json:"token"`
		ExpiresAt   string            `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Token{}, fmt.Errorf("github: decode installation token: %w", err)
	}

	// PARSED HERE RATHER THAN BY time.Time's decoder, whose error quotes the
	// value it could not read.
	expiresAt, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if out.Token == "" || out.ExpiresAt == "" || err != nil {
		return Token{}, errors.New("github: installation-token response was incomplete")
	}

	s.token = NewToken(out.Token, expiresAt, out.Permissions)

	return s.token, nil
}

// rememberJWT keeps a JWT this source is about to send.
func (s *appKeyState) rememberJWT(jwt string) {
	s.sentMu.Lock()
	defer s.sentMu.Unlock()

	s.sentJWTs = append(s.sentJWTs, jwt)
	if len(s.sentJWTs) > maxSentJWTs {
		s.sentJWTs = s.sentJWTs[len(s.sentJWTs)-maxSentJWTs:]
	}
}

// carriesJWT reports whether a body holds any JWT this source has sent.
func (s appKeyTokens) carriesJWT(body []byte) bool {
	s.sentMu.Lock()
	sent := slices.Clone(s.sentJWTs)
	s.sentMu.Unlock()

	return slices.ContainsFunc(sent, func(jwt string) bool { return carries(body, jwt) })
}

const redactedTokenSource = "github.TokenSource{app key, credentials:[redacted]}"

// String redacts every credential the source holds.
func (appKeyTokens) String() string { return redactedTokenSource }

// GoString covers %#v, which does not consult String.
func (s appKeyTokens) GoString() string { return s.String() }

// Format makes every verb safe.
func (s appKeyTokens) Format(f fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(f, s.String())

	_ = verb
}

// MarshalJSON keeps the source's credentials out of anything that serializes it.
func (s appKeyTokens) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(s.String())
	if err != nil {
		return nil, fmt.Errorf("github: marshal redacted token source: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (s appKeyTokens) LogValue() slog.Value { return slog.StringValue(s.String()) }
